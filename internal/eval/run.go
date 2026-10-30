package eval

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/omorsi45/hazard/internal/config"
)

// Populations a run measures (--source).
const (
	SourceMutation = "mutation"
	SourceCI       = "ci"
	SourceBoth     = "both"
)

// UsageError is a request the user has to correct: a durations file that is
// not there or not a durations file.
type UsageError struct{ Msg string }

func (e *UsageError) Error() string { return e.Msg }

// Options configures Run.
type Options struct {
	RepoDir     string // absolute git top level; only read
	DBPath      string // .hazard/hazard.db
	ConfigPath  string // handed to the engine as is; empty lets the engine find the repository's own
	Config      config.Config
	Source      string // mutation, ci or both
	Split       SplitSpec
	Predictors  []string // kinds, in row order
	Baselines   []string // all, l1-only, l1-coverage
	Recall      float64
	Bootstrap   int
	Seed        uint64
	Grid        []float64
	GridText    string
	Durations   string // path of the baseline durations
	DurationsOK bool   // the user named the path: a missing file is an error
	Log         io.Writer
}

// Result is a finished evaluation: the report and the scored pairs behind its
// reliability tables.
type Result struct {
	Report Report
	Pairs  []Pair
}

// loadDurations builds the cost model. The default file may be absent, which
// leaves the time metrics N/A; one the user named must exist.
func loadDurations(opts Options) (*CostModel, DurationsInfo, map[string]int, error) {
	d, err := ReadDurations(opts.Durations)
	switch {
	case err == nil:
	case !opts.DurationsOK && errors.Is(err, fs.ErrNotExist):
		return nil, DurationsInfo{Reason: "no file at " + opts.Durations + "; run hazard mutate or pass --durations"}, nil, nil
	default:
		return nil, DurationsInfo{}, nil, &UsageError{Msg: err.Error()}
	}
	info := DurationsInfo{Available: true, Path: d.Path, BaseSHA: d.BaseSHA, Known: len(d.MS), Ignored: d.Ignored}
	return NewCostModel(d.MS), info, d.MS, nil
}

// Run evaluates the configurations on the populations asked for. A request
// that cannot be evaluated as it stands (no database, an index of another
// commit) is an *events.RefusalError, a result and not a failure.
func Run(ctx context.Context, opts Options) (*Result, error) {
	cost, dinfo, testMS, err := loadDurations(opts)
	if err != nil {
		return nil, err
	}
	wantMutation, wantReal := opts.Source != SourceCI, opts.Source != SourceMutation

	var loaded loadedPredictors
	if wantMutation {
		loaded = loadPredictors(opts.RepoDir, opts.Config, opts.Predictors)
	} else {
		for _, kind := range opts.Predictors {
			st := PredictorState{Kind: kind, ID: predictorRow[kind]}
			loaded.States = append(loaded.States, st)
			loaded.Info = append(loaded.Info, PredictorInfo{ID: st.ID, Kind: kind, Status: "not loaded: no mutation population in this run"})
		}
	}
	plan := Plan{
		Baselines: opts.Baselines, Predictors: loaded.States, Grid: opts.Grid, Recall: opts.Recall,
		Reps: opts.Bootstrap, Seed: opts.Seed, Cost: cost, HasCoverage: true, PMode: ModeGraph,
	}

	rep := Report{Schema: SchemaVersion, Tool: "hazard eval"}
	var res Result
	if wantMutation {
		col, err := collectMutation(ctx, collectInput{
			RepoDir: opts.RepoDir, DBPath: opts.DBPath, ConfigPath: opts.ConfigPath, Config: opts.Config,
			Predictors: loaded.Usable, TestMS: testMS,
			WantGraph: slices.Contains(opts.Baselines, BaselineL1Only), WantCover: slices.Contains(opts.Baselines, BaselineL1Coverage),
			NeedTune: len(loaded.Usable) > 0, Log: opts.Log,
		})
		if err != nil {
			return nil, err
		}
		// With no replayed event nothing is known about the index: B2 is then N/A
		// like every other row, not "unavailable".
		plan.HasCoverage = col.HasCoverage || len(col.Tune)+len(col.Test) == 0
		if col.HasCoverage {
			plan.PMode = ModeCoverage
		}
		built := BuildSynthetic(plan, col.Tune, col.Test)
		rel := built.Reliability
		if rel == nil {
			rel = []Reliability{}
		}
		observed := col.Observed
		if observed == nil {
			observed = []Count{}
		}
		rep.Synthetic = &Synthetic{
			Population: SyntheticLabel, MutationBase: col.Base, PMode: plan.PMode, Observed: observed, Skipped: col.Skipped,
			Events: col.Counts, Rows: built.Rows, Reliability: rel, PairsFile: FilePairs,
		}
		res.Pairs = built.Pairs
		if dinfo.Available && col.Base != "" && dinfo.BaseSHA != "" && dinfo.BaseSHA != col.Base {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("test durations were measured at %s but the mutants were made at %s", dinfo.BaseSHA, col.Base))
		}
	}
	if wantReal {
		labels, err := ReadReal(ctx, opts.DBPath, opts.Split)
		if err != nil {
			return nil, err
		}
		real := BuildReal(labels, plan)
		rep.Real = &real
	}
	if cost != nil {
		dinfo.Imputed = cost.Imputed()
	}

	rep.Inputs = Inputs{
		Source: opts.Source, Split: opts.Split.Method, Recall: opts.Recall, Bootstrap: opts.Bootstrap, Seed: opts.Seed,
		Grid: opts.GridText, Baselines: opts.Baselines, Predictors: loaded.Info, Durations: dinfo,
	}
	if !opts.Split.At.IsZero() {
		rep.Inputs.SplitAt = opts.Split.At.Format(DateLayout)
	}
	rep.Files = Files{Report: FileReport, HTML: FileHTML}
	if rep.Synthetic != nil {
		rep.Files.Pairs, rep.Files.Reliability = FilePairs, FileReliability
	}
	res.Report = rep
	return &res, nil
}

// Write writes report.json, report.html and, when the report has a synthetic
// section, pairs.csv and reliability.csv into dir, creating it. Everything is
// rendered before the first file is written, so a rendering failure leaves
// the directory as it was.
func (r *Result) Write(dir string) error {
	files := map[string][]byte{}
	var err error
	if files[FileReport], err = r.Report.JSON(); err != nil {
		return err
	}
	if files[FileHTML], err = r.Report.HTML(); err != nil {
		return err
	}
	if s := r.Report.Synthetic; s != nil {
		var pairs, rel bytes.Buffer
		if err := WritePairs(&pairs, r.Pairs); err != nil {
			return fmt.Errorf("eval: render %s: %w", FilePairs, err)
		}
		if err := WriteReliability(&rel, s.Reliability); err != nil {
			return fmt.Errorf("eval: render %s: %w", FileReliability, err)
		}
		files[FilePairs], files[FileReliability] = pairs.Bytes(), rel.Bytes()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("eval: create the report directory: %w", err)
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			return fmt.Errorf("eval: write %s: %w", name, err)
		}
	}
	return nil
}
