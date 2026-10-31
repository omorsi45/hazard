package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/omorsi45/hazard/internal/config"
	"github.com/omorsi45/hazard/internal/control"
	"github.com/omorsi45/hazard/internal/engine"
	"github.com/omorsi45/hazard/internal/events"
	"github.com/omorsi45/hazard/internal/impact"
	"github.com/omorsi45/hazard/internal/index"
	"github.com/omorsi45/hazard/internal/labels"
	"github.com/omorsi45/hazard/internal/predict"
	"github.com/omorsi45/hazard/internal/store"
)

// Split methods of --split.
const (
	splitTemporal = "temporal"
	splitRandom   = "random"
)

// Reasons a commit is not a calibration unit. Reasons that come from the label
// build itself are named in the labels package.
const (
	reasonInTraining           = "in_training"
	reasonBeforeTrainingCutoff = "before_training_cutoff"
	reasonAtOrAfterSplitAt     = "at_or_after_split_at"
	reasonRandomHoldout        = "random_holdout"
)

// dateLayout is the layout of --split-at and of the cutoff in training.json.
const dateLayout = "2006-01-02"

// calibrateNote is printed beside every lock (seed 10.3): the bound and the
// number to believe are different numbers.
const calibrateNote = "the formal bound assumes exchangeable commits, which commit history is not; a temporal backtest is the number to believe"

const calibrateLong = `Fit the Learn-then-Test threshold lambda from the strong CI regression labels of
the current label build and write it, with the identity it is valid for, to
hazard.lock (--out).

A unit is a commit that at least one strong regression label blames. Weak
labels, mutation records and excluded (flaky) candidates never count. Units the
chosen predictor was trained on (training.json next to its artifact) are
dropped. --split temporal keeps the units whose commit time is at or after the
training cutoff and strictly before --split-at (YYYY-MM-DD, UTC). --split random
orders the remaining units by ID, shuffles them with a generator fixed by --seed
and calibrates on the first half.

The requirement comes first: at least MinCalibrationN(alpha, delta) units are
needed (299 at alpha 0.01 and delta 0.05). With fewer, calibrate reads no
predictor artifact and builds no index; it prints

  calibrate: refused: need n >= <need>, have <have>

followed by the number of units dropped for each reason, writes no lock (an
existing one is left untouched) and exits 1. A label build that is not current
is refused the same way before any unit is counted. With enough units, each is
replayed (parent to commit in a scratch clone, with an index built at the
parent), scored by the frozen predictor and judged by the controller of hazard
select, abstentions included. No field observations exist at that point, so
the drift monitor is not applied. A unit whose replay fails (the parent cannot
be checked out, or no index can be built at it) stays in the sample as a miss at
every lambda, because an unknown outcome is never a success; the lock records
how many (replay_failures) and calibrate prints the count.

The bound assumes calibration and future commits are exchangeable. Commit
history is not, so a temporal backtest is the number to believe.

Side effects: temporary clones of the repository (removed afterwards), the lock
at --out and a row in the calibrations table of .hazard/hazard.db. The database
is read from the working directory like hazard labels; only the replay needs a
git repository.

Exit codes: 0 lock written; 1 refused and no lock written (refusal is a result, not an error);
2 usage or configuration error; 70 internal error.`

// Test seams.
var (
	calibrateLoadPredictor = engine.LoadPredictor
	calibrateNow           = time.Now
)

type calibrateRequest struct {
	alpha, delta                             float64
	grid                                     []float64
	method, predictor, unit, out, configPath string
	splitAt                                  time.Time
	seed                                     uint64
	jsonMode                                 bool
	verbose                                  bool
}

func newCalibrateCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use: "calibrate", Short: "Fit the risk threshold from the strong CI labels and write hazard.lock", Long: calibrateLong,
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 0 {
				return usageError{errors.New("calibrate accepts no positional arguments")}
			}
			return nil
		},
	}
	cmd.RunE = runCalibrate
	f := cmd.Flags()
	f.Float64("alpha", 0.01, "target miss rate, in (0,1)")
	f.Float64("delta", 0.05, "error probability, in (0,1)")
	f.String("split", splitTemporal, "temporal|random")
	f.String("split-at", "", "YYYY-MM-DD UTC, exclusive upper bound of the calibration window (required for temporal)")
	f.String("predictor", "ensemble", "static|logistic|jev|ensemble")
	f.String("grid", "0.005:0.500:0.005", "threshold grid start:end:step")
	f.String("unit", "commit", "calibration unit (commit)")
	f.String("out", "hazard.lock", "lock output path")
	f.Uint64("seed", 1, "seed of the random split")
	f.String("config", "", "config path (default: hazard.yml, then .hazard/hazard.yml)")
	return cmd
}

// parseCalibrateFlags reads and validates the flags; every violation is a
// usage error, reported before anything is read.
func parseCalibrateFlags(cmd *cobra.Command) (calibrateRequest, error) {
	f := cmd.Flags()
	var req calibrateRequest
	req.alpha, _ = f.GetFloat64("alpha")
	req.delta, _ = f.GetFloat64("delta")
	req.method, _ = f.GetString("split")
	req.predictor, _ = f.GetString("predictor")
	req.unit, _ = f.GetString("unit")
	req.out, _ = f.GetString("out")
	req.seed, _ = f.GetUint64("seed")
	req.configPath, _ = f.GetString("config")
	req.jsonMode, _ = f.GetBool("json")
	verbose, _ := f.GetCount("verbose")
	req.verbose = verbose > 0
	gridText, _ := f.GetString("grid")
	splitAt, _ := f.GetString("split-at")

	switch {
	case !(req.alpha > 0 && req.alpha < 1):
		return req, usageError{fmt.Errorf("invalid --alpha %v: want a value in (0,1)", req.alpha)}
	case !(req.delta > 0 && req.delta < 1):
		return req, usageError{fmt.Errorf("invalid --delta %v: want a value in (0,1)", req.delta)}
	case !oneOf(req.method, splitTemporal, splitRandom):
		return req, usageError{fmt.Errorf("invalid --split %q: want temporal or random", req.method)}
	case !oneOf(req.predictor, "static", "logistic", "jev", "ensemble"):
		return req, usageError{fmt.Errorf("invalid --predictor %q: want static, logistic, jev or ensemble", req.predictor)}
	case req.unit != "commit":
		return req, usageError{fmt.Errorf("invalid --unit %q: only commit is supported", req.unit)}
	case req.out == "":
		return req, usageError{errors.New("--out must not be empty")}
	}
	var err error
	if req.grid, err = control.ParseGrid(gridText); err != nil {
		return req, usageError{err}
	}
	switch {
	case req.method == splitTemporal && splitAt == "":
		return req, usageError{errors.New("--split temporal requires --split-at YYYY-MM-DD")}
	case req.method == splitRandom && splitAt != "":
		return req, usageError{errors.New("--split-at only applies to --split temporal")}
	case splitAt != "":
		if req.splitAt, err = time.Parse(dateLayout, splitAt); err != nil || req.splitAt.Format(dateLayout) != splitAt {
			return req, usageError{fmt.Errorf("invalid --split-at %q: want YYYY-MM-DD", splitAt)}
		}
	}
	return req, nil
}

