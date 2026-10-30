package telemetry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/omorsi45/hazard/internal/runner"
	"github.com/omorsi45/hazard/internal/store"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// sel is a narrowed selection record made at offset after t0.
func sel(id, commit string, offset time.Duration, members ...string) Record {
	r := Record{ID: id, Time: t0.Add(offset).Format(time.RFC3339), Commit: commit, Mode: ModeRun, LockDigest: "sha256:lock-" + id, Members: []Member{}}
	for _, m := range members {
		r.Members = append(r.Members, Member{TestID: m, Source: "certain"})
	}
	return r
}

const (
	tA = "go:example.com/m/p:TestA"
	tB = "go:example.com/m/p:TestB"
	tC = "go:example.com/m/p:TestC"
)

// fakeGit is a linear history with scripted test outcomes.
type fakeGit struct {
	history  []string          // oldest first
	firstBad map[string]string // test -> first failing commit; the test fails from there on
	unknown  map[string]bool   // commits at which every test is inconclusive
	probes   []string          // "commit test" log
	probeErr error
}

func (g *fakeGit) index(c string) int { return slices.Index(g.history, c) }

func (g *fakeGit) deps() AuditDeps {
	return AuditDeps{
		IsAncestor: func(_ context.Context, a, b string) (bool, error) {
			i, j := g.index(a), g.index(b)
			return i >= 0 && j >= 0 && i <= j, nil
		},
		Range: func(_ context.Context, oldest, newest string) ([]string, string, error) {
			i, j := g.index(oldest), g.index(newest)
			if i < 0 || j < i {
				return nil, "", fmt.Errorf("no path from %s to %s", oldest, newest)
			}
			parent := ""
			if i > 0 {
				parent = g.history[i-1]
			}
			return slices.Clone(g.history[i : j+1]), parent, nil
		},
		Probe: func(_ context.Context, commit, test string) (State, error) {
			g.probes = append(g.probes, commit+" "+test)
			if g.probeErr != nil {
				return StateUnknown, g.probeErr
			}
			if g.unknown[commit] {
				return StateUnknown, nil
			}
			if bad, ok := g.firstBad[test]; ok && g.index(commit) >= g.index(bad) {
				return StateFail, nil
			}
			return StatePass, nil
		},
	}
}

func linearHistory(n int) *fakeGit {
	g := &fakeGit{firstBad: map[string]string{}, unknown: map[string]bool{}}
	for i := 1; i <= n; i++ {
		g.history = append(g.history, fmt.Sprintf("c%d", i))
	}
	return g
}

func auditInput(commit string, failing []string, records ...Record) AuditInput {
	return AuditInput{Records: records, Since: Since{Time: t0}, FullRunID: "run1", Commit: commit, Failing: failing, Now: t0.Add(time.Hour)}
}

func TestParseSince(t *testing.T) {
	got, err := ParseSince("2026-10-01T12:00:00Z")
	if err != nil || !got.Time.Equal(t0) || got.ID != "" {
		t.Errorf("a time: %+v, %v", got, err)
	}
	got, err = ParseSince("0123456789abcdef0123456789abcdef")
	if err != nil || got.ID != "0123456789abcdef0123456789abcdef" || !got.Time.IsZero() {
		t.Errorf("an ID: %+v, %v", got, err)
	}
	if _, err := ParseSince(""); err == nil {
		t.Error("an empty --since-run must be an error")
	}
}

