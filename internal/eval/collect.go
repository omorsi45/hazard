package eval

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/omorsi45/hazard/internal/config"
	"github.com/omorsi45/hazard/internal/control"
	"github.com/omorsi45/hazard/internal/engine"
	"github.com/omorsi45/hazard/internal/events"
	"github.com/omorsi45/hazard/internal/impact"
	"github.com/omorsi45/hazard/internal/index"
	"github.com/omorsi45/hazard/internal/mutate"
	"github.com/omorsi45/hazard/internal/predict"
	"github.com/omorsi45/hazard/internal/store"
)

// Change kinds of a mutation event (seed 12.2): a mutant of a Go file or of a
// configuration file.
const (
	KindGo  = "go"
	KindCfg = "cfg"
)

// kindOf is the change kind of the file a mutant changes, by the rule
// events.MutationEvents uses to pick its operator.
func kindOf(file string) string {
	if strings.EqualFold(path.Ext(file), ".go") {
		return KindGo
	}
	return KindCfg
}

// Count is a number of observed mutants of one partition and status.
type Count struct {
	Partition string `json:"partition"`
	Status    string `json:"status"`
	N         int    `json:"n"`
}

// EventCounts are the replayed events of each evaluation partition.
type EventCounts struct {
	Tune             int            `json:"tune"`
	TuneWithFailures int            `json:"tune_with_failures"`
	Test             int            `json:"test"`
	TestWithFailures int            `json:"test_with_failures"`
	TestByKind       map[string]int `json:"test_by_kind"`
	TrainNotRead     int            `json:"train_not_read"`
	TuneNotReplayed  int            `json:"tune_not_replayed,omitempty"`
}

// collectInput is what the replay of the mutation events needs.
type collectInput struct {
	RepoDir    string
	DBPath     string
	ConfigPath string // handed to the engine as is
	Config     config.Config
	Predictors map[string]predict.Predictor // the usable ones, by kind
	TestMS     map[string]int               // durations for the controller's cost estimate
	WantGraph  bool                         // B1 is asked for
	WantCover  bool                         // B2 is asked for
	NeedTune   bool                         // some predictor row needs lambda
	Log        io.Writer
}

func (in collectInput) logf(format string, args ...any) {
	if in.Log != nil {
		_, _ = fmt.Fprintf(in.Log, "eval: "+format+"\n", args...)
	}
}

// collected is the replayed mutation population.
type collected struct {
	Tune, Test  []*Event
	Observed    []Count
	Skipped     map[string]int
	Counts      EventCounts
	Base        string
	HasCoverage bool
}

// readMutationRecords converts the mutants table and its kills to records,
// ordered by ID, the way the corpus collector of hazard train reads them. A
// missing database is a refusal and is not created.
func readMutationRecords(ctx context.Context, dbPath string) (_ []mutate.Record, err error) {
	if _, statErr := os.Stat(dbPath); errors.Is(statErr, fs.ErrNotExist) {
		return nil, &events.RefusalError{Reason: fmt.Sprintf("there is no database at %s: run hazard mutate first", dbPath)}
	} else if statErr != nil {
		return nil, fmt.Errorf("eval: %w", statErr)
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("eval: %w", err)
	}
	// Every handle must be closed (which checkpoints the write-ahead log)
	// before the first replay copies the database file.
	defer func() { err = errors.Join(err, st.Close()) }()
	return readRecords(ctx, st.DB())
}

