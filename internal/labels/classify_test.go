package labels

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

type mod = func(parent, fail, desc *spec)

type stateCase struct {
	name    string
	mods    []mod
	extra   func(f *fixture)
	state   string
	reasons map[string]string // exact reason -> effect set expected on the c1 candidate
}

// runStateCases builds the standard history per case and asserts the frozen
// decision of the candidate (c1, TestAlpha, linux): its final state and its
// exact reason set, plus both conservation equations.
func runStateCases(t *testing.T, cases []stateCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.standard(tc.mods...)
			if tc.extra != nil {
				tc.extra(f)
			}
			r := f.build()
			assertConserved(t, r)
			row := f.candidate(r.BuildID, "c1", testAlpha, "linux")
			if row.State != tc.state {
				t.Errorf("state = %s, want %s (reasons %v)", row.State, tc.state, row.Reasons)
			}
			want := tc.reasons
			if want == nil {
				want = map[string]string{}
			}
			if !reflect.DeepEqual(row.Reasons, want) {
				t.Errorf("reasons = %v, want %v", row.Reasons, want)
			}
		})
	}
}

func TestParentAndRecoveryPredicates(t *testing.T) {
	runStateCases(t, []stateCase{
		{name: "parent pass, persistent fail and verified descendant pass is strong", state: "strong"},
		{name: "no parent observation is pending", mods: []mod{func(p, c, d *spec) { p.SHA = "" }},
			state: "pending", reasons: map[string]string{"missing_parent_observation": "pending"}},
		{name: "unknown parents are pending", mods: []mod{func(p, c, d *spec) { c.Parents = "" }},
			state: "pending", reasons: map[string]string{"parent_unknown": "pending"}},
		{name: "verified root has no parent run", mods: []mod{func(p, c, d *spec) { c.Parents = "[]" }},
			state: "pending", reasons: map[string]string{"no_parent": "pending"}},
		{name: "multi-parent commit is merge ambiguity", mods: []mod{func(p, c, d *spec) { c.Parents = ps("p1", "p2") }},
			state: "pending", reasons: map[string]string{"merge_ambiguity": "pending"}},
		{name: "parent pass in a different environment cannot complete the comparison", mods: []mod{func(p, c, d *spec) { p.Env = "windows" }},
			state: "pending", reasons: map[string]string{"parent_comparison_unavailable": "pending"}},
		{name: "parent pass with unknown environment cannot complete the comparison", mods: []mod{func(p, c, d *spec) { p.NoEnv = true }},
			state: "pending", reasons: map[string]string{"parent_comparison_unavailable": "pending"}},
		{name: "parent pass without environment evidence cannot complete the comparison", mods: []mod{func(p, c, d *spec) { p.NoEnvEvidence = true }},
			state: "pending", reasons: map[string]string{"parent_comparison_unavailable": "pending"}},
		{name: "incomplete parent execution cannot complete the comparison", mods: []mod{func(p, c, d *spec) { p.State = "incomplete" }},
			state: "pending", reasons: map[string]string{"parent_comparison_unavailable": "pending"}},
		{name: "parent that also fails is not a regression start", mods: []mod{func(p, c, d *spec) { p.Status = "fail" }},
			state: "pending", reasons: map[string]string{"parent_not_passing": "pending"}},
		{name: "conflicting parent facts are not resolved by first row", state: "pending",
			extra:   func(f *fixture) { f.obs(spec{SHA: "c1", Run: "run-c1b", Status: "skip", Parents: ps("p9")}) },
			reasons: map[string]string{"parent_conflict": "pending"}},
		{name: "deletion without a passing descendant is weak", mods: []mod{func(p, c, d *spec) { d.Test = testBeta }},
			state: "weak", reasons: map[string]string{"no_passing_descendant": "weak"}},
		{name: "no descendant is weak", mods: []mod{func(p, c, d *spec) { d.SHA = "" }},
			state: "weak", reasons: map[string]string{"no_passing_descendant": "weak"}},
		{name: "unrelated future pass is not recovery", mods: []mod{func(p, c, d *spec) { d.Parents = ps("zzz") }},
			state: "weak", reasons: map[string]string{"no_passing_descendant": "weak"}},
		{name: "descendant with unknown parents has no verified path", mods: []mod{func(p, c, d *spec) { d.Parents = "" }},
			state: "weak", reasons: map[string]string{"no_passing_descendant": "weak"}},
		{name: "descendant on another branch is not recovery", mods: []mod{func(p, c, d *spec) { d.Branch = "release" }},
			state: "weak", reasons: map[string]string{"no_passing_descendant": "weak"}},
		{name: "descendant in another environment is not recovery", mods: []mod{func(p, c, d *spec) { d.Env = "windows" }},
			state: "weak", reasons: map[string]string{"no_passing_descendant": "weak"}},
		{name: "incomplete descendant execution is not recovery", mods: []mod{func(p, c, d *spec) { d.State = "incomplete" }},
			state: "weak", reasons: map[string]string{"no_passing_descendant": "weak"}},
		{name: "descendant through an intermediate commit is verified", mods: []mod{func(p, c, d *spec) { d.Parents = ps("c2") }}, state: "strong",
			extra: func(f *fixture) { f.obs(spec{SHA: "c2", Run: "run-c2", Parents: ps("c1"), Status: "skip"}) }},
		{name: "descendant merge commit with the candidate among its parents is verified", mods: []mod{func(p, c, d *spec) { d.Parents = ps("other", "c1") }}, state: "strong"},
	})
}