func TestSinceRecords(t *testing.T) {
	// Equal times: the ID form must use the position, not the time.
	recs := []Record{sel("r1", "c1", 0), sel("r2", "c2", time.Minute), sel("r3", "c3", time.Minute), sel("r4", "c4", 2*time.Minute)}
	ids := func(rs []Record) []string {
		var out []string
		for _, r := range rs {
			out = append(out, r.ID)
		}
		return out
	}
	got, err := SinceRecords(recs, Since{ID: "r3"})
	if err != nil || !reflect.DeepEqual(ids(got), []string{"r3", "r4"}) {
		t.Errorf("since an ID: %v, %v; want r3 and later (inclusive)", ids(got), err)
	}
	got, err = SinceRecords(recs, Since{Time: t0.Add(time.Minute)})
	if err != nil || !reflect.DeepEqual(ids(got), []string{"r2", "r3", "r4"}) {
		t.Errorf("since a time: %v, %v; want records at or after it", ids(got), err)
	}
	if _, err := SinceRecords(recs, Since{ID: "nope"}); !errors.Is(err, ErrSinceNotFound) {
		t.Errorf("unknown ID: %v, want ErrSinceNotFound", err)
	}
	bad := append([]Record{}, recs...)
	bad[1].Time = "yesterday"
	if _, err := SinceRecords(bad, Since{Time: t0}); err == nil {
		t.Error("a record with an unreadable time must be an error, not silently skipped")
	}
}

func TestAuditSameCommitMissAndNonMiss(t *testing.T) {
	g := linearHistory(5)
	in := auditInput("c5", []string{tA, tB},
		sel("r1", "c5", time.Minute, tA),       // omits TestB: a miss
		sel("r2", "c5", 2*time.Minute, tA, tB)) // caught both
	rep, err := Audit(context.Background(), in, g.deps())
	if err != nil {
		t.Fatal(err)
	}
	wantSel := []SelectionAudit{
		{SelectionID: "r1", LockDigest: "sha256:lock-r1", Commit: "c5", FullRunID: "run1", Failing: []string{tA, tB}, Missed: []string{tB}, RegressionBearing: true},
		{SelectionID: "r2", LockDigest: "sha256:lock-r2", Commit: "c5", FullRunID: "run1", Failing: []string{tA, tB}, Missed: []string{}, RegressionBearing: true},
	}
	if !reflect.DeepEqual(rep.Selections, wantSel) {
		t.Errorf("selections =\n%+v\nwant\n%+v", rep.Selections, wantSel)
	}
	if len(rep.MissList) != 1 {
		t.Fatalf("misses = %+v", rep.MissList)
	}
	m := rep.MissList[0]
	if m.ID == "" || m.SelectionID != "r1" || m.TestID != tB || m.DetectedAt != "2026-10-01T13:00:00Z" || m.FullRunID != "run1" || m.BlameWindow != "c5..c5" || !m.Attributed {
		t.Errorf("miss = %+v", m)
	}
	if rep.Audited != 2 || rep.RegressionBearing != 2 || rep.Misses != 1 || rep.Attributed != 1 || rep.Unattributed != 0 {
		t.Errorf("counts = %+v", rep)
	}
	if len(g.probes) != 0 {
		t.Errorf("a same-commit audit needs no bisect: %v", g.probes)
	}
	again, err := Audit(context.Background(), in, g.deps())
	if err != nil || again.MissList[0].ID != m.ID {
		t.Errorf("the miss ID must be stable so a re-audit does not duplicate rows: %q vs %q (%v)", again.MissList[0].ID, m.ID, err)
	}
}

func TestAuditAGreenFullRunIsNotRegressionBearing(t *testing.T) {
	g := linearHistory(2)
	rep, err := Audit(context.Background(), auditInput("c2", nil, sel("r1", "c2", time.Minute, tA)), g.deps())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Selections) != 1 || rep.Audited != 1 || rep.RegressionBearing != 0 || rep.Misses != 0 || rep.Selections[0].RegressionBearing || len(rep.Selections[0].Failing) != 0 {
		t.Fatalf("report = %+v", rep)
	}
	if rep.Selections[0].Failing == nil || rep.Selections[0].Missed == nil || rep.MissList == nil || rep.UnattributedTests == nil || rep.Failing == nil {
		t.Error("the JSON lists must be arrays, never null")
	}
}