func runCalibrate(cmd *cobra.Command, _ []string) (err error) {
	req, err := parseCalibrateFlags(cmd)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	if err := ctx.Err(); err != nil {
		return err
	}
	out := cmd.OutOrStdout()

	// Configuration and artifacts are found from the repository root; the
	// corpus store directory need not be a repository (only the replay is).
	base, repoErr := repoRoot(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	if repoErr != nil {
		if base, err = os.Getwd(); err != nil {
			return err
		}
	}
	cfg, err := loadIndexConfig(base, req.configPath)
	if err != nil {
		return usageError{err}
	}

	report, err := readCalibrationUnits(ctx)
	if err != nil {
		return err
	}
	if report.Freshness != labels.FreshnessCurrent {
		return refuseNotCurrent(out, req.jsonMode, report.Freshness)
	}

	var training trainingRecord
	if dir := trainingDir(base, cfg, req.predictor); dir != "" && len(report.Units) > 0 {
		if training, err = readTraining(dir); err != nil {
			return err
		}
	}
	eligible, excluded := splitUnits(report.Units, splitSpec{Method: req.method, SplitAt: req.splitAt, Seed: req.seed, Training: training})
	for reason, n := range report.Unusable {
		excluded[reason] += n
	}

	need := control.MinCalibrationN(req.alpha, req.delta)
	if len(eligible) < need {
		return refuseSample(out, req.jsonMode, calibrateRefusal{NeedN: need, HaveN: len(eligible), Excluded: excluded, BuildID: report.BuildID, Cutoff: report.Cutoff})
	}

	if repoErr != nil {
		return repoErr
	}
	p, id, err := calibrateLoadPredictor(base, cfg, req.predictor)
	if err != nil {
		if errors.Is(err, engine.ErrPredictorUnavailable) {
			return usageError{fmt.Errorf("predictor %s is unavailable: %w", req.predictor, err)}
		}
		return err
	}
	id.PolicyDigest = engine.BuildPolicy(cfg, req.predictor, impact.ModeGraph).Digest()
	testMS, err := historyDurations(ctx)
	if err != nil {
		return err
	}
	configPath := ""
	if req.configPath != "" {
		if configPath, err = filepath.Abs(req.configPath); err != nil {
			return usageError{err}
		}
	}
	scratch, err := os.MkdirTemp("", "hazard-calibrate-*")
	if err != nil {
		return err
	}
	defer func() {
		if rmErr := removeScratch(scratch); rmErr != nil {
			err = errors.Join(err, rmErr)
		}
	}()
	replay := &calibrationReplay{
		repoDir: base, configPath: configPath, cfg: cfg, predictor: p, testMS: testMS,
		lockPath: filepath.Join(scratch, "absent.lock"), // never exists: the replay must not run a predictor of its own
	}
	if req.verbose && !req.jsonMode {
		replay.log = cmd.ErrOrStderr()
	}

	units := make([]control.CalUnit, 0, len(eligible))
	for i, u := range eligible {
		if err := ctx.Err(); err != nil {
			return err
		}
		replay.logf("replaying %s (%d of %d)", u.ID, i+1, len(eligible))
		cu, skip, err := replay.unit(ctx, u)
		if err != nil {
			return err
		}
		if skip != "" {
			excluded[skip]++
			continue
		}
		units = append(units, cu)
	}

	result := control.Calibrate(units, control.CalibrationParams{
		Alpha: req.alpha, Delta: req.delta, Grid: req.grid, Unit: req.unit, Split: req.method, SplitAt: splitAtText(req),
		L1Mode:   control.L1ModeGraph, // the replay index has no coverage and forces ModeGraph
		Identity: id, Now: calibrateNow(),
	})
	if result.LTT.Refused {
		refusal := calibrateRefusal{NeedN: result.LTT.NeedN, HaveN: result.LTT.HaveN, Excluded: excluded, BuildID: report.BuildID, Cutoff: report.Cutoff}
		if result.LTT.HaveN >= result.LTT.NeedN && len(result.LTT.Points) > 0 {
			first := result.LTT.Points[0]
			refusal.Detail = fmt.Sprintf("no lambda of the grid is certified: at the smallest lambda %s the empirical miss rate is %s, against alpha %s",
				control.PValueKey(first.Lambda), control.PValueKey(first.Risk), control.PValueKey(req.alpha))
		}
		return refuseSample(out, req.jsonMode, refusal)
	}

	lock := result.Lock
	outPath, err := filepath.Abs(req.out)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}
	if err := control.WriteLock(outPath, lock); err != nil {
		return err
	}
	lock.Digest = lock.ComputeDigest()
	if err := insertCalibration(ctx, lock); err != nil {
		return fmt.Errorf("the lock was written to %s but %w", outPath, err)
	}
	return writeCalibrateAccepted(out, req.jsonMode, calibrateAccepted{Out: req.out, Lock: lock, Excluded: excluded})
}

