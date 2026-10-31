// Package engine runs the selection pipeline shared by the select, run and
// explain commands: configuration, change extraction, index loading and
// staleness, L1, and the controller (lock, predictor, drift and the decision).
// Every failure or unknown degrades to a full-suite Decision; an error is
// returned only for an invalid invocation or a canceled context.
package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/omorsi45/hazard/internal/change"
	"github.com/omorsi45/hazard/internal/config"
	"github.com/omorsi45/hazard/internal/control"
	"github.com/omorsi45/hazard/internal/impact"
	"github.com/omorsi45/hazard/internal/index"
	"github.com/omorsi45/hazard/internal/predict"
)

// ErrInvalidRef marks an explicit Base or Head that does not resolve to a
// commit. The CLI maps it to a usage error.
var ErrInvalidRef = errors.New("invalid ref")

// ErrInvalidConfig marks a configuration file that cannot be read or is
// invalid. The CLI maps it to a usage error.
var ErrInvalidConfig = errors.New("invalid config")

// ErrRecallOverride marks a request for a recall other than the lock's. The
// threshold of a lock is certified for the recall it was calibrated for, so
// another recall needs a new calibration. The CLI maps it to a usage error.
var ErrRecallOverride = errors.New("recall override is not supported without recalibration")

// Options is one selection request. Predictor, MaxCandidates, BudgetMS,
// BudgetUSD and Redact override the lock's predictor, the candidate cap, the
// time budget (5000 ms when zero), the spend cap and the redaction level; a cap
// or a redaction level that differs from the configuration voids a lock
// calibrated without it (policy_mismatch). Recall must be zero: a recall
// override is refused with ErrRecallOverride. Explain is accepted for the callers
// that read the scores, and changes nothing.
type Options struct {
	RepoDir, Base, Head, ConfigPath, LockPath, Predictor string // RepoDir is required and absolute
	MaxCandidates, BudgetMS                              int
	BudgetUSD                                            float64
	Redact                                               string
	Shadow, DryRunPayload, Explain                       bool
	Recall                                               float64 // 0 = from lock
	Mode                                                 impact.Mode
	ModeSet                                              bool // false: coverage mode when the index has coverage, else graph mode

	// load replaces LoadPredictor in a test; nil means LoadPredictor.
	load func(repoDir string, cfg config.Config, kind string) (predict.Predictor, control.Identity, error)
}

// Selected is one test in a selection. Package and RunnerID come from the
// index inventory and are empty for a test that is not in it.
type Selected struct {
	TestID, Source, Reason string // Source: certain|added|always_run|tier
	P                      *float64
	Package, RunnerID      string
}

// Decision is the outcome of Select.
type Decision struct {
	ID                   string
	FullSuite, Abstained bool
	AbstainReason        string
	LockDigest           string
	Lambda               float64
	Tests                []Selected // sorted by TestID; empty when FullSuite
	Hypothetical         []Selected // what narrowing would select under the active policy, even when FullSuite; nil when L1 itself demands the full suite
	AllTests             []string   // inventory of a usable index plus the Go tests that changed test files define at head; nil when no usable index was loaded
	Change               *change.ChangeSet
	L1                   *impact.Result
	Warnings             []string
	Scores               []TestScore // what the predictor said about each residual candidate, in TestID order; nil when none was asked
	EstMSSelected        int
	EstMSFull            int
	OverheadMS           int
	CostUSD              float64
	Payload              []byte // DryRunPayload only
}

const (
	reasonNoMergeBase  = "no_merge_base"
	reasonChangeError  = "change_error"
	reasonNoIndex      = "no_index"
	reasonIndexCorrupt = "index_corrupt"
	reasonIndexStale   = "index_stale"
	reasonHistory      = "history_error"
	reasonNoLock       = "no_lock"
	reasonLockError    = "lock_error"
	reasonShadow       = "shadow"

	sourceCertain   = "certain"
	sourceAlwaysRun = "always_run"

	emptyPayload   = `{"requests":[]}`
	noPredictorMsg = "no external predictor in the active policy; nothing would be sent"
)

// Select decides which tests to run for opts. A safety refusal is a valid
// full-suite Decision; the error is non-nil only for an invalid invocation
// (ErrInvalidRef, ErrInvalidConfig, a relative RepoDir) or a canceled context.
func Select(ctx context.Context, opts Options) (Decision, error) {
	start := time.Now()
	if err := ctx.Err(); err != nil {
		return Decision{}, err
	}
	if !filepath.IsAbs(opts.RepoDir) {
		return Decision{}, fmt.Errorf("engine: RepoDir %q must be an absolute path", opts.RepoDir)
	}
	if opts.Recall != 0 {
		return Decision{}, ErrRecallOverride
	}
	id, err := newID()
	if err != nil {
		return Decision{}, err
	}
	d, err := pipeline(ctx, opts, start)
	if err != nil {
		return Decision{}, err
	}
	if err := ctx.Err(); err != nil {
		return Decision{}, err
	}
	d.ID = id
	if opts.Shadow {
		applyShadow(&d)
	}
	if opts.DryRunPayload && d.Payload == nil {
		d.Payload = []byte(emptyPayload)
		d.Warnings = append(d.Warnings, noPredictorMsg)
	}
	d.OverheadMS = int(time.Since(start).Milliseconds())
	return d, nil
}