func TestAuditOnlyAuditsRealSelectionsSinceTheStart(t *testing.T) {
	g := linearHistory(3)
	full := sel("full", "c3", time.Minute, tA)
	full.FullSuite = true
	shadow := sel("shadow", "c3", time.Minute, tA)
	shadow.Mode, shadow.FullSuite = ModeShadow, true
	shadowNarrow := sel("shadow2", "c3", time.Minute, tA)
	shadowNarrow.Mode = ModeShadow
	old := sel("old", "c3", -time.Hour, tA)
	selectMode := sel("sel", "c3", 2*time.Minute, tA)
	selectMode.Mode = ModeSelect
	rep, err := Audit(context.Background(), auditInput("c3", []string{tB}, old, full, shadow, shadowNarrow, selectMode, sel("run", "c3", 3*time.Minute, tA)), g.deps())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range rep.Selections {
		got = append(got, s.SelectionID)
	}
	if !reflect.DeepEqual(got, []string{"sel", "run"}) {
		t.Errorf("audited %v, want only the select and run records since the start (not full-suite, shadow or older ones)", got)
	}
}

func TestAuditInclusiveSinceByID(t *testing.T) {
	g := linearHistory(2)
	in := auditInput("c2", []string{tB}, sel("r1", "c2", time.Minute, tA), sel("r2", "c2", 2*time.Minute, tA))
	in.Since = Since{ID: "r2"}
	rep, err := Audit(context.Background(), in, g.deps())
	if err != nil || rep.Audited != 1 || rep.Selections[0].SelectionID != "r2" {
		t.Errorf("report = %+v, err %v; want only r2", rep, err)
	}
	in.Since = Since{ID: "ghost"}
	if _, err := Audit(context.Background(), in, g.deps()); !errors.Is(err, ErrSinceNotFound) {
		t.Errorf("err = %v, want ErrSinceNotFound", err)
	}
}

// TestAuditAnotherCommitIsNotAMissWithoutBisect is the attribution guard: a
// selection made at another commit cannot be blamed for a failure the full run
// found at a later one unless a bisect proves where it started.
func TestAuditAnotherCommitIsNotAMissWithoutBisect(t *testing.T) {
	g := linearHistory(5)
	in := auditInput("c5", []string{tB}, sel("r1", "c3", time.Minute, tA))
	rep, err := Audit(context.Background(), in, g.deps())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Misses != 0 || len(rep.MissList) != 0 || rep.Attributed != 0 {
		t.Fatalf("a record on another commit is not a miss without a bisect: %+v", rep)
	}
	if rep.Audited != 0 || len(rep.Selections) != 0 {
		t.Errorf("only same-commit selections are audited: %+v", rep.Selections)
	}
	if rep.Unattributed != 1 || len(rep.UnattributedTests) != 1 || rep.UnattributedTests[0].TestID != tB || !strings.Contains(rep.UnattributedTests[0].Reason, "--bisect") {
		t.Errorf("the failure must be reported as unattributed with the way out: %+v", rep.UnattributedTests)
	}
	if len(g.probes) != 0 {
		t.Errorf("no bisect was asked for: %v", g.probes)
	}
}

func TestAuditBisectAttributesTheFirstFailingCommit(t *testing.T) {
	g := linearHistory(5)
	g.firstBad[tB] = "c3"
	in := auditInput("c5", []string{tB}, sel("r1", "c3", time.Minute, tA))
	in.Bisect = true
	rep, err := Audit(context.Background(), in, g.deps())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.MissList) != 1 {
		t.Fatalf("report = %+v", rep)
	}
	m := rep.MissList[0]
	if m.SelectionID != "r1" || m.TestID != tB || m.BlameWindow != "c2..c3" || !m.Attributed || m.FullRunID != "run1" {
		t.Errorf("miss = %+v, want r1 missing TestB with blame window c2..c3", m)
	}
	if rep.Misses != 1 || rep.Attributed != 1 || rep.Unattributed != 0 || rep.Audited != 0 {
		t.Errorf("counts = %+v", rep)
	}
	wantProbes := []string{"c5 " + tB, "c2 " + tB, "c3 " + tB}
	if !reflect.DeepEqual(g.probes, wantProbes) {
		t.Errorf("probes = %v, want %v (full-run commit, the commit before the range, then the search)", g.probes, wantProbes)
	}
}

