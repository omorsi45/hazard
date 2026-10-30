package labels

import (
	"context"
	"reflect"
	"testing"
	"time"
)

var authoredBase = time.Date(2025, 7, 1, 12, 0, 0, 0, time.UTC)

// authored records the authored time of a tested commit, as mining does.
func (f *fixture) authored(sha string, at time.Time) {
	f.t.Helper()
	f.exec(`UPDATE ci_artifacts SET tested_authored_at=? WHERE tested_sha=?`, at.UTC().Format(testObservationFormat), sha)
}

// strongChain inserts a parent pass, a failing commit and a passing
// descendant of one test, which makes the commit a strong candidate, and gives
// the commit an authored time.
func (f *fixture) strongChain(parent, commit, desc, test string, at time.Time) {
	f.t.Helper()
	f.obs(spec{SHA: parent, Parents: ps(parent + "-root"), Status: "pass", Test: test})
	f.obs(spec{SHA: commit, Parents: ps(parent), Status: "fail", Message: "assertion failed", Test: test})
	f.obs(spec{SHA: desc, Parents: ps(commit), Status: "pass", Test: test})
	f.authored(commit, at)
}

func unitIDs(r UnitReport) []string {
	ids := make([]string, 0, len(r.Units))
	for _, u := range r.Units {
		ids = append(ids, u.ID)
	}
	return ids
}

func strongUnits(t *testing.T, f *fixture) UnitReport {
	t.Helper()
	r, err := StrongUnits(context.Background(), f.db)
	if err != nil {
		t.Fatalf("StrongUnits: %v", err)
	}
	return r
}

func TestStrongUnitsAreTheStrongCommitsOfTheCurrentBuild(t *testing.T) {
	f := newFixture(t)
	f.strongChain("p1", "c1", "d1", testAlpha, authoredBase)
	f.strongChain("p2", "c2", "d2", testBeta, authoredBase.Add(48*time.Hour))
	// One commit with two strong tests is one unit that lists both.
	f.strongChain("p3", "c3", "d3", testBeta, authoredBase.Add(72*time.Hour))
	f.obs(spec{SHA: "p3", Run: "run-p3-alpha", Parents: ps("p3-root"), Status: "pass", Test: testAlpha})
	f.obs(spec{SHA: "c3", Run: "run-c3-alpha", Parents: ps("p3"), Status: "fail", Message: "assertion failed", Test: testAlpha})
	f.obs(spec{SHA: "d3", Run: "run-d3-alpha", Parents: ps("c3"), Status: "pass", Test: testAlpha})
	built := f.build()
	if built.StrongCandidates != 4 {
		t.Fatalf("fixture must hold 4 strong candidates, got %d", built.StrongCandidates)
	}

	got := strongUnits(t, f)
	if got.Freshness != FreshnessCurrent || got.BuildID != built.BuildID || got.Cutoff != built.Cutoff {
		t.Fatalf("identity = %q %q %q, want the current build %q %q", got.Freshness, got.BuildID, got.Cutoff, built.BuildID, built.Cutoff)
	}
	want := []Unit{
		{ID: "ci:c1", SHA: "c1", ParentSHA: "p1", CommitTime: authoredBase, Repository: repoKey, Failing: []string{testAlpha}},
		{ID: "ci:c2", SHA: "c2", ParentSHA: "p2", CommitTime: authoredBase.Add(48 * time.Hour), Repository: repoKey, Failing: []string{testBeta}},
		{ID: "ci:c3", SHA: "c3", ParentSHA: "p3", CommitTime: authoredBase.Add(72 * time.Hour), Repository: repoKey, Failing: []string{testAlpha, testBeta}},
	}
	if !reflect.DeepEqual(got.Units, want) {
		t.Errorf("units =\n%+v\nwant\n%+v", got.Units, want)
	}
	if len(got.Unusable) != 0 {
		t.Errorf("unusable = %v, want none", got.Unusable)
	}
}