// Standard history units: unit-1 is the parent pass (run-p1), unit-2 the failing
// candidate (run-c1) and unit-3 the descendant pass (run-d1).
func TestWitnessesMustBeCanonicalPromotedResults(t *testing.T) {
	collide := func(unit string) func(f *fixture) {
		return func(f *fixture) { f.exclusion(unit, "duplicate_test_identity") }
	}
	conflict := func(run, unit string) func(f *fixture) {
		return func(f *fixture) { f.gap(run, unit, "acquisition_conflict", true) }
	}
	notCanonicalParent := map[string]string{"parent_pass_not_canonical": "pending"}
	noRecovery := map[string]string{"no_passing_descendant": "weak"}
	runStateCases(t, []stateCase{
		{name: "parent pass with a duplicate_test_identity exclusion cannot satisfy the parent", state: "pending",
			extra: collide("unit-1"), reasons: notCanonicalParent},
		{name: "descendant pass with a duplicate_test_identity exclusion is not recovery", state: "weak",
			extra: collide("unit-3"), reasons: noRecovery},
		{name: "parent pass in a unit with an active acquisition_conflict gap cannot satisfy the parent", state: "pending",
			extra: conflict("run-p1", "unit-1"), reasons: notCanonicalParent},
		{name: "descendant pass in a unit with an active acquisition_conflict gap is not recovery", state: "weak",
			extra: conflict("run-d1", "unit-3"), reasons: noRecovery},
		{name: "parent pass that was not promoted to test_results cannot satisfy the parent", state: "pending",
			mods:    []mod{func(p, c, d *spec) { p.NoPromote = true }},
			reasons: notCanonicalParent},
		{name: "descendant pass that was not promoted to test_results is not recovery", state: "weak",
			mods:    []mod{func(p, c, d *spec) { d.NoPromote = true }},
			reasons: noRecovery},
		{name: "a resolved conflict gap does not disqualify a witness", state: "strong",
			extra: func(f *fixture) { f.gap("run-p1", "unit-1", "acquisition_conflict", false) }},
		{name: "a canonical parent pass still satisfies the parent beside a colliding one", state: "strong",
			extra: func(f *fixture) {
				f.obs(spec{SHA: "p1", Run: "run-p1b", Parents: ps("p0"), Status: "pass"})
				f.exclusion("unit-1", "duplicate_test_identity")
			}},
		{name: "a canonical descendant pass still recovers beside a colliding one", state: "strong",
			extra: func(f *fixture) {
				f.obs(spec{SHA: "d1", Run: "run-d1b", Parents: ps("c1"), Status: "pass"})
				f.exclusion("unit-3", "duplicate_test_identity")
			}},
		{name: "a colliding same-commit pass is still a flaky signal", state: "excluded",
			extra: func(f *fixture) {
				unit := f.obs(spec{SHA: "c1", Run: "run-c1-pass", Parents: ps("p1"), Status: "pass"})
				f.exclusion(unit, "duplicate_test_identity")
			},
			reasons: map[string]string{"flaky_same_commit": "exclude"}},
		{name: "a same-commit pass in a conflicted unit is still a flaky signal", state: "excluded",
			extra: func(f *fixture) {
				unit := f.obs(spec{SHA: "c1", Run: "run-c1-pass", Parents: ps("p1"), Status: "pass"})
				f.gap("run-c1-pass", unit, "acquisition_conflict", true)
			},
			reasons: map[string]string{"flaky_same_commit": "exclude"}},
		{name: "an unpromoted same-commit pass is still a flaky signal", state: "excluded",
			extra: func(f *fixture) {
				f.obs(spec{SHA: "c1", Run: "run-c1-pass", Parents: ps("p1"), Status: "pass", NoPromote: true})
			},
			reasons: map[string]string{"flaky_same_commit": "exclude"}},
	})
}