// splitAtText is the split point as recorded in the lock: empty for a random split.
func splitAtText(req calibrateRequest) string {
	if req.splitAt.IsZero() {
		return ""
	}
	return req.splitAt.Format(dateLayout)
}

// removeScratch deletes the temporary directory of the replay.
func removeScratch(dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove %s: %w", dir, err)
	}
	return nil
}

// readCalibrationUnits reads the cohort of the current label build. A database
// that does not exist is an unavailable build, and is not created.
func readCalibrationUnits(ctx context.Context) (report labels.UnitReport, err error) {
	if _, statErr := os.Stat(labelsDBPath); errors.Is(statErr, fs.ErrNotExist) {
		return labels.UnitReport{Freshness: labels.FreshnessUnavailable}, nil
	} else if statErr != nil {
		return labels.UnitReport{}, statErr
	}
	s, err := store.Open(labelsDBPath)
	if err != nil {
		return labels.UnitReport{}, err
	}
	defer func() { err = errors.Join(err, s.Close()) }()
	return labels.StrongUnits(ctx, s.DB())
}

// trainingRecord is what calibrate keeps of training.json.
type trainingRecord struct {
	HasRecord bool
	Cutoff    time.Time       // UTC midnight of the exclusive training cutoff
	EventIDs  map[string]bool // every event the training read, in any split
}

// trainingDir is the directory that holds training.json for a predictor kind:
// the directory of its artifact. The Jev predictor is not trained.
func trainingDir(base string, cfg config.Config, kind string) string {
	var artifact string
	switch kind {
	case "static":
		artifact = cfg.Predictor.Static.Calibration
	case "logistic", "ensemble":
		artifact = cfg.Predictor.Logistic.Weights
	default:
		return ""
	}
	if !filepath.IsAbs(artifact) {
		artifact = filepath.Join(base, filepath.FromSlash(artifact))
	}
	return filepath.Dir(artifact)
}

// readTraining reads training.json of dir. A missing file is no record. A file
// that cannot be trusted is an error: the units the predictor read could not
// be told apart from the others.
func readTraining(dir string) (trainingRecord, error) {
	path := filepath.Join(dir, "training.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return trainingRecord{}, nil
	}
	if err != nil {
		return trainingRecord{}, fmt.Errorf("read %s: %w", path, err)
	}
	var m predict.TrainingManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return trainingRecord{}, fmt.Errorf("%s is not a training record: %w", path, err)
	}
	if m.Schema != predict.TrainingSchema {
		return trainingRecord{}, fmt.Errorf("%s has schema %d, want %d", path, m.Schema, predict.TrainingSchema)
	}
	cutoff, err := time.Parse(dateLayout, m.Cutoff)
	if err != nil {
		return trainingRecord{}, fmt.Errorf("%s has the cutoff %q: %w", path, m.Cutoff, err)
	}
	ids := map[string]bool{}
	for _, split := range m.Splits {
		for _, id := range split.EventIDs {
			ids[id] = true
		}
	}
	return trainingRecord{HasRecord: true, Cutoff: cutoff, EventIDs: ids}, nil
}

type splitSpec struct {
	Method   string
	SplitAt  time.Time
	Seed     uint64
	Training trainingRecord
}