// Weak, pending and excluded (flaky) candidates never become units: the query
// reads the strong state only (G12: excluded labels never count).
func TestStrongUnitsNeverCountWeakPendingOrExcludedCandidates(t *testing.T) {
	f := newFixture(t)
	f.strongChain("p1", "c1", "d1", testAlpha, authoredBase)

	// weak: fails at the commit, parent passes, nothing recovers.
	f.obs(spec{SHA: "pw", Parents: ps("pw-root"), Status: "pass"})
	f.obs(spec{SHA: "cw", Parents: ps("pw"), Status: "fail", Message: "assertion failed"})
	f.authored("cw", authoredBase.Add(time.Hour))

	// pending: no observation of the parent at all.
	f.obs(spec{SHA: "cp", Parents: ps("pp"), Status: "fail", Message: "assertion failed"})
	f.authored("cp", authoredBase.Add(2*time.Hour))

	// excluded: a strong-looking history, but the test also passes at the
	// failing commit in the same environment, which is a flaky signal.
	f.strongChain("pe", "ce", "de", testAlpha, authoredBase.Add(3*time.Hour))
	f.obs(spec{SHA: "ce", Run: "run-ce-pass", Parents: ps("pe"), Status: "pass"})

	built := f.build()
	if built.StrongCandidates != 1 || built.WeakCandidates != 1 || built.PendingCandidates != 1 || built.ExcludedCandidates != 1 {
		t.Fatalf("fixture counts strong=%d weak=%d pending=%d excluded=%d, want one of each",
			built.StrongCandidates, built.WeakCandidates, built.PendingCandidates, built.ExcludedCandidates)
	}

	got := strongUnits(t, f)
	if want := []string{"ci:c1"}; !reflect.DeepEqual(unitIDs(got), want) {
		t.Errorf("units = %v, want %v: only the strong commit counts", unitIDs(got), want)
	}
	if len(got.Unusable) != 0 {
		t.Errorf("unusable = %v, want none: a non-strong commit is not even a candidate unit", got.Unusable)
	}
}

func TestStrongUnitsOfANonCurrentBuildAreNotCounted(t *testing.T) {
	t.Run("stale", func(t *testing.T) {
		f := newFixture(t)
		f.strongChain("p1", "c1", "d1", testAlpha, authoredBase)
		f.build()
		f.flag(testAlpha, "quarantined") // a semantic input changed after the build
		got := strongUnits(t, f)
		if got.Freshness != FreshnessStale || len(got.Units) != 0 || len(got.Unusable) != 0 {
			t.Errorf("stale build: freshness=%q units=%v unusable=%v, want stale and nothing counted", got.Freshness, unitIDs(got), got.Unusable)
		}
		if got.BuildID == "" {
			t.Error("a stale report must still name the build it describes")
		}
	})
	t.Run("unavailable", func(t *testing.T) {
		f := newFixture(t)
		f.strongChain("p1", "c1", "d1", testAlpha, authoredBase)
		got := strongUnits(t, f)
		if got.Freshness != FreshnessUnavailable || len(got.Units) != 0 || got.BuildID != "" {
			t.Errorf("no build: freshness=%q units=%v build=%q, want unavailable and nothing counted", got.Freshness, unitIDs(got), got.BuildID)
		}
	})
}

// Only the current build is read: an older build with strong candidates does
// not leak into the cohort.
func TestStrongUnitsReadTheCurrentBuildOnly(t *testing.T) {
	f := newFixture(t)
	f.strongChain("p1", "c1", "d1", testAlpha, authoredBase)
	first := f.build()
	if first.StrongCandidates != 1 {
		t.Fatalf("first build strong = %d, want 1", first.StrongCandidates)
	}
	// An earlier cutoff sees none of the observations, so the new current
	// build holds no candidate while the old one still holds a strong one.
	empty, err := Build(context.Background(), f.db, Options{Before: baseTime})
	if err != nil {
		t.Fatal(err)
	}
	if empty.BuildID == first.BuildID || empty.StrongCandidates != 0 {
		t.Fatalf("second build = %+v, want a different build without strong candidates", empty)
	}
	got := strongUnits(t, f)
	if got.BuildID != empty.BuildID || len(got.Units) != 0 {
		t.Errorf("units = %v of build %s, want none of the current build %s", unitIDs(got), got.BuildID, empty.BuildID)
	}
}

// A commit whose authored time is unknown, or contradictory across the
// acquired reports, cannot be placed in time: it is reported, not counted.
func TestStrongUnitsWithoutAUsableCommitTimeAreReportedNotCounted(t *testing.T) {
	f := newFixture(t)
	f.strongChain("p1", "c1", "d1", testAlpha, authoredBase)
	f.strongChain("p2", "c2", "d2", testBeta, authoredBase)
	f.strongChain("p3", "c3", "d3", testBeta, authoredBase)
	f.exec(`UPDATE ci_artifacts SET tested_authored_at=NULL WHERE tested_sha='c2'`)
	// c3 is reported with two different authored times by two artifacts.
	f.obs(spec{SHA: "c3", Run: "run-c3-again", Parents: ps("p3"), Status: "fail", Message: "assertion failed", Test: testBeta})
	f.exec(`UPDATE ci_artifacts SET tested_authored_at=? WHERE run_id='run-c3-again'`, authoredBase.Add(time.Hour).Format(testObservationFormat))
	f.build()

	got := strongUnits(t, f)
	if want := []string{"ci:c1"}; !reflect.DeepEqual(unitIDs(got), want) {
		t.Errorf("units = %v, want %v", unitIDs(got), want)
	}
	if want := map[string]int{"commit_time_unknown": 2}; !reflect.DeepEqual(got.Unusable, want) {
		t.Errorf("unusable = %v, want %v", got.Unusable, want)
	}
}