func TestEveryAttemptAtTheCommitGroupsIntoOneCandidate(t *testing.T) {
	f := newFixture(t)
	f.standard()
	f.obs(spec{SHA: "c1", Run: "run-c1-retry", Parents: ps("p1"), Status: "fail", Message: "assertion failed"})
	f.obs(spec{SHA: "c1", Run: "run-c1-windows", Parents: ps("p1"), Status: "fail", Env: "windows"})
	r := f.build()
	assertCounts(t, r, counts{raw: 3, grouped: 3, candidates: 2, pending: 1, strong: 1})
	if f.candidate(r.BuildID, "c1", testAlpha, "linux").State != "strong" || f.candidate(r.BuildID, "c1", testAlpha, "windows").State != "pending" {
		t.Error("environments must never merge into retries")
	}
	if got := f.count(`SELECT member_count FROM label_candidates WHERE build_id=? AND environment_id='linux'`, r.BuildID); got != 2 {
		t.Errorf("linux member_count = %d, want 2", got)
	}
}

func TestPositiveDisqualifiersExclude(t *testing.T) {
	samePass := func(f *fixture) { f.obs(spec{SHA: "c1", Run: "run-c1-pass", Parents: ps("p1"), Status: "pass"}) }
	runStateCases(t, []stateCase{
		{name: "same-commit comparable pass and fail is flaky", extra: samePass,
			state: "excluded", reasons: map[string]string{"flaky_same_commit": "exclude"}},
		{name: "same-commit pass from an incomplete report still excludes", state: "excluded",
			extra: func(f *fixture) {
				f.obs(spec{SHA: "c1", Run: "run-c1-pass", Parents: ps("p1"), Status: "pass", State: "incomplete"})
			},
			reasons: map[string]string{"flaky_same_commit": "exclude"}},
		{name: "pass in a different known environment is not a flaky signal", state: "strong",
			extra: func(f *fixture) {
				f.obs(spec{SHA: "c1", Run: "run-c1-win", Parents: ps("p1"), Status: "pass", Env: "windows"})
			}},
		{name: "pass in an unknown environment cannot prove or clear flakiness", state: "pending",
			extra: func(f *fixture) {
				f.obs(spec{SHA: "c1", Run: "run-c1-unk", Parents: ps("p1"), Status: "pass", NoEnv: true})
			},
			reasons: map[string]string{"unknown_environment_pass": "pending"}},
		{name: "infra_reason excludes", mods: []mod{func(p, c, d *spec) { c.Infra = "runner_timeout" }},
			state: "excluded", reasons: map[string]string{"infra": "exclude"}},
		{name: "generic error text is not infrastructure", mods: []mod{func(p, c, d *spec) { c.Message = "error: expected 3 got 4, failure in assertion" }}, state: "strong"},
		{name: "status error is excluded as execution_error", mods: []mod{func(p, c, d *spec) { c.Status = "error" }},
			state: "excluded", reasons: map[string]string{"execution_error": "exclude"}},
		{name: "unstable test identity excludes", mods: []mod{func(p, c, d *spec) { c.Unstable = true }},
			state: "excluded", reasons: map[string]string{"unstable_identity": "exclude"}},
		{name: "inventory unstable flag excludes", state: "excluded", extra: func(f *fixture) { f.flag(testAlpha, "unstable_id") },
			reasons: map[string]string{"unstable_identity": "exclude"}},
		{name: "manual quarantine excludes", state: "excluded", extra: func(f *fixture) { f.flag(testAlpha, "quarantined") },
			reasons: map[string]string{"quarantined": "exclude"}},
		{name: "duplicate test identity collision excludes", state: "excluded",
			extra:   func(f *fixture) { f.exclusion("unit-2", "duplicate_test_identity") },
			reasons: map[string]string{"acquisition_collision": "exclude"}},
		{name: "active acquisition conflict excludes", state: "excluded",
			extra:   func(f *fixture) { f.gap("run-c1", "unit-2", "acquisition_conflict", true) },
			reasons: map[string]string{"acquisition_conflict": "exclude"}},
		{name: "every applicable exclusion reason is retained", state: "excluded",
			mods: []mod{func(p, c, d *spec) { c.Infra = "runner_timeout" }},
			extra: func(f *fixture) {
				f.obs(spec{SHA: "c1", Run: "run-c1-pass", Parents: ps("p1"), Status: "pass"})
				f.flag(testAlpha, "quarantined")
			},
			reasons: map[string]string{"infra": "exclude", "flaky_same_commit": "exclude", "quarantined": "exclude"}},
		{name: "excluded wins over pending", mods: []mod{func(p, c, d *spec) { p.SHA = "" }}, state: "excluded",
			extra:   func(f *fixture) { f.flag(testAlpha, "quarantined") },
			reasons: map[string]string{"quarantined": "exclude", "missing_parent_observation": "pending"}},
	})
}