func TestAuditBisectFindsTheFirstBadCommitAnywhereInTheRange(t *testing.T) {
	for bad := 1; bad <= 9; bad++ {
		t.Run(fmt.Sprintf("first bad c%d of 9", bad), func(t *testing.T) {
			g := linearHistory(9)
			g.firstBad[tB] = fmt.Sprintf("c%d", bad)
			first := sel("first", "c1", time.Minute, tA)
			in := auditInput("c9", []string{tB}, first, sel("at-bad", fmt.Sprintf("c%d", bad), 2*time.Minute, tA))
			in.Bisect = true
			rep, err := Audit(context.Background(), in, g.deps())
			if err != nil {
				t.Fatal(err)
			}
			wantSel := "at-bad"
			if bad == 1 {
				wantSel = "first" // both records sit at c1
			}
			var gotSel []string
			for _, m := range rep.MissList {
				gotSel = append(gotSel, m.SelectionID)
			}
			if len(rep.MissList) == 0 || !slices.Contains(gotSel, wantSel) {
				t.Fatalf("misses %v, want one for %s; report %+v", gotSel, wantSel, rep)
			}
			if got := rep.MissList[len(rep.MissList)-1].BlameWindow; !strings.HasSuffix(got, fmt.Sprintf("..c%d", bad)) {
				t.Errorf("blame window %q must end at the first bad commit c%d", got, bad)
			}
			if len(g.probes) > 6 {
				t.Errorf("%d probes for 9 commits, want a binary search: %v", len(g.probes), g.probes)
			}
		})
	}
}

func TestAuditBisectBlameWindowOfARootCommit(t *testing.T) {
	g := linearHistory(3)
	g.firstBad[tB] = "c1"
	in := auditInput("c3", []string{tB}, sel("r1", "c1", time.Minute, tA))
	in.Bisect = true
	rep, err := Audit(context.Background(), in, g.deps())
	if err != nil || len(rep.MissList) != 1 || rep.MissList[0].BlameWindow != "root..c1" {
		t.Errorf("miss list = %+v, err %v; want blame window root..c1", rep.MissList, err)
	}
}

func TestAuditBisectLeavesUnattributedWhatItCannotProve(t *testing.T) {
	type tc struct {
		name   string
		setup  func(*fakeGit)
		record Record
		reason string
	}
	for _, c := range []tc{
		{"the first bad commit has no selection record", func(g *fakeGit) { g.firstBad[tB] = "c4" }, sel("r1", "c3", time.Minute, tA), "no selection"},
		{"the selection at the first bad commit included the test", func(g *fakeGit) { g.firstBad[tB] = "c3" }, sel("r1", "c3", time.Minute, tA, tB), "no selection"},
		{"the test passes at the full-run commit again", func(g *fakeGit) {}, sel("r1", "c3", time.Minute, tA), "reproduce"},
		{"the test already failed before the first selection", func(g *fakeGit) { g.firstBad[tB] = "c1" }, sel("r1", "c3", time.Minute, tA), "before"},
		{"a commit is inconclusive", func(g *fakeGit) { g.firstBad[tB] = "c3"; g.unknown["c3"] = true }, sel("r1", "c3", time.Minute, tA), "inconclusive"},
		{"a probe fails", func(g *fakeGit) { g.probeErr = errors.New("worktree exploded") }, sel("r1", "c3", time.Minute, tA), "worktree exploded"},
		{"the selection is on a commit that is not an ancestor", func(g *fakeGit) { g.firstBad[tB] = "c3" }, sel("r1", "elsewhere", time.Minute, tA), "no audited selection"},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := linearHistory(5)
			c.setup(g)
			in := auditInput("c5", []string{tB}, c.record)
			in.Bisect = true
			rep, err := Audit(context.Background(), in, g.deps())
			if err != nil {
				t.Fatal(err)
			}
			if rep.Misses != 0 || len(rep.MissList) != 0 {
				t.Errorf("nothing was proven, yet misses = %+v", rep.MissList)
			}
			if rep.Unattributed != 1 || !strings.Contains(rep.UnattributedTests[0].Reason, c.reason) {
				t.Errorf("unattributed = %+v, want one mentioning %q", rep.UnattributedTests, c.reason)
			}
		})
	}
}