// newID returns 16 random bytes as hexadecimal.
func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("engine: generate decision ID: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// full turns d into a full-suite decision. Tests stays empty whenever the
// full suite is requested.
func (d *Decision) full(reason string, warnings ...string) {
	d.FullSuite, d.Abstained, d.AbstainReason = true, true, reason
	d.Tests = nil
	d.Warnings = append(d.Warnings, warnings...)
}

// applyShadow makes the full suite the run set while keeping Hypothetical.
// A decision that already was a full suite keeps its own reason.
func applyShadow(d *Decision) {
	if d.FullSuite {
		d.Warnings = append(d.Warnings, fmt.Sprintf("shadow: the run set is the full suite; the decision already was one (%s)", d.AbstainReason))
	} else {
		d.AbstainReason = reasonShadow
	}
	d.FullSuite = true
	d.Tests = nil
}

// pipeline runs configuration, extraction, index, L1 and the controller. It
// returns an error only for an invalid invocation or a canceled context. start
// is when Select began, which the time budget counts from.
func pipeline(ctx context.Context, opts Options, start time.Time) (d Decision, err error) {
	cfg, err := loadConfig(opts)
	if err != nil {
		return d, err
	}
	if err := checkRefs(ctx, opts); err != nil {
		return d, err
	}

	cs, err := change.Extract(ctx, opts.RepoDir, change.Options{
		Base:              opts.Base,
		Head:              opts.Head,
		DefaultBranch:     cfg.Repo.DefaultBranch,
		FullSuiteTriggers: cfg.Repo.FullSuiteTriggers,
		GeneratedGlobs:    cfg.Repo.GeneratedGlobs,
		// The local structure summary reads the token classes of the hunks. The
		// text stays in memory: internal/redact decides what leaves the
		// machine, and the decision's JSON never carries it.
		HunkText: true,
	})
	switch {
	case ctx.Err() != nil:
		return d, ctx.Err()
	case errors.Is(err, change.ErrNoMergeBase):
		d.full(reasonNoMergeBase, err.Error())
		return d, nil
	case err != nil:
		d.full(reasonChangeError, err.Error())
		return d, nil
	}
	d.Change = cs

	m, err := index.Load(opts.RepoDir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		d.full(reasonNoIndex, err.Error())
		return d, nil
	case err != nil:
		d.full(reasonIndexCorrupt, err.Error())
		return d, nil
	}

	head := newTreeHead(ctx, opts.RepoDir, opts.Head == "", cs.HeadSHA)
	defer func() {
		if cerr := head.Close(); cerr != nil {
			d.Warnings = append(d.Warnings, "head reader: "+cerr.Error())
		}
	}()
	stale, err := staleFor(m, cfg, changedPaths(cs), head)
	switch {
	case ctx.Err() != nil:
		return d, ctx.Err()
	case err != nil:
		d.full(reasonIndexStale, "index staleness could not be determined: "+err.Error())
		return d, nil
	case stale != "":
		d.full(reasonIndexStale, "index is stale: "+stale)
		return d, nil
	}
	d.AllTests = testIDs(m)

	mapPackages(cs, m)
	mi, reason, detail := withHeadTests(m, cs, head)
	if reason != "" {
		d.full(reason, detail)
		return d, nil
	}
	if mi != m {
		d.AllTests = testIDs(mi)
	}
	age := -1
	if m.Coverage != nil {
		if age, err = coverageAge(ctx, opts.RepoDir, m.Coverage.HeadSHA, cs.BaseSHA); err != nil {
			if ctx.Err() != nil {
				return d, ctx.Err()
			}
			age = -1
			d.Warnings = append(d.Warnings, "coverage age could not be determined: "+err.Error())
		}
	}
	digests, err := baseDigests(ctx, opts.RepoDir, cs.BaseSHA, cs.Files)
	if err != nil {
		if ctx.Err() != nil {
			return d, ctx.Err()
		}
		d.Warnings = append(d.Warnings, "base digests are incomplete: "+err.Error())
	}

	always, err := alwaysRun(ctx, opts.RepoDir)
	switch {
	case ctx.Err() != nil:
		return d, ctx.Err()
	case err != nil:
		d.full(reasonHistory, err.Error())
		return d, nil
	}

	ls := loadLock(opts)
	mode, modeReason, modeDetail := l1Mode(opts, m.Coverage != nil, ls)
	if modeReason != "" {
		d.full(modeReason, modeDetail)
		return d, nil
	}
	res, err := impact.L1(impact.Input{
		Change:        cs,
		Index:         mi,
		History:       impact.NoHistory{},
		Impact:        cfg.Impact,
		Mode:          mode,
		AlwaysRun:     always,
		MaxCandidates: opts.MaxCandidates,
		CoverageAge:   age,
		BaseDigests:   digests,
		ReadFile:      headReader(ctx, opts, cs),
	})
	if err != nil {
		return d, fmt.Errorf("engine: %w", err)
	}
	if ctx.Err() != nil {
		return d, ctx.Err()
	}
	d.L1 = res
	if res.FullReason != "" {
		d.full(res.FullReason)
		return d, nil
	}
	d.Hypothetical = hypothetical(res, mi, always)

	st := stage{opts: opts, cfg: effectiveConfig(cfg, opts), start: start, cs: cs, m: mi, res: res, always: always, mode: mode, lock: ls}
	if err := st.run(ctx, &d); err != nil {
		return d, err
	}
	return d, nil
}

// staleFor decides whether the index describes the repository at head for a
// change set (D055). It is a variable only so a test can make the check fail.
var staleFor = (*index.Manifest).StaleFor

// changedPaths is the Path and OldPath of every changed file: the files that
// may differ from the index.
func changedPaths(cs *change.ChangeSet) map[string]bool {
	set := make(map[string]bool, len(cs.Files))
	for _, f := range cs.Files {
		set[f.Path] = true
		if f.OldPath != "" {
			set[f.OldPath] = true
		}
	}
	return set
}

// loadConfig reads opts.ConfigPath, else RepoDir/hazard.yml, else
// RepoDir/.hazard/hazard.yml, else the defaults.
func loadConfig(opts Options) (config.Config, error) {
	path := opts.ConfigPath
	if path == "" {
		for _, rel := range []string{"hazard.yml", filepath.Join(".hazard", "hazard.yml")} {
			candidate := filepath.Join(opts.RepoDir, rel)
			_, err := os.Stat(candidate)
			if err == nil {
				path = candidate
				break
			}
			if !errors.Is(err, fs.ErrNotExist) {
				return config.Config{}, fmt.Errorf("%w: %v", ErrInvalidConfig, err)
			}
		}
	}
	if path == "" {
		return config.Defaults(), nil
	}
	cfg, err := config.Load(path)
	if err != nil {
		return config.Config{}, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	return cfg, nil
}

// mapPackages fills ChangedFile.Package from the graph, trying the old path of
// a rename when the new one is unknown.
func mapPackages(cs *change.ChangeSet, m *index.Manifest) {
	for i := range cs.Files {
		f := &cs.Files[i]
		if id, ok := m.Graph.PackageOfFile(f.Path); ok {
			f.Package = id
		} else if f.OldPath != "" {
			if id, ok := m.Graph.PackageOfFile(f.OldPath); ok {
				f.Package = id
			}
		}
	}
}

// headReader reads head content for the shared-symbol feature: from the work
// tree when Head is empty, else from the head commit.
func headReader(ctx context.Context, opts Options, cs *change.ChangeSet) func(string) ([]byte, error) {
	return func(rel string) ([]byte, error) {
		if !filepath.IsLocal(filepath.FromSlash(rel)) {
			return nil, fmt.Errorf("engine: refusing to read %q outside the repository", rel)
		}
		if opts.Head == "" {
			return os.ReadFile(filepath.Join(opts.RepoDir, filepath.FromSlash(rel)))
		}
		res, err := runGit(ctx, opts.RepoDir, nil, "show", cs.HeadSHA+":"+rel)
		if err != nil {
			return nil, err
		}
		if res.exit != 0 {
			return nil, fmt.Errorf("git show %s:%s failed with exit status %d", cs.HeadSHA, rel, res.exit)
		}
		return res.stdout, nil
	}
}

// hypothetical is the CERTAIN set as selections, enriched from the inventory.
// The reason of an always-run test is the history reason when there is one.
func hypothetical(res *impact.Result, m *index.Manifest, always map[string]string) []Selected {
	byID := make(map[string]index.Test, len(m.Tests))
	for _, t := range m.Tests {
		byID[t.ID] = t
	}
	out := make([]Selected, 0, len(res.Certain))
	for _, mem := range res.Certain {
		s := Selected{TestID: mem.TestID, Source: sourceCertain, Reason: mem.Reason}
		if mem.Reason == sourceAlwaysRun {
			s.Source = sourceAlwaysRun
			if why := always[mem.TestID]; why != "" {
				s.Reason = why
			}
		}
		if t, ok := byID[mem.TestID]; ok {
			s.Package, s.RunnerID = t.Package, t.RunnerID
		}
		out = append(out, s)
	}
	return out
}

// controllerLockPath is the lock file the controller reads: Options.LockPath,
// relative to the repository when it is not absolute, else hazard.lock in it.
func controllerLockPath(opts Options) string {
	switch {
	case opts.LockPath == "":
		return filepath.Join(opts.RepoDir, "hazard.lock")
	case filepath.IsAbs(opts.LockPath):
		return opts.LockPath
	}
	return filepath.Join(opts.RepoDir, opts.LockPath)
}