func TestEveryInfrastructurePhraseExcludesCaseInsensitively(t *testing.T) {
	for _, phrase := range []string{"network unreachable", "no space left", "docker daemon", "registry auth", "oom kill", "out of memory", "job timed out", "runner failure"} {
		t.Run(phrase, func(t *testing.T) {
			runStateCases(t, []stateCase{{name: "phrase", mods: []mod{func(p, c, d *spec) { c.Message = "FATAL: " + strings.ToUpper(phrase) + " while running" }},
				state: "excluded", reasons: map[string]string{"infra": "exclude"}}})
		})
	}
}

func TestSkipOnlyObservationsAreNotCandidates(t *testing.T) {
	f := newFixture(t)
	f.obs(spec{SHA: "c1", Parents: ps("p1"), Status: "skip"})
	r := f.build()
	assertCounts(t, r, counts{})
}

func TestAlwaysRunIsTheUnionOfIdentifiedUnstableTests(t *testing.T) {
	f := newFixture(t)
	const gamma, delta, eps, zeta = "go:p:TestGamma", "go:p:TestDelta", "go:p:TestEps", "go:p:TestZeta"
	f.standard() // TestAlpha, same-commit pass added below makes it flaky
	f.obs(spec{SHA: "c1", Run: "run-c1-pass", Parents: ps("p1"), Status: "pass"})
	f.flag(testBeta, "quarantined") // no failing member in the cohort at all
	f.obs(spec{SHA: "g1", Run: "run-g1", Test: gamma, Status: "fail", Unstable: true, Parents: ps("g0")})
	f.obs(spec{SHA: "d1x", Run: "run-d", Test: delta, Status: "fail", NoEnv: true})
	f.flag(delta, "quarantined")
	f.obs(spec{SHA: "e1", Run: "run-e", Test: eps, Status: "fail", Infra: "runner_timeout", Parents: ps("e0")})
	f.obs(spec{SHA: "z1", Run: "run-z", Test: zeta, Status: "fail", Parents: ps("z0")})
	f.obs(spec{SHA: "n1", Run: "run-n", Status: "fail", NoTestID: true, Unstable: true})
	r := f.build()
	want := []string{testAlpha, testBeta, delta, gamma}
	if !reflect.DeepEqual(r.AlwaysRunTests, want) {
		t.Errorf("always_run_tests = %v, want %v", r.AlwaysRunTests, want)
	}
	// Delta (quarantined) and the unknown-identity unstable record are both
	// known disqualifiers; the latter adds no runnable ID to always-run.
	if r.UnresolvedExcluded != 2 || r.UnresolvedPending != 0 {
		t.Errorf("unresolved excluded/pending = %d/%d, want 2/0", r.UnresolvedExcluded, r.UnresolvedPending)
	}
	assertConserved(t, r)
	stored := f.count(`SELECT count(*) FROM label_builds WHERE build_id=? AND always_run_json LIKE '%TestGamma%'`, r.BuildID)
	if stored != 1 {
		t.Error("always-run membership was not frozen with the build")
	}
}