func TestAuditBisectStartsAtTheTopologicallyOldestAncestorSelection(t *testing.T) {
	g := linearHistory(6)
	g.firstBad[tB] = "c4"
	in := auditInput("c6", []string{tB},
		sel("off-branch", "elsewhere", time.Minute, tA), // not an ancestor: ignored
		sel("newer", "c5", 2*time.Minute, tA),
		sel("oldest", "c4", 3*time.Minute, tA)) // recorded later, but on the older commit
	in.Bisect = true
	rep, err := Audit(context.Background(), in, g.deps())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.MissList) != 1 || rep.MissList[0].SelectionID != "oldest" || rep.MissList[0].BlameWindow != "c3..c4" {
		t.Errorf("misses = %+v, want the selection on c4 blamed with window c3..c4", rep.MissList)
	}
	if len(g.probes) < 2 || g.probes[1] != "c3 "+tB {
		t.Errorf("probes = %v, want the parent of the oldest eligible commit (c3) verified second", g.probes)
	}
}

func TestAuditSameCommitSelectionsExplainTheFailureSoNothingIsBisected(t *testing.T) {
	g := linearHistory(5)
	g.firstBad[tB] = "c3"
	in := auditInput("c5", []string{tB}, sel("r1", "c3", time.Minute, tA), sel("r2", "c5", 2*time.Minute, tA, tB))
	in.Bisect = true
	rep, err := Audit(context.Background(), in, g.deps())
	if err != nil {
		t.Fatal(err)
	}
	if len(g.probes) != 0 || rep.Misses != 0 || rep.Unattributed != 0 || rep.Audited != 1 {
		t.Errorf("probes %v, report %+v; the same-commit selection caught the test", g.probes, rep)
	}
}

func TestAuditBisectsEachUnexplainedTestSeparately(t *testing.T) {
	g := linearHistory(5)
	g.firstBad[tB] = "c3"
	g.firstBad[tC] = "c4"
	in := auditInput("c5", []string{tB, tC}, sel("r3", "c3", time.Minute, tA), sel("r4", "c4", 2*time.Minute, tA))
	in.Bisect = true
	rep, err := Audit(context.Background(), in, g.deps())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, m := range rep.MissList {
		got[m.TestID] = m.SelectionID + " " + m.BlameWindow
	}
	want := map[string]string{tB: "r3 c2..c3", tC: "r4 c3..c4"}
	if !reflect.DeepEqual(got, want) || rep.Misses != 2 || rep.Attributed != 2 {
		t.Errorf("misses = %v (counts %+v), want %v", got, rep, want)
	}
}

func TestAuditCountsFailuresWithoutAnIdentity(t *testing.T) {
	g := linearHistory(2)
	in := auditInput("c2", []string{tB}, sel("r1", "c2", time.Minute, tB))
	in.Unidentified = 2
	rep, err := Audit(context.Background(), in, g.deps())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Unattributed != 2 || len(rep.Warnings) == 0 || !strings.Contains(strings.Join(rep.Warnings, "\n"), "2 failing") {
		t.Errorf("report = %+v; unidentified failures must be counted and warned about", rep)
	}
}

