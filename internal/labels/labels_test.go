package labels

import (
	"reflect"
	"testing"
)

// assertConserved checks both D048 conservation equations.
func assertConserved(t *testing.T, r Report) {
	t.Helper()
	if r.RawFailingCases != r.GroupedMemberCount+r.UnresolvedRawMembers {
		t.Errorf("raw_failing_cases=%d != grouped_member_count=%d + unresolved_raw_members=%d", r.RawFailingCases, r.GroupedMemberCount, r.UnresolvedRawMembers)
	}
	if r.UnresolvedRawMembers != r.UnresolvedPending+r.UnresolvedExcluded {
		t.Errorf("unresolved_raw_members=%d != pending=%d + excluded=%d", r.UnresolvedRawMembers, r.UnresolvedPending, r.UnresolvedExcluded)
	}
	if r.RawCandidates != r.ExcludedCandidates+r.PendingCandidates+r.WeakCandidates+r.StrongCandidates {
		t.Errorf("raw_candidates=%d != excluded=%d + pending=%d + weak=%d + strong=%d", r.RawCandidates, r.ExcludedCandidates, r.PendingCandidates, r.WeakCandidates, r.StrongCandidates)
	}
}

type counts struct{ raw, grouped, unresolved, candidates, excluded, pending, weak, strong int }

func assertCounts(t *testing.T, r Report, want counts) {
	t.Helper()
	got := counts{r.RawFailingCases, r.GroupedMemberCount, r.UnresolvedRawMembers, r.RawCandidates, r.ExcludedCandidates, r.PendingCandidates, r.WeakCandidates, r.StrongCandidates}
	if got != want {
		t.Errorf("counts = %+v, want %+v", got, want)
	}
	assertConserved(t, r)
}

func TestBuildStrongAttribution(t *testing.T) {
	f := newFixture(t)
	f.standard()
	r := f.build()
	assertCounts(t, r, counts{raw: 1, grouped: 1, candidates: 1, strong: 1})
	if !reflect.DeepEqual(r.StrongCommitsByRepository, map[string]int{repoKey: 1}) {
		t.Errorf("strong commits = %v", r.StrongCommitsByRepository)
	}
	if r.Freshness != FreshnessCurrent || len(r.BuildID) != 64 || r.Cutoff != "2026-01-01T00:00:00Z" || r.AlgorithmVersion != AlgorithmVersion {
		t.Errorf("identity fields: %+v", r)
	}
	if len(r.InputDigest) != 64 || len(r.DecisionDigest) != 64 {
		t.Errorf("digests: input=%q decision=%q", r.InputDigest, r.DecisionDigest)
	}
	row := f.candidate(r.BuildID, "c1", testAlpha, "linux")
	if row.State != "strong" || row.Kind != "candidate" || len(row.Reasons) != 0 {
		t.Errorf("candidate = %+v", row)
	}
}

func TestBuildEmptyDatabaseIsAValidZeroCohort(t *testing.T) {
	f := newFixture(t)
	r := f.build()
	assertCounts(t, r, counts{})
	if r.Freshness != FreshnessCurrent || r.BuildID == "" {
		t.Errorf("empty cohort not published: %+v", r)
	}
	if r.CandidateReasonCounts == nil || r.UnresolvedReasonCounts == nil || r.StrongCommitsByRepository == nil || r.AcquisitionExclusionsByReason == nil || r.AlwaysRunTests == nil || r.Limitations == nil {
		t.Errorf("empty collections must be non-nil for JSON: %+v", r)
	}
	if len(r.Limitations) == 0 {
		t.Error("empty cohort must state that no rows is not evidence of no regressions")
	}
}

func TestUnresolvedMembersAreCountedButNeverCandidates(t *testing.T) {
	f := newFixture(t)
	f.standard()
	noSHA := f.obs(spec{Run: "r-a", SHA: "x1", Status: "fail", NoSHA: true})
	noTest := f.obs(spec{Run: "r-b", SHA: "x2", Status: "fail", NoTestID: true})
	noEnv := f.obs(spec{Run: "r-c", SHA: "x3", Status: "fail", NoEnv: true})
	noEvidence := f.obs(spec{Run: "r-d", SHA: "x4", Status: "fail", NoEnvEvidence: true})
	both := f.obs(spec{Run: "r-e", SHA: "x5", Status: "fail", NoSHA: true, NoEnv: true})
	r := f.build()
	assertCounts(t, r, counts{raw: 6, grouped: 1, unresolved: 5, candidates: 1, pending: 0, strong: 1})
	if r.UnresolvedPending != 5 || r.UnresolvedExcluded != 0 {
		t.Errorf("unresolved pending/excluded = %d/%d, want 5/0", r.UnresolvedPending, r.UnresolvedExcluded)
	}
	wantReasons := map[string]int{"missing_tested_revision": 2, "missing_identity": 1, "missing_environment": 3}
	if !reflect.DeepEqual(r.UnresolvedReasonCounts, wantReasons) {
		t.Errorf("unresolved reasons = %v, want %v", r.UnresolvedReasonCounts, wantReasons)
	}
	if len(r.CandidateReasonCounts) != 0 {
		t.Errorf("unresolved reasons leaked into candidate counts: %v", r.CandidateReasonCounts)
	}
	records := f.unresolved(r.BuildID)
	for unit, want := range map[string][]string{
		noSHA: {"missing_tested_revision"}, noTest: {"missing_identity"}, noEnv: {"missing_environment"},
		noEvidence: {"missing_environment"}, both: {"missing_environment", "missing_tested_revision"},
	} {
		got, ok := records[unit]
		if !ok || got.State != "pending" || !reflect.DeepEqual(sortedKeys(got.Reasons), want) {
			t.Errorf("record %s = %+v, want pending %v", unit, got, want)
		}
	}
	if f.count(`SELECT count(*) FROM label_candidates WHERE build_id=? AND kind='candidate'`, r.BuildID) != 1 {
		t.Error("unresolved records entered the candidate universe")
	}
}