// splitUnits returns the units that calibrate, sorted by ID, and the number of
// units dropped for each reason. A unit the predictor was trained on is always
// dropped. A temporal split keeps the units at or after the training cutoff
// (none when there is no record) and strictly before the split point. A random
// split sorts the remaining units by ID, shuffles them with a generator fixed
// by the seed and keeps the first half, rounded down: fewer units can only
// refuse more.
func splitUnits(units []labels.Unit, spec splitSpec) (eligible []labels.Unit, excluded map[string]int) {
	excluded = map[string]int{}
	pool := make([]labels.Unit, 0, len(units))
	for _, u := range units {
		if spec.Training.EventIDs[u.ID] {
			excluded[reasonInTraining]++
			continue
		}
		pool = append(pool, u)
	}
	slices.SortFunc(pool, func(a, b labels.Unit) int { return strings.Compare(a.ID, b.ID) })

	if spec.Method == splitRandom {
		rand.New(rand.NewPCG(spec.Seed, 0)).Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })
		half := len(pool) / 2
		eligible = slices.Clone(pool[:half])
		if held := len(pool) - half; held > 0 {
			excluded[reasonRandomHoldout] = held
		}
		slices.SortFunc(eligible, func(a, b labels.Unit) int { return strings.Compare(a.ID, b.ID) })
		return eligible, excluded
	}
	for _, u := range pool {
		switch {
		case spec.Training.HasRecord && u.CommitTime.Before(spec.Training.Cutoff):
			excluded[reasonBeforeTrainingCutoff]++
		case !u.CommitTime.Before(spec.SplitAt):
			excluded[reasonAtOrAfterSplitAt]++
		default:
			eligible = append(eligible, u)
		}
	}
	return eligible, excluded
}

// historyDurations is the median duration in milliseconds of each test with a
// measurement in the history database, as the engine reads it for a selection
// (skipped runs and missing or negative durations are not measurements; the
// median of an even count is the mean of the middle pair rounded up). The
// controller imputes the tests without one.
func historyDurations(ctx context.Context) (_ map[string]int, err error) {
	s, err := store.Open(labelsDBPath)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, s.Close()) }()
	rows, err := s.DB().QueryContext(ctx, `SELECT test_id, duration_ms FROM test_results
		WHERE duration_ms IS NOT NULL AND duration_ms >= 0 AND status <> 'skip' ORDER BY test_id, duration_ms`)
	if err != nil {
		return nil, fmt.Errorf("query test durations: %w", err)
	}
	samples := map[string][]int{} // each list is ascending, from the ORDER BY
	for rows.Next() {
		var id string
		var ms int
		if err := rows.Scan(&id, &ms); err != nil {
			return nil, errors.Join(fmt.Errorf("read test durations: %w", err), rows.Close())
		}
		samples[id] = append(samples[id], ms)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("read test durations: %w", err)
	}
	out := make(map[string]int, len(samples))
	for id, ms := range samples {
		mid := len(ms) / 2
		if len(ms)%2 == 1 {
			out[id] = ms[mid]
		} else {
			out[id] = ms[mid-1] + (ms[mid]-ms[mid-1]+1)/2
		}
	}
	return out, nil
}

// calibrationReplay turns a unit into the controller's view of its commit.
type calibrationReplay struct {
	repoDir, configPath string
	cfg                 config.Config
	predictor           predict.Predictor
	testMS              map[string]int
	lockPath            string // a path that never exists
	log                 io.Writer
}

func (r *calibrationReplay) logf(format string, args ...any) {
	if r.log != nil {
		_, _ = fmt.Fprintf(r.log, "calibrate: "+format+"\n", args...)
	}
}