func readRecords(ctx context.Context, db *sql.DB) ([]mutate.Record, error) {
	killers := map[string][]string{}
	kills, err := db.QueryContext(ctx, `SELECT mutant_id, test_id FROM mutant_kills ORDER BY mutant_id, test_id`)
	if err != nil {
		return nil, fmt.Errorf("eval: read mutant kills: %w", err)
	}
	for kills.Next() {
		var id, test string
		if err := kills.Scan(&id, &test); err != nil {
			return nil, errors.Join(fmt.Errorf("eval: read mutant kills: %w", err), kills.Close())
		}
		killers[id] = append(killers[id], test)
	}
	if err := errors.Join(kills.Err(), kills.Close()); err != nil {
		return nil, fmt.Errorf("eval: read mutant kills: %w", err)
	}
	rows, err := db.QueryContext(ctx, `SELECT id, file, line, operator, original, mutated, base_sha, status FROM mutants ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("eval: read mutants: %w", err)
	}
	var recs []mutate.Record
	for rows.Next() {
		var r mutate.Record
		if err := rows.Scan(&r.ID, &r.File, &r.Line, &r.Operator, &r.Original, &r.Mutated, &r.BaseSHA, &r.Status); err != nil {
			return nil, errors.Join(fmt.Errorf("eval: read mutants: %w", err), rows.Close())
		}
		if r.Status == "killed" {
			r.Killers = killers[r.ID]
		}
		recs = append(recs, r)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("eval: read mutants: %w", err)
	}
	return recs, nil
}

// layerBuilder turns an engine decision into a Layer.
type layerBuilder struct {
	cfg         config.Config
	testMS      map[string]int
	testPackage map[string]string
}

// layer is the Layer of a decision whose controller never ran (the lock is
// absent by design), with the inventory it saw. When the engine stopped before
// L1 the layer is stopped and the event runs the full suite: unknown is never
// a narrower selection.
func (b layerBuilder) layer(d engine.Decision, failing map[string]bool) (*Layer, []string) {
	l := &Layer{OverheadMS: float64(d.OverheadMS)}
	if d.L1 == nil || d.Change == nil {
		l.Stopped = d.AbstainReason
		if l.Stopped == "" {
			l.Stopped = "no_l1"
		}
		return l, d.AllTests
	}
	l.FullReason = d.L1.FullReason
	listed := make(map[string]bool, len(d.L1.Certain)+len(d.L1.Candidates))
	for _, m := range d.L1.Certain {
		l.Certain = append(l.Certain, m.TestID)
		listed[m.TestID] = true
	}
	for _, c := range d.L1.Candidates {
		listed[c.TestID] = true
	}
	// A failing test that the index knows and L1 neither selected nor listed was
	// cut by the candidate cap: L1 ranks every inventory test it did not select.
	inventory := make(map[string]bool, len(d.L1.AllTests))
	for _, id := range d.L1.AllTests {
		inventory[id] = true
	}
	for id := range failing {
		if inventory[id] && !listed[id] {
			l.TruncMissed++
		}
	}
	in := engine.ControllerInput(b.cfg, nil, d.L1, nil, 0, b.testMS, control.DriftStatus{})
	in.TestPackage = b.testPackage
	in.FullReason, in.TriggersFull = d.L1.FullReason, d.Change.TriggersFull
	l.Input = in
	return l, d.L1.AllTests
}

// collectMutation reads the mutants of the evaluation partitions and replays
// each as an event: one scratch clone, one commit per mutant, one engine run
// per L1 mode asked for and one scoring per usable predictor. The train
// partition is counted and never read.
func collectMutation(ctx context.Context, in collectInput) (*collected, error) {
	recs, err := readMutationRecords(ctx, in.DBPath)
	if err != nil {
		return nil, err
	}
	c := &collected{Skipped: map[string]int{}, Counts: EventCounts{TestByKind: map[string]int{}}}
	counts := map[[2]string]int{}
	var replay []mutate.Record
	for _, r := range recs {
		part := events.Partition(events.MutationGroup(r.File))
		counts[[2]string{part, r.Status}]++
		switch {
		case part == events.PartitionTrain:
			c.Counts.TrainNotRead++
		case part == events.PartitionTune && !in.NeedTune:
			c.Counts.TuneNotReplayed++
		default:
			replay = append(replay, r)
		}
	}
	for key, n := range counts {
		c.Observed = append(c.Observed, Count{Partition: key[0], Status: key[1], N: n})
	}
	sort.Slice(c.Observed, func(i, j int) bool {
		a, b := c.Observed[i], c.Observed[j]
		return a.Partition+"\x00"+a.Status < b.Partition+"\x00"+b.Status
	})

	evs, contents, skipped, err := events.MutationEvents(ctx, in.RepoDir, replay)
	if err != nil {
		return nil, err
	}
	maps.Copy(c.Skipped, skipped)
	if len(evs) == 0 {
		return c, nil
	}

	m, err := index.Load(in.RepoDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, &events.RefusalError{Reason: fmt.Sprintf("there is no index to replay the mutants on: run hazard index with the mutation base %s checked out", evs[0].BaseSHA)}
	}
	if err != nil {
		return nil, fmt.Errorf("eval: load the index: %w", err)
	}
	for _, ev := range evs {
		if ev.BaseSHA != m.HeadSHA {
			return nil, &events.RefusalError{Reason: fmt.Sprintf("the index describes %s but mutant %s was made at %s: run hazard index with %s checked out, then evaluate again",
				m.HeadSHA, ev.ID, ev.BaseSHA, ev.BaseSHA)}
		}
	}
	c.Base = m.HeadSHA
	c.HasCoverage = m.Coverage != nil
	if err := c.replay(ctx, in, m, evs, contents); err != nil {
		return nil, err
	}
	return c, nil
}

// replay replays the events in one scratch clone of the mutation base.
func (c *collected) replay(ctx context.Context, in collectInput, m *index.Manifest, evs []events.Event, contents map[string][]byte) (err error) {
	rep, err := events.NewReplay(ctx, in.RepoDir, m.HeadSHA)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, rep.Close()) }()

	// A lock committed in the replayed repository must never decide for a
	// configuration: the decisions come from the scoring and the controller
	// here. The path is in a directory of its own and cannot exist.
	lockDir, err := os.MkdirTemp("", "hazard-eval-lock-*")
	if err != nil {
		return fmt.Errorf("eval: %w", err)
	}
	defer func() { err = errors.Join(err, os.RemoveAll(lockDir)) }()
	lockPath := filepath.Join(lockDir, "absent.lock")

	testPackage := make(map[string]string, len(m.Tests))
	inventory := make([]string, len(m.Tests))
	for i, t := range m.Tests {
		testPackage[t.ID] = t.Package
		inventory[i] = t.ID
	}
	builder := layerBuilder{cfg: in.Config, testMS: in.TestMS, testPackage: testPackage}
	pMode := impact.ModeGraph
	if c.HasCoverage {
		pMode = impact.ModeCoverage
	}
	wantP := len(in.Predictors) > 0
	runGraph := in.WantGraph || (wantP && pMode == impact.ModeGraph)
	runCoverage := c.HasCoverage && (in.WantCover || wantP)

	var lastAll []string
	intern := func(all []string) []string {
		if lastAll != nil && slices.Equal(all, lastAll) {
			return lastAll
		}
		lastAll = all
		return all
	}
	in.logf("replaying %d mutation events on %s", len(evs), m.HeadSHA)
	for i, ev := range evs {
		if err := ctx.Err(); err != nil {
			return err
		}
		head, err := rep.Commit(ctx, ev.File, contents[ev.ID])
		if err != nil {
			return fmt.Errorf("eval: mutant %s: %w", ev.ID, err)
		}
		e := NewEvent(ev.ID, kindOf(ev.File), events.Partition(ev.Group), ev.Failing, nil)

		var all []string
		step := func(mode impact.Mode) (*Layer, engine.Decision, error) {
			d, err := rep.Select(ctx, head, engine.Options{ConfigPath: in.ConfigPath, LockPath: lockPath, ModeSet: true, Mode: mode})
			if err != nil {
				return nil, d, fmt.Errorf("eval: mutant %s: %w", ev.ID, err)
			}
			l, inv := builder.layer(d, e.failSet)
			if all == nil && inv != nil {
				all = inv
			}
			return l, d, nil
		}
		var graphDecision, coverDecision engine.Decision
		if runGraph {
			if e.Graph, graphDecision, err = step(impact.ModeGraph); err != nil {
				return err
			}
		}
		if runCoverage {
			if e.Coverage, coverDecision, err = step(impact.ModeCoverage); err != nil {
				return err
			}
		}
		if all == nil {
			all = inventory
		}
		e.All = intern(all)
		if wantP {
			e.PLayer, e.Scored = e.Graph, map[string]*Scored{}
			scoredOn := graphDecision
			if pMode == impact.ModeCoverage {
				e.PLayer, scoredOn = e.Coverage, coverDecision
			}
			if err := scoreEvent(ctx, e, scoredOn, in.Predictors); err != nil {
				return err
			}
		}

		switch e.Partition {
		case events.PartitionTune:
			c.Tune = append(c.Tune, e)
		case events.PartitionTest:
			c.Test = append(c.Test, e)
		}
		if (i+1)%25 == 0 {
			in.logf("replayed %d of %d events", i+1, len(evs))
		}
	}
	c.count()
	return nil
}

// scoreEvent asks every usable predictor about the residual candidates of an
// event. Nobody is asked when L1 or the change already demands the full suite;
// a predictor that fails marks the event, which then abstains.
func scoreEvent(ctx context.Context, e *Event, d engine.Decision, predictors map[string]predict.Predictor) error {
	if e.PLayer == nil || e.PLayer.demandsFull() || d.L1 == nil || d.Change == nil {
		return nil
	}
	for _, kind := range slices.Sorted(maps.Keys(predictors)) {
		start := time.Now()
		cands, scores, err := engine.Score(ctx, predictors[kind], d.Change, d.L1.Candidates)
		ms := float64(time.Since(start)) / float64(time.Millisecond)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			e.Scored[kind] = &Scored{Err: err.Error(), ScoreMS: ms}
			continue
		}
		usd := 0.0
		for _, sc := range scores {
			usd += sc.CostUSD
		}
		e.Scored[kind] = NewScored(e, cands, ms, usd)
	}
	return nil
}

// count fills the event counts of the replayed population.
func (c *collected) count() {
	for _, e := range c.Tune {
		c.Counts.Tune++
		if len(e.Failing) > 0 {
			c.Counts.TuneWithFailures++
		}
	}
	for _, e := range c.Test {
		c.Counts.Test++
		c.Counts.TestByKind[e.Kind]++
		if len(e.Failing) > 0 {
			c.Counts.TestWithFailures++
		}
	}
}
