package events

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/omorsi45/hazard/internal/config"
	"github.com/omorsi45/hazard/internal/engine"
	"github.com/omorsi45/hazard/internal/impact"
	"github.com/omorsi45/hazard/internal/index"
	"github.com/omorsi45/hazard/internal/mutate"
	"github.com/omorsi45/hazard/internal/predict"
	"github.com/omorsi45/hazard/internal/store"
)

// RefusalError means the corpus cannot be collected on this repository as it
// stands, for a reason the user can act on (a missing database, an index for
// another commit). It is a result, not a failure of the program.
type RefusalError struct{ Reason string }

func (e *RefusalError) Error() string { return e.Reason }

// CollectOptions configures Collect.
type CollectOptions struct {
	RepoDir    string        // absolute git top level; only read
	DBPath     string        // .hazard/hazard.db
	ConfigPath string        // handed to the engine as is; empty lets the engine find the repository's own
	Config     config.Config // builds the index at the parent of each CI event
	Cutoff     time.Time     // exclusive: an event at exactly this instant is excluded
	CIWeight   float64       // Example.Weight of the rows of a CI event
	Log        io.Writer     // progress; nil discards
}

// Corpus is the training input collected from the database: the events that
// yielded rows, and the counts of everything else.
type Corpus struct {
	Events    []predict.TrainEvent
	Collected predict.Collected
}

// Collect reads the mutants and the CI labels of the current label build from
// the database, restricts them to the train partition and to events strictly
// before the cutoff, replays each remaining event through the engine in a
// scratch clone and turns the L1 candidates into labelled rows. Tune and test
// events are counted and never replayed: they belong to evaluation.
//
// A mutation event needs the index of the repository, which must describe the
// mutation base (RefusalError otherwise). A CI event is replayed on an index
// built, without coverage, at its parent in a scratch checkout.
func Collect(ctx context.Context, opts CollectOptions) (*Corpus, error) {
	c := &collector{opts: opts, observed: map[[3]string]int{}, skipped: map[string]int{}}
	recs, labels, err := c.readDatabase(ctx)
	if err != nil {
		return nil, err
	}
	if err := c.mutation(ctx, recs); err != nil {
		return nil, err
	}
	if err := c.ci(ctx, labels); err != nil {
		return nil, err
	}
	return c.corpus(), nil
}

type collector struct {
	opts     CollectOptions
	observed map[[3]string]int // origin, partition, status -> events
	skipped  map[string]int
	rows     []predict.TrainEvent
	certain  int
	unlisted int
}

func (c *collector) logf(format string, args ...any) {
	if c.opts.Log != nil {
		_, _ = fmt.Fprintf(c.opts.Log, format+"\n", args...)
	}
}

func (c *collector) observe(origin, partition, status string) {
	c.observed[[3]string{origin, partition, status}]++
}

func (c *collector) skip(reason string, n int) { c.skipped[reason] += n }

// inTrainPartition is the only gate between the observed events and the ones
// training reads: tune and test events never pass it.
func inTrainPartition(group string) bool { return Partition(group) == PartitionTrain }

// ciLabel is one commit with the tests the current label build marks as
// regressed by it.
type ciLabel struct {
	sha     string
	strong  bool // at least one of its labels is strong
	failing []string
}

func (c *collector) readDatabase(ctx context.Context) ([]mutate.Record, []ciLabel, error) {
	if _, err := os.Stat(c.opts.DBPath); errors.Is(err, fs.ErrNotExist) {
		return nil, nil, &RefusalError{Reason: fmt.Sprintf("there is no database at %s: run hazard mutate and hazard ci-mine first", c.opts.DBPath)}
	} else if err != nil {
		return nil, nil, fmt.Errorf("events: %w", err)
	}
	st, err := store.Open(c.opts.DBPath)
	if err != nil {
		return nil, nil, fmt.Errorf("events: %w", err)
	}
	// The replay copies the database file, so every handle must be closed
	// (which checkpoints the write-ahead log) before the first replay starts.
	defer func() { _ = st.Close() }()
	recs, err := readMutationRecords(ctx, st.DB())
	if err != nil {
		return nil, nil, err
	}
	labels, err := readCILabels(ctx, st.DB())
	if err != nil {
		return nil, nil, err
	}
	return recs, labels, nil
}