// unit replays parent..commit of u in a scratch clone with an index built at
// the parent, asks the frozen predictor about the residual candidates and
// gathers the input of the controller exactly as a selection would. A unit
// whose clone or index fails is returned marked Failed, not dropped: its
// outcome is unknown, and the controller counts it as a miss at every lambda.
// skip is non-empty when the engine stopped before L1, which mirrors a
// production abstention before the deterministic layer: nothing is known about
// the commit, the unit is dropped and counted, and the sample only shrinks. A
// predictor failure aborts the calibration. The replay itself never reads a lock, so
// one committed in the replayed revision cannot change the decision.
func (r *calibrationReplay) unit(ctx context.Context, u labels.Unit) (cu control.CalUnit, skip string, err error) {
	rep, err := events.NewReplay(ctx, r.repoDir, u.ParentSHA)
	if err != nil {
		if ctx.Err() != nil {
			return cu, "", ctx.Err()
		}
		r.logf("%s: %v: counted as a miss", u.ID, err)
		return failedUnit(u), "", nil
	}
	defer func() { err = errors.Join(err, rep.Close()) }()
	m, err := index.Build(ctx, rep.Dir(), r.cfg, index.Options{})
	if err != nil {
		if ctx.Err() != nil {
			return cu, "", ctx.Err()
		}
		r.logf("%s: index at the parent: %v: counted as a miss", u.ID, err)
		return failedUnit(u), "", nil
	}
	if err := index.Publish(rep.Dir(), m); err != nil {
		return cu, "", fmt.Errorf("calibrate: %s: %w", u.ID, err)
	}
	// The index has no coverage, so L1 runs in graph mode.
	d, err := rep.Select(ctx, u.SHA, engine.Options{ConfigPath: r.configPath, LockPath: r.lockPath, ModeSet: true, Mode: impact.ModeGraph})
	if err != nil {
		return cu, "", fmt.Errorf("calibrate: %s: %w", u.ID, err)
	}
	if d.L1 == nil || d.Change == nil {
		kind, _, _ := strings.Cut(d.AbstainReason, ":")
		r.logf("%s: the engine stopped before L1: %s", u.ID, d.AbstainReason)
		return cu, "replay_" + kind, nil
	}

	var cands []control.Candidate
	if d.L1.FullReason == "" && len(d.Change.TriggersFull) == 0 { // otherwise the controller abstains and nobody is asked
		if cands, _, err = engine.Score(ctx, r.predictor, d.Change, d.L1.Candidates); err != nil {
			return cu, "", fmt.Errorf("calibrate: %s: %w", u.ID, err)
		}
	}
	in := engine.ControllerInput(r.cfg, m, d.L1, cands, 0, r.testMS, control.DriftStatus{})
	in.FullReason, in.TriggersFull = d.L1.FullReason, d.Change.TriggersFull
	return control.CalUnit{ID: u.ID, Failing: u.Failing, Input: in}, "", nil
}

// failedUnit is a calibration unit whose replay failed: it keeps its ID and
// regressions and carries no input, because none was learned.
func failedUnit(u labels.Unit) control.CalUnit {
	return control.CalUnit{ID: u.ID, Failing: u.Failing, Failed: true}
}

// insertCalibration records the lock in the calibrations table.
func insertCalibration(ctx context.Context, l control.Lock) (err error) {
	grid, err := json.Marshal(l.Grid)
	if err != nil {
		return fmt.Errorf("encode the grid: %w", err)
	}
	pvalues, err := json.Marshal(l.PValues)
	if err != nil {
		return fmt.Errorf("encode the p-values: %w", err)
	}
	s, err := store.Open(labelsDBPath)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, s.Close()) }()
	if err := insertCalibrationRow(ctx, s.DB(), l, string(grid), string(pvalues)); err != nil {
		return fmt.Errorf("the calibrations row could not be inserted: %w", err)
	}
	return nil
}