func TestAuditStopsOnCancellation(t *testing.T) {
	g := linearHistory(5)
	g.firstBad[tB] = "c3"
	ctx, cancel := context.WithCancel(context.Background())
	deps := g.deps()
	probe := deps.Probe
	deps.Probe = func(c context.Context, commit, test string) (State, error) {
		cancel()
		return probe(c, commit, test)
	}
	in := auditInput("c5", []string{tB}, sel("r1", "c3", time.Minute, tA))
	in.Bisect = true
	if _, err := Audit(ctx, in, deps); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestFailingTests(t *testing.T) {
	cases := []runner.JUnitCase{
		{Suite: "example.com/m/p", Classname: "example.com/m/p", Name: "TestB", Status: "fail"},
		{Suite: "example.com/m/p", Classname: "example.com/m/p", Name: "TestB", Status: "fail"}, // duplicate
		{Suite: "example.com/m/p", Classname: "example.com/m/p", Name: "TestA", Status: "pass"},
		{Suite: "example.com/m/p", Classname: "example.com/m/p", Name: "TestS", Status: "skip"},
		{Suite: "example.com/m/p", Classname: "example.com/m/p", Name: "TestC/with space", Status: "error"},
		{Suite: "pytest", Classname: "tests.test_x.TestK", Name: "test_a[1]", Status: "error"},
		{Suite: "", Classname: "test_y", Name: "test_known", Status: "fail"},
		{Suite: "", Classname: "test_z", Name: "test_file", Status: "fail", File: "test_z.py"},
		{Suite: "p", Classname: "p", Name: "", Status: "fail"}, // no name: no identity
	}
	known := map[string]bool{"py:test_y:test_known": true}
	ids, unidentified := FailingTests(cases, known)
	want := []string{
		"go:example.com/m/p:TestB",
		"go:example.com/m/p:TestC/with_space",
		"py:test_y:test_known",
		"py:test_z:test_file",
		"py:tests.test_x.TestK:test_a[1]",
	}
	if !reflect.DeepEqual(ids, want) || unidentified != 1 {
		t.Errorf("FailingTests = %v, %d; want %v, 1", ids, unidentified, want)
	}
}

func TestPersistWritesFieldMissesAndAuditLines(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, ".hazard", "hazard.db") // does not exist yet: Persist creates it
	audits := filepath.Join(dir, ".hazard", "audits.jsonl")
	g := linearHistory(5)
	rep, err := Audit(context.Background(), auditInput("c5", []string{tA, tB}, sel("r1", "c5", time.Minute, tA)), g.deps())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // twice: an identical re-audit must not duplicate the miss
		if err := Persist(db, audits, rep); err != nil {
			t.Fatal(err)
		}
	}
	s, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	}()
	var n, attributed int
	var sid, tid, at, run, window string
	if err := s.DB().QueryRow(`SELECT count(*) FROM field_misses`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("field_misses rows = %d, err %v; want 1", n, err)
	}
	if err := s.DB().QueryRow(`SELECT selection_id, test_id, detected_at, full_run_id, blame_window, attributed FROM field_misses`).Scan(&sid, &tid, &at, &run, &window, &attributed); err != nil {
		t.Fatal(err)
	}
	if sid != "r1" || tid != tB || at != "2026-10-01T13:00:00Z" || run != "run1" || window != "c5..c5" || attributed != 1 {
		t.Errorf("row = %s %s %s %s %s %d", sid, tid, at, run, window, attributed)
	}
	data, err := os.ReadFile(audits)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("audits.jsonl lines = %d, want one per Persist call: %q", len(lines), data)
	}
	for _, key := range []string{`"selection_id":"r1"`, `"lock_digest":"sha256:lock-r1"`, `"commit":"c5"`, `"full_run_id":"run1"`, `"failing":["` + tA + `","` + tB + `"]`, `"missed":["` + tB + `"]`, `"regression_bearing":true`} {
		if !strings.Contains(lines[0], key) {
			t.Errorf("audit line lacks %s: %s", key, lines[0])
		}
	}
}