// readMutationRecords converts the mutants table and its kills to records,
// ordered by ID.
func readMutationRecords(ctx context.Context, db *sql.DB) ([]mutate.Record, error) {
	killers := map[string][]string{}
	kills, err := db.QueryContext(ctx, `SELECT mutant_id, test_id FROM mutant_kills ORDER BY mutant_id, test_id`)
	if err != nil {
		return nil, fmt.Errorf("events: read mutant kills: %w", err)
	}
	for kills.Next() {
		var id, test string
		if err := kills.Scan(&id, &test); err != nil {
			return nil, errors.Join(fmt.Errorf("events: read mutant kills: %w", err), kills.Close())
		}
		killers[id] = append(killers[id], test)
	}
	if err := errors.Join(kills.Err(), kills.Close()); err != nil {
		return nil, fmt.Errorf("events: read mutant kills: %w", err)
	}
	rows, err := db.QueryContext(ctx, `SELECT id, file, line, operator, original, mutated, base_sha, status FROM mutants ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("events: read mutants: %w", err)
	}
	var recs []mutate.Record
	for rows.Next() {
		var r mutate.Record
		if err := rows.Scan(&r.ID, &r.File, &r.Line, &r.Operator, &r.Original, &r.Mutated, &r.BaseSHA, &r.Status); err != nil {
			return nil, errors.Join(fmt.Errorf("events: read mutants: %w", err), rows.Close())
		}
		if r.Status == "killed" {
			r.Killers = killers[r.ID]
		}
		recs = append(recs, r)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("events: read mutants: %w", err)
	}
	return recs, nil
}

// readCILabels returns the strong and weak labels of the current label build,
// one entry per commit, ordered by SHA. Pending and excluded candidates are not
// labels, and neither is anything of an older build.
func readCILabels(ctx context.Context, db *sql.DB) ([]ciLabel, error) {
	rows, err := db.QueryContext(ctx, `SELECT tested_sha, test_id, state FROM label_candidates
		WHERE kind = 'candidate' AND state IN ('strong', 'weak')
		  AND build_id IN (SELECT build_id FROM label_builds WHERE is_current = 1)
		ORDER BY tested_sha, test_id`)
	if err != nil {
		return nil, fmt.Errorf("events: read labels: %w", err)
	}
	var out []ciLabel
	for rows.Next() {
		var sha, test, state string
		if err := rows.Scan(&sha, &test, &state); err != nil {
			return nil, errors.Join(fmt.Errorf("events: read labels: %w", err), rows.Close())
		}
		if len(out) == 0 || out[len(out)-1].sha != sha {
			out = append(out, ciLabel{sha: sha})
		}
		last := &out[len(out)-1]
		last.strong = last.strong || state == "strong"
		if !slices.Contains(last.failing, test) {
			last.failing = append(last.failing, test)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("events: read labels: %w", err)
	}
	return out, nil
}

// modeFor is the L1 policy of an index: coverage when it has coverage.
func modeFor(m *index.Manifest) impact.Mode {
	if m.Coverage != nil {
		return impact.ModeCoverage
	}
	return impact.ModeGraph
}

func (c *collector) engineOptions(m *index.Manifest) engine.Options {
	return engine.Options{ConfigPath: c.opts.ConfigPath, ModeSet: true, Mode: modeFor(m)}
}

// addRows records the outcome of one replayed event.
func (c *collector) addRows(prefix string, ev Event, d engine.Decision, weight float64) {
	rows, stats, skip := Examples(d, ev, weight)
	if skip != "" {
		c.skip(prefix+":"+skip, 1)
		return
	}
	c.certain += stats.FailingCertain
	c.unlisted += stats.FailingUnlisted
	c.rows = append(c.rows, predict.TrainEvent{ID: ev.ID, Origin: ev.Origin, Group: ev.Group, Time: ev.Time, Examples: rows})
}

func (c *collector) mutation(ctx context.Context, recs []mutate.Record) error {
	var train []mutate.Record
	for _, r := range recs {
		group := MutationGroup(r.File)
		c.observe(OriginMutation, Partition(group), r.Status)
		if inTrainPartition(group) {
			train = append(train, r)
		}
	}
	evs, contents, skipped, err := MutationEvents(ctx, c.opts.RepoDir, train)
	if err != nil {
		return err
	}
	for status, n := range skipped {
		c.skip("mutation:"+status, n)
	}
	var eligible []Event
	for _, ev := range evs {
		if ev.Time.Before(c.opts.Cutoff) {
			eligible = append(eligible, ev)
		} else {
			c.skip("mutation:after_cutoff", 1)
		}
	}
	if len(eligible) == 0 {
		return nil
	}
	m, err := index.Load(c.opts.RepoDir)
	if errors.Is(err, fs.ErrNotExist) {
		return &RefusalError{Reason: fmt.Sprintf("there is no index to replay the mutants on: run hazard index with the mutation base %s checked out", eligible[0].BaseSHA)}
	}
	if err != nil {
		return fmt.Errorf("events: load the index: %w", err)
	}
	for _, ev := range eligible {
		if ev.BaseSHA != m.HeadSHA {
			return &RefusalError{Reason: fmt.Sprintf("the index describes %s but mutant %s was made at %s: run hazard index with %s checked out, then train again",
				m.HeadSHA, ev.ID, ev.BaseSHA, ev.BaseSHA)}
		}
	}
	// One replay per base; the check above leaves a single base, the index's.
	return c.replayMutants(ctx, m, eligible, contents)
}

func (c *collector) replayMutants(ctx context.Context, m *index.Manifest, eligible []Event, contents map[string][]byte) error {
	byBase := map[string][]Event{}
	for _, ev := range eligible {
		byBase[ev.BaseSHA] = append(byBase[ev.BaseSHA], ev)
	}
	for _, base := range slices.Sorted(maps.Keys(byBase)) {
		if err := c.replayBase(ctx, m, base, byBase[base], contents); err != nil {
			return err
		}
	}
	return nil
}

// replayBase replays the mutants of one base in one scratch clone.
func (c *collector) replayBase(ctx context.Context, m *index.Manifest, base string, evs []Event, contents map[string][]byte) (err error) {
	rep, err := NewReplay(ctx, c.opts.RepoDir, base)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := rep.Close(); cerr != nil {
			err = errors.Join(err, cerr)
		}
	}()
	c.logf("replaying %d mutation events on %s", len(evs), base)
	for _, ev := range evs {
		head, err := rep.Commit(ctx, ev.File, contents[ev.ID])
		if err != nil {
			return fmt.Errorf("events: mutant %s: %w", ev.ID, err)
		}
		ev.HeadSHA = head
		d, err := rep.Select(ctx, head, c.engineOptions(m))
		if err != nil {
			return fmt.Errorf("events: mutant %s: %w", ev.ID, err)
		}
		c.addRows("mutation", ev, d, 1)
	}
	return nil
}

func (c *collector) ci(ctx context.Context, labels []ciLabel) error {
	for _, l := range labels {
		origin, status := OriginCIWeak, "weak"
		if l.strong {
			origin, status = OriginCIStrong, "strong"
		}
		group := "ci:" + l.sha
		c.observe(origin, Partition(group), status)
		if !inTrainPartition(group) {
			continue
		}
		at, parent, err := commitInfo(ctx, c.opts.RepoDir, l.sha)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			c.logf("commit %s: %v", l.sha, err)
			c.skip("ci:commit_unavailable", 1)
			continue
		}
		switch {
		case !at.Before(c.opts.Cutoff):
			c.skip("ci:after_cutoff", 1)
			continue
		case parent == "":
			c.skip("ci:no_parent", 1)
			continue
		}
		ev := Event{ID: group, Origin: origin, Group: group, BaseSHA: parent, HeadSHA: l.sha, Failing: slices.Sorted(slices.Values(l.failing)), Time: at}
		if err := c.replayCommit(ctx, ev); err != nil {
			return err
		}
	}
	return nil
}

// replayCommit replays one CI event on an index built at its parent.
func (c *collector) replayCommit(ctx context.Context, ev Event) (err error) {
	rep, err := NewReplay(ctx, c.opts.RepoDir, ev.BaseSHA)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c.logf("event %s: %v", ev.ID, err)
		c.skip("ci:replay_failed", 1)
		return nil
	}
	defer func() {
		if cerr := rep.Close(); cerr != nil {
			err = errors.Join(err, cerr)
		}
	}()
	c.logf("indexing %s for %s", ev.BaseSHA, ev.ID)
	m, err := index.Build(ctx, rep.Dir(), c.opts.Config, index.Options{})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c.logf("event %s: index at the parent: %v", ev.ID, err)
		c.skip("ci:index_failed", 1)
		return nil
	}
	if err := index.Publish(rep.Dir(), m); err != nil {
		return fmt.Errorf("events: event %s: %w", ev.ID, err)
	}
	d, err := rep.Select(ctx, ev.HeadSHA, c.engineOptions(m))
	if err != nil {
		return fmt.Errorf("events: event %s: %w", ev.ID, err)
	}
	c.addRows("ci", ev, d, c.opts.CIWeight)
	return nil
}

// commitInfo is the committer time of a commit and its first parent, empty for
// a root commit.
func commitInfo(ctx context.Context, repoDir, sha string) (time.Time, string, error) {
	out, err := runGit(ctx, repoDir, nil, nil, "show", "-s", "--format=%ct%n%P", sha+"^{commit}", "--")
	if err != nil {
		return time.Time{}, "", err
	}
	lines := strings.SplitN(strings.TrimRight(string(out), "\r\n"), "\n", 2)
	secs, err := strconv.ParseInt(strings.TrimSpace(lines[0]), 10, 64)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("committer time of %s: %w", sha, err)
	}
	parent := ""
	if len(lines) == 2 {
		parent, _, _ = strings.Cut(strings.TrimSpace(lines[1]), " ")
	}
	return time.Unix(secs, 0).UTC(), parent, nil
}

func (c *collector) corpus() *Corpus {
	slices.SortFunc(c.rows, func(a, b predict.TrainEvent) int {
		if d := a.Time.Compare(b.Time); d != 0 {
			return d
		}
		return strings.Compare(a.ID, b.ID)
	})
	counts := make([]predict.EventCount, 0, len(c.observed))
	for key, n := range c.observed {
		counts = append(counts, predict.EventCount{Origin: key[0], Partition: key[1], Status: key[2], Count: n})
	}
	slices.SortFunc(counts, func(a, b predict.EventCount) int {
		return strings.Compare(a.Origin+"\x00"+a.Partition+"\x00"+a.Status, b.Origin+"\x00"+b.Partition+"\x00"+b.Status)
	})
	return &Corpus{
		Events: c.rows,
		Collected: predict.Collected{
			Events: counts, Skipped: c.skipped, FailingCertain: c.certain, FailingUnlisted: c.unlisted,
		},
	}
}