func insertCalibrationRow(ctx context.Context, db *sql.DB, l control.Lock, grid, pvalues string) error {
	_, err := db.ExecContext(ctx, `INSERT INTO calibrations(id,created_at,alpha,delta,lambda,n_cal,emp_miss_rate,split,predictor,model_version,rubric_version,grid,pvalues)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		l.Digest, l.CreatedAt, l.Alpha, l.Delta, l.Lambda, l.NCal, l.EmpiricalMissRate, l.Split, l.Predictor, l.ModelVersion, l.RubricVersion, grid, pvalues)
	return err
}

// encodeJSON writes v as one line of JSON without HTML escaping, so that a
// reason such as "need n >= 299" reads as it is printed in text mode.
func encodeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// calibrateRefusal is the refusal for a sample that is too small, or whose
// risk no lambda of the grid controls.
type calibrateRefusal struct {
	NeedN, HaveN    int
	Excluded        map[string]int
	BuildID, Cutoff string // the frozen label build the units were counted in
	Detail          string
}

type refusalJSON struct {
	Refused  bool           `json:"refused"`
	Reason   string         `json:"reason"`
	NeedN    int            `json:"need_n"`
	HaveN    int            `json:"have_n"`
	Excluded map[string]int `json:"excluded"`
	BuildID  string         `json:"build_id"`
	Cutoff   string         `json:"cutoff"`
	Detail   string         `json:"detail,omitempty"`
}

// refuseSample prints the refusal and ends the command with exit 1 and no
// message of its own: a refusal is a result, not an error.
func refuseSample(w io.Writer, jsonMode bool, r calibrateRefusal) error {
	line := fmt.Sprintf("need n >= %d, have %d", r.NeedN, r.HaveN)
	if jsonMode {
		doc := refusalJSON{Refused: true, Reason: line, NeedN: r.NeedN, HaveN: r.HaveN, Excluded: r.Excluded, BuildID: r.BuildID, Cutoff: r.Cutoff, Detail: r.Detail}
		if err := encodeJSON(w, doc); err != nil {
			return err
		}
		return exitError{code: 1}
	}
	lw := &lineWriter{w: w}
	lw.printf("calibrate: refused: %s\n", line)
	lw.printf("label_build: build_id=%s cutoff=%s\n", orNone(r.BuildID), orNone(r.Cutoff))
	lw.printf("excluded_units: %s\n", formatCounts(r.Excluded))
	if r.Detail != "" {
		lw.printf("detail: %s\n", r.Detail)
	}
	if lw.err != nil {
		return lw.err
	}
	return exitError{code: 1}
}

// refuseNotCurrent refuses before any unit is counted: the label build the
// units would come from is stale or does not exist.
func refuseNotCurrent(w io.Writer, jsonMode bool, freshness string) error {
	reason := fmt.Sprintf("label build is %s: run hazard labels build before calibrating", freshness)
	if jsonMode {
		if err := encodeJSON(w, map[string]any{"refused": true, "reason": reason, "freshness": freshness}); err != nil {
			return err
		}
		return exitError{code: 1}
	}
	if _, err := fmt.Fprintf(w, "calibrate: refused: %s\n", reason); err != nil {
		return err
	}
	return exitError{code: 1}
}

type calibrateAccepted struct {
	Out      string
	Lock     control.Lock
	Excluded map[string]int
}

func writeCalibrateAccepted(w io.Writer, jsonMode bool, a calibrateAccepted) error {
	if jsonMode {
		return encodeJSON(w, struct {
			Refused  bool           `json:"refused"`
			Out      string         `json:"out"`
			Lock     control.Lock   `json:"lock"`
			Excluded map[string]int `json:"excluded"`
			Note     string         `json:"note"`
		}{Out: a.Out, Lock: a.Lock, Excluded: a.Excluded, Note: calibrateNote})
	}
	l := a.Lock
	lw := &lineWriter{w: w}
	lw.printf("calibrate: wrote %s\n", a.Out)
	lw.printf("lambda: %s\n", control.PValueKey(l.Lambda))
	lw.printf("n_cal: %d\n", l.NCal)
	lw.printf("replay_failures: %d\n", l.ReplayFailures)
	lw.printf("empirical_miss_rate: %s\n", control.PValueKey(l.EmpiricalMissRate))
	lw.printf("alpha: %s delta: %s unit: %s\n", control.PValueKey(l.Alpha), control.PValueKey(l.Delta), l.Unit)
	lw.printf("split: %s split_at: %s\n", l.Split, orNone(l.SplitAt))
	lw.printf("predictor: %s model_version: %s rubric_version: %s feature_version: %s\n", l.Predictor, l.ModelVersion, l.RubricVersion, l.FeatureVersion)
	lw.printf("excluded_units: %s\n", formatCounts(a.Excluded))
	lw.printf("note: %s\n", calibrateNote)
	return lw.err
}