func TestCompletenessAndScopedGapsKeepCandidatesPending(t *testing.T) {
	runStateCases(t, []stateCase{
		{name: "incomplete execution", mods: []mod{func(p, c, d *spec) { c.State = "incomplete" }},
			state: "pending", reasons: map[string]string{"incomplete_execution": "pending"}},
		{name: "capped execution", mods: []mod{func(p, c, d *spec) { c.State = "capped" }},
			state: "pending", reasons: map[string]string{"incomplete_execution": "pending"}},
		{name: "incomplete run", mods: []mod{func(p, c, d *spec) { c.RunState = "incomplete" }},
			state: "pending", reasons: map[string]string{"incomplete_execution": "pending"}},
		{name: "another incomplete run at the same commit", state: "pending",
			extra:   func(f *fixture) { f.run("run-c1-other", "c1", "incomplete") },
			reasons: map[string]string{"incomplete_execution": "pending"}},
		{name: "incomplete suite", mods: []mod{func(p, c, d *spec) { c.Suite = true }},
			state: "pending", reasons: map[string]string{"incomplete_suite": "pending"}},
		{name: "truncated diagnostics are never strong", mods: []mod{func(p, c, d *spec) { c.Trunc = true }},
			state: "pending", reasons: map[string]string{"incomplete_diagnostics": "pending"}},
		{name: "truncated diagnostics with matched infra evidence stay excluded", mods: []mod{func(p, c, d *spec) { c.Trunc = true; c.Message = "no space left on device" }},
			state: "excluded", reasons: map[string]string{"infra": "exclude", "incomplete_diagnostics": "pending"}},
		{name: "unverified execution", mods: []mod{func(p, c, d *spec) { c.NoExec = true }},
			state: "pending", reasons: map[string]string{"unverified_execution": "pending"}},
		{name: "active gap scoped to the run", state: "pending",
			extra:   func(f *fixture) { f.gap("run-c1", "jobs", "jobs_unavailable", true) },
			reasons: map[string]string{"active_gap": "pending"}},
		{name: "active gap scoped to the unit", state: "pending",
			extra:   func(f *fixture) { f.gap("run-elsewhere", "unit-2", "parse_failed", true) },
			reasons: map[string]string{"active_gap": "pending"}},
		{name: "conflict in another unit of the same run", state: "pending",
			extra:   func(f *fixture) { f.gap("run-c1", "unit-99", "acquisition_conflict", true) },
			reasons: map[string]string{"active_gap": "pending"}},
		{name: "resolved gap does not block", state: "strong",
			extra: func(f *fixture) { f.gap("run-c1", "jobs", "jobs_unavailable", false) }},
		{name: "active gap of an unrelated run does not block", state: "strong",
			extra: func(f *fixture) { f.gap("run-elsewhere", "jobs", "jobs_unavailable", true) }},
		{name: "active discovery gap blocks strong attribution", state: "pending",
			extra:   func(f *fixture) { f.gap("", "runs", "discovery_capped", true) },
			reasons: map[string]string{"discovery_gap": "pending"}},
		{name: "candidate attempts on different branches cannot verify a branch", state: "pending",
			extra: func(f *fixture) {
				f.obs(spec{SHA: "c1", Run: "run-c1b", Status: "fail", Parents: ps("p1"), Branch: "release"})
			},
			reasons: map[string]string{"branch_unverified": "pending"}},
		{name: "blank observed branch cannot verify recovery", state: "pending",
			extra:   func(f *fixture) { f.exec(`UPDATE ci_artifacts SET branch='' WHERE unit_key='unit-2'`) },
			reasons: map[string]string{"branch_unverified": "pending", "no_passing_descendant": "weak"}},
	})
}

func TestCutoffIsExclusiveAndAppliesToEveryWitness(t *testing.T) {
	after := cutoff.Add(time.Hour)
	runStateCases(t, []stateCase{
		{name: "pass after the cutoff is not recovery", mods: []mod{func(p, c, d *spec) { d.At = after }},
			state: "weak", reasons: map[string]string{"no_passing_descendant": "weak"}},
		{name: "parent pass after the cutoff is not a witness", mods: []mod{func(p, c, d *spec) { p.At = after }},
			state: "pending", reasons: map[string]string{"missing_parent_observation": "pending"}},
		{name: "same-commit pass after the cutoff is not a flaky signal", state: "strong",
			extra: func(f *fixture) {
				f.obs(spec{SHA: "c1", Run: "run-c1-late", Parents: ps("p1"), Status: "pass", At: after})
			}},
		{name: "descendant pass exactly at the cutoff is excluded", mods: []mod{func(p, c, d *spec) { d.At = cutoff }},
			state: "weak", reasons: map[string]string{"no_passing_descendant": "weak"}},
		{name: "descendant pass just before the cutoff counts", mods: []mod{func(p, c, d *spec) { d.At = cutoff.Add(-time.Nanosecond) }}, state: "strong"},
	})
}

func TestFailuresAtOrAfterTheCutoffAreNotRawFailingCases(t *testing.T) {
	f := newFixture(t)
	f.obs(spec{SHA: "a1", Run: "r-a", Status: "fail", Parents: ps("a0"), At: cutoff.Add(-time.Nanosecond)})
	f.obs(spec{SHA: "b1", Run: "r-b", Status: "fail", Parents: ps("b0"), At: cutoff})
	f.obs(spec{SHA: "c1", Run: "r-c", Status: "fail", Parents: ps("c0"), At: cutoff.Add(time.Hour)})
	f.obs(spec{SHA: "n1", Run: "r-n", Status: "fail", NoSHA: true, At: cutoff})
	r := f.build()
	assertCounts(t, r, counts{raw: 1, grouped: 1, candidates: 1, pending: 1})
}
