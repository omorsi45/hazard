package labels

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func (f *fixture) stats() Report {
	f.t.Helper()
	r, err := ReadStats(context.Background(), f.db)
	if err != nil {
		f.t.Fatalf("ReadStats: %v", err)
	}
	return r
}

func (f *fixture) ledgerRows() [5]int {
	f.t.Helper()
	return [5]int{
		f.count(`SELECT count(*) FROM label_builds`), f.count(`SELECT count(*) FROM label_candidates`),
		f.count(`SELECT count(*) FROM label_candidate_members`), f.count(`SELECT count(*) FROM label_candidate_exclusions`),
		f.count(`SELECT count(*) FROM label_builds WHERE is_current=1`),
	}
}

func (f *fixture) currentBuild() string {
	f.t.Helper()
	var id string
	if err := f.db.QueryRow(`SELECT build_id FROM label_builds WHERE is_current=1`).Scan(&id); err != nil {
		f.t.Fatalf("no current build: %v", err)
	}
	return id
}

func TestRepeatedBuildIsANoOpWithTheSameBuildID(t *testing.T) {
	f := newFixture(t)
	f.standard()
	first := f.build()
	rows := f.ledgerRows()
	var created string
	if err := f.db.QueryRow(`SELECT created_at FROM label_builds`).Scan(&created); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		again := f.build()
		if !reflect.DeepEqual(again, first) {
			t.Fatalf("repeated build changed the report:\nfirst %+v\nagain %+v", first, again)
		}
	}
	if got := f.ledgerRows(); got != rows {
		t.Errorf("ledger rows %v after repeats, want unchanged %v", got, rows)
	}
	var createdAgain string
	if err := f.db.QueryRow(`SELECT created_at FROM label_builds`).Scan(&createdAgain); err != nil || createdAgain != created {
		t.Errorf("creation metadata changed: %q -> %q (%v)", created, createdAgain, err)
	}
}

func TestRevertedInputsReuseTheEarlierBuild(t *testing.T) {
	f := newFixture(t)
	f.standard()
	a := f.build()
	f.flag(testAlpha, "quarantined")
	b := f.build()
	if a.BuildID == b.BuildID || f.currentBuild() != b.BuildID {
		t.Fatalf("quarantine edit must create a new current build: %s vs %s", a.BuildID, b.BuildID)
	}
	f.exec(`UPDATE tests SET quarantined=0 WHERE id=?`, testAlpha)
	back := f.build()
	if back.BuildID != a.BuildID || f.currentBuild() != a.BuildID {
		t.Errorf("reverted inputs must reuse the first build: got %s want %s", back.BuildID, a.BuildID)
	}
	if rows := f.ledgerRows(); rows[0] != 2 || rows[4] != 1 {
		t.Errorf("builds/current = %d/%d, want 2/1", rows[0], rows[4])
	}
	if f.candidate(b.BuildID, "c1", testAlpha, "linux").State != "excluded" {
		t.Error("prior build's frozen decisions were rewritten")
	}
}

func TestDifferentCutoffsAreDifferentBuilds(t *testing.T) {
	f := newFixture(t)
	f.standard()
	a := f.build()
	other, err := Build(context.Background(), f.db, Options{Before: cutoff.AddDate(0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	if other.BuildID == a.BuildID || other.Cutoff != "2026-01-02T00:00:00Z" || f.currentBuild() != other.BuildID {
		t.Errorf("cutoff must be part of the build identity: %+v", other)
	}
}

func TestBuildRejectsMissingCutoff(t *testing.T) {
	f := newFixture(t)
	if _, err := Build(context.Background(), f.db, Options{}); err == nil {
		t.Fatal("zero cutoff accepted")
	}
	if f.ledgerRows() != [5]int{} {
		t.Error("rejected build wrote rows")
	}
}

func TestBuildRejectsACutoffThatIsNotAUTCMidnight(t *testing.T) {
	f := newFixture(t)
	// A closed database fails any statement, so only validation that runs before
	// the database is touched can produce the midnight error.
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	zone := time.FixedZone("EST", -5*3600)
	for name, before := range map[string]time.Time{
		"one nanosecond past midnight":       cutoff.Add(time.Nanosecond),
		"half a second past midnight":        cutoff.Add(500 * time.Millisecond),
		"one second past midnight":           cutoff.Add(time.Second),
		"one minute past midnight":           cutoff.Add(time.Minute),
		"noon":                               cutoff.Add(12 * time.Hour),
		"midnight in a zone west of UTC":     time.Date(2026, 1, 1, 0, 0, 0, 0, zone),
		"midnight UTC minus one nanosecond":  cutoff.Add(-time.Nanosecond),
		"a non-midnight instant in the past": time.Date(2025, 12, 31, 13, 30, 0, 0, time.UTC),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Build(context.Background(), f.db, Options{Before: before})
			if err == nil || !strings.Contains(err.Error(), "UTC midnight") {
				t.Errorf("Build error = %v, want a UTC midnight validation error", err)
			}
		})
	}
}

func TestBuildAcceptsAnyRepresentationOfUTCMidnight(t *testing.T) {
	f := newFixture(t)
	f.standard()
	zone := time.FixedZone("EST", -5*3600)
	local := time.Date(2025, 12, 31, 19, 0, 0, 0, zone) // 2026-01-01T00:00:00Z
	r, err := Build(context.Background(), f.db, Options{Before: local})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if r.Cutoff != "2026-01-01T00:00:00Z" || r.BuildID != f.build().BuildID {
		t.Errorf("cutoff = %q, want the same build as the UTC midnight", r.Cutoff)
	}
}

func TestRequestedWindowsEnterTheDigestAsADistinctSet(t *testing.T) {
	const (
		jun, jul, aug = "2025-06-01T00:00:00Z", "2025-07-01T00:00:00Z", "2025-08-01T00:00:00Z"
		widened       = "2025-05-01T00:00:00Z"
	)
	setWindow := func(f *fixture, run, since, until string) {
		f.exec(`UPDATE mining_runs SET requested_since=?, requested_until=? WHERE run_id=?`, since, until, run)
	}
	windows := func(f *fixture) int {
		facts, err := loadFacts(context.Background(), f.db, cutoff)
		if err != nil {
			f.t.Fatal(err)
		}
		return len(facts.Windows)
	}
	t.Run("a widened window changes the digest and the build", func(t *testing.T) {
		f := newFixture(t)
		f.standard()
		for _, run := range []string{"run-p1", "run-c1", "run-d1"} {
			setWindow(f, run, jun, jul)
		}
		narrow := f.build()
		for _, run := range []string{"run-p1", "run-c1", "run-d1"} {
			setWindow(f, run, widened, jul)
		}
		if got := f.stats(); got.Freshness != FreshnessStale || got.BuildID != narrow.BuildID {
			t.Errorf("widened window: stats = %s on %s, want stale on %s", got.Freshness, got.BuildID, narrow.BuildID)
		}
		wide := f.build()
		if wide.InputDigest == narrow.InputDigest || wide.BuildID == narrow.BuildID {
			t.Error("a widened acquisition window did not change the input digest")
		}
	})
	t.Run("the same window on every attempt is one distinct window", func(t *testing.T) {
		f := newFixture(t)
		f.standard()
		for _, run := range []string{"run-p1", "run-c1", "run-d1"} {
			setWindow(f, run, jun, jul)
		}
		if got := windows(f); got != 1 {
			t.Errorf("distinct windows = %d, want 1", got)
		}
		setWindow(f, "run-d1", jul, aug)
		if got := windows(f); got != 2 {
			t.Errorf("distinct windows = %d, want 2", got)
		}
	})
	t.Run("only the set matters, not which attempt carries which window", func(t *testing.T) {
		a, b := newFixture(t), newFixture(t)
		a.standard()
		b.standard()
		setWindow(a, "run-p1", jun, jul)
		setWindow(a, "run-d1", jul, aug)
		setWindow(b, "run-p1", jul, aug)
		setWindow(b, "run-d1", jun, jul)
		if x, y := a.build(), b.build(); x.InputDigest != y.InputDigest {
			t.Error("swapping windows between attempts changed the digest")
		}
	})
	t.Run("an absent window differs from an empty one", func(t *testing.T) {
		f := newFixture(t)
		f.standard()
		absent := f.build()
		for _, run := range []string{"run-p1", "run-c1", "run-d1"} {
			setWindow(f, run, "", "")
		}
		if again := f.build(); again.InputDigest == absent.InputDigest {
			t.Error("NULL and empty windows digest identically")
		}
	})
}

func TestReportCarriesAcquisitionAccounting(t *testing.T) {
	f := newFixture(t)
	f.standard()
	f.exclusion("unit-1", "unverified_execution")
	f.exclusion("unit-2", "unverified_execution")
	f.exclusion("unit-3", "unsupported_status")
	f.gap("run-c1", "jobs", "jobs_unavailable", true)
	f.gap("run-p1", "jobs", "jobs_unavailable", false)
	f.gap("", "runs", "discovery_capped", true)
	r := f.build()
	if !reflect.DeepEqual(r.AcquisitionExclusionsByReason, map[string]int{"unverified_execution": 2, "unsupported_status": 1}) {
		t.Errorf("acquisition exclusions = %v", r.AcquisitionExclusionsByReason)
	}
	if r.ActiveGaps != 2 {
		t.Errorf("active gaps = %d, want 2", r.ActiveGaps)
	}
	if got := f.stats(); !reflect.DeepEqual(got, r) {
		t.Errorf("stats differ from the build report:\nbuild %+v\nstats %+v", r, got)
	}
}

func TestLimitationsStateWhatTheCountsDoNotProve(t *testing.T) {
	f := newFixture(t)
	f.obs(spec{SHA: "x1", Run: "r-x", Status: "fail", NoSHA: true})
	f.gap("", "runs", "discovery_capped", true)
	r := f.build()
	joined := strings.Join(r.Limitations, "\n")
	for _, want := range []string{"not evidence of no regressions", "active acquisition gap", "never calibration units", "no strong candidates"} {
		if !strings.Contains(joined, want) {
			t.Errorf("limitations omit %q:\n%s", want, joined)
		}
	}
	for _, text := range r.Limitations {
		if strings.ContainsRune(text, '\u2014') {
			t.Errorf("limitation contains an em dash: %q", text)
		}
	}
}

func TestInputDigestTracksClassificationInputsOnly(t *testing.T) {
	after := cutoff.Add(24 * time.Hour)
	cases := []struct {
		name      string
		mutate    func(f *fixture)
		sensitive bool
	}{
		{"passing comparison observation flips", func(f *fixture) { f.exec(`UPDATE ci_observations SET status='skip' WHERE unit_key='unit-1'`) }, true},
		{"another passing comparison observation", func(f *fixture) { f.obs(spec{SHA: "p1", Run: "run-p1b", Status: "pass"}) }, true},
		{"manual quarantine", func(f *fixture) { f.flag(testAlpha, "quarantined") }, true},
		{"manual unstable annotation", func(f *fixture) { f.flag(testAlpha, "unstable_id") }, true},
		{"canonical promotion of a witness removed", func(f *fixture) { f.unpromote("unit-1") }, true},
		{"provenance enrichment", func(f *fixture) {
			f.exec(`UPDATE ci_artifacts SET environment_evidence='stronger proof' WHERE unit_key='unit-2'`)
		}, true},
		{"parent facts", func(f *fixture) {
			f.exec(`UPDATE ci_artifacts SET tested_parent_shas_json='["p1","p2"]' WHERE unit_key='unit-2'`)
		}, true},
		{"checkpoint state", func(f *fixture) { f.setRunState("run-c1", "incomplete") }, true},
		{"gap appears", func(f *fixture) { f.gap("run-c1", "jobs", "jobs_unavailable", true) }, true},
		{"acquisition exclusion appears", func(f *fixture) { f.exclusion("unit-2", "unsupported_status") }, true},
		{"diagnostic content", func(f *fixture) {
			f.exec(`UPDATE ci_observations SET raw_message='different text' WHERE unit_key='unit-2'`)
		}, true},
		{"mining_runs.observed_at", func(f *fixture) { f.exec(`UPDATE mining_runs SET observed_at='2030-01-01T00:00:00.000000000Z'`) }, false},
		{"mining_sources.observed_at", func(f *fixture) { f.exec(`UPDATE mining_sources SET observed_at='2030-01-01T00:00:00.000000000Z'`) }, false},
		{"gap first and last seen times", func(f *fixture) {
			f.exec(`UPDATE acquisition_gaps SET first_acquired_at='2031-01-01T00:00:00.000000000Z', last_acquired_at='2032-01-01T00:00:00.000000000Z'`)
		}, false},
		{"replay counters and raw paths", func(f *fixture) {
			f.exec(`UPDATE ci_artifacts SET replay_cases=replay_cases+7, raw_blob_path='elsewhere/blob'`)
		}, false},
		{"inventory bookkeeping without flags", func(f *fixture) {
			f.exec(`INSERT INTO tests(id,lang,package,name,file,first_seen,last_seen,runs,fails,flaky_score) VALUES('go:p:Other','go','p','O','o.go','2025-01-01T00:00:00Z','2025-01-01T00:00:00Z',9,9,0.9)`)
		}, false},
		{"observation after the cutoff", func(f *fixture) { f.obs(spec{SHA: "c9", Run: "run-c1", Status: "fail", At: after}) }, false},
		{"acquisition exclusion of a report after the cutoff", func(f *fixture) {
			unit := f.obs(spec{SHA: "c9", Run: "run-c1", Status: "fail", At: after})
			f.exclusion(unit, "unsupported_status")
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.standard()
			f.gap("run-elsewhere", "jobs", "jobs_unavailable", true)
			base := f.build()
			tc.mutate(f)
			got := f.stats()
			wantFreshness := FreshnessCurrent
			if tc.sensitive {
				wantFreshness = FreshnessStale
			}
			if got.Freshness != wantFreshness || got.BuildID != base.BuildID {
				t.Errorf("stats freshness=%s build=%s, want %s on prior build %s", got.Freshness, got.BuildID, wantFreshness, base.BuildID)
			}
			again := f.build()
			if (again.InputDigest != base.InputDigest) != tc.sensitive {
				t.Errorf("input digest changed=%t, want %t", again.InputDigest != base.InputDigest, tc.sensitive)
			}
			if tc.sensitive && again.BuildID == base.BuildID {
				t.Error("changed input reused the old build")
			}
			if !tc.sensitive && again.BuildID != base.BuildID {
				t.Error("unchanged input created a new build")
			}
		})
	}
}

func TestResolvingAGapChangesTheInputDigest(t *testing.T) {
	f := newFixture(t)
	f.standard()
	f.gap("run-c1", "jobs", "jobs_unavailable", true)
	active := f.build()
	f.exec(`UPDATE acquisition_gaps SET resolved_at='2030-01-01T00:00:00.000000000Z'`)
	resolved := f.build()
	if active.InputDigest == resolved.InputDigest || resolved.StrongCandidates != 1 || active.PendingCandidates != 1 {
		t.Errorf("resolution must change the digest and the decision: active=%+v resolved=%+v", active, resolved)
	}
}

func TestCommandOrderConvergesOnTheSameDecisions(t *testing.T) {
	history := func(f *fixture) {
		f.standard()
		f.obs(spec{SHA: "m1", Run: "run-m1", Test: testBeta, Status: "fail", Parents: ps("m0")})
		f.obs(spec{SHA: "m1", Run: "run-m1p", Test: testBeta, Status: "pass", Parents: ps("m0")})
		f.obs(spec{SHA: "u1", Run: "run-u1", Status: "fail", NoEnv: true})
	}
	one := newFixture(t) // build, flaky, build
	history(one)
	for range 3 {
		one.build()
	}
	two := newFixture(t) // flaky, build
	history(two)
	for range 2 {
		two.build()
	}
	a, b := one.stats(), two.stats()
	if a.DecisionDigest != b.DecisionDigest || a.InputDigest != b.InputDigest || a.BuildID != b.BuildID || !reflect.DeepEqual(a, b) {
		t.Errorf("command order changed the result:\n%+v\n%+v", a, b)
	}
	if a.RawCandidates != 2 || a.RawFailingCases != 3 || a.UnresolvedRawMembers != 1 || a.ExcludedCandidates != 1 {
		t.Errorf("raw counts changed by the exclusion pass: %+v", a)
	}
}

func TestReadStatsReportsUnavailableWithoutABuild(t *testing.T) {
	f := newFixture(t)
	f.standard()
	f.gap("run-c1", "jobs", "jobs_unavailable", true)
	f.exclusion("unit-2", "unsupported_status")
	r := f.stats()
	if r.Freshness != FreshnessUnavailable || r.BuildID != "" || r.RawFailingCases != 0 || r.StrongCandidates != 0 {
		t.Errorf("missing build must be unavailable, not zero eligibility: %+v", r)
	}
	if !strings.Contains(strings.Join(r.Limitations, "\n"), "unavailable") {
		t.Errorf("limitations do not say eligibility is unavailable: %v", r.Limitations)
	}
	if r.ActiveGaps != 1 || r.AcquisitionExclusionsByReason["unsupported_status"] != 1 {
		t.Errorf("live acquisition facts missing from the unavailable report: %+v", r)
	}
	if f.ledgerRows() != [5]int{} {
		t.Error("ReadStats created label rows")
	}
	if r.CandidateReasonCounts == nil || r.AlwaysRunTests == nil || r.Limitations == nil {
		t.Error("collections must be non-nil for JSON")
	}
}

func TestUnavailableStatsStillReportManuallyFlaggedAlwaysRunTests(t *testing.T) {
	f := newFixture(t)
	f.standard()
	f.flag(testBeta, "quarantined")
	f.flag(testAlpha, "unstable_id")
	f.flag(testAlpha, "quarantined") // both flags on one test list it once
	r := f.stats()
	if r.Freshness != FreshnessUnavailable || r.BuildID != "" {
		t.Fatalf("no build must report unavailable: %+v", r)
	}
	if want := []string{testAlpha, testBeta}; !reflect.DeepEqual(r.AlwaysRunTests, want) {
		t.Errorf("always_run_tests = %v, want %v (an empty list would read as no test must run)", r.AlwaysRunTests, want)
	}
	if f.ledgerRows() != [5]int{} {
		t.Error("ReadStats created label rows")
	}
}

func TestStaleStatsListNewlyFlaggedTestsBesideTheFrozenSet(t *testing.T) {
	f := newFixture(t)
	f.standard()
	f.obs(spec{SHA: "c1", Run: "run-c1-pass", Parents: ps("p1"), Status: "pass"}) // TestAlpha is flaky at c1
	f.flag(testBeta, "unstable_id")
	built := f.build()
	if want := []string{testAlpha, testBeta}; !reflect.DeepEqual(built.AlwaysRunTests, want) {
		t.Fatalf("built always_run_tests = %v, want %v", built.AlwaysRunTests, want)
	}
	const gamma = "go:p:TestGamma"
	f.flag(gamma, "quarantined") // the edit that makes the build stale
	f.exec(`UPDATE tests SET quarantined=0, unstable_id=0 WHERE id=?`, testBeta)
	before := f.ledgerRows()
	r := f.stats()
	if r.Freshness != FreshnessStale || r.BuildID != built.BuildID {
		t.Fatalf("stats = %s on %s, want stale on %s", r.Freshness, r.BuildID, built.BuildID)
	}
	// Frozen: Alpha (flaky) and Beta. Live: Gamma. The union keeps all three, sorted.
	if want := []string{testAlpha, testBeta, gamma}; !reflect.DeepEqual(r.AlwaysRunTests, want) {
		t.Errorf("stale always_run_tests = %v, want %v", r.AlwaysRunTests, want)
	}
	if f.ledgerRows() != before {
		t.Error("ReadStats wrote rows")
	}
	var frozen string
	if err := f.db.QueryRow(`SELECT always_run_json FROM label_builds WHERE build_id=?`, built.BuildID).Scan(&frozen); err != nil || strings.Contains(frozen, "TestGamma") {
		t.Errorf("stale stats must not rewrite the frozen set: %q (%v)", frozen, err)
	}
}

func TestStaleStatsListANewQuarantineOfAnOtherwiseUnlistedTest(t *testing.T) {
	f := newFixture(t)
	f.standard()
	built := f.build()
	if len(built.AlwaysRunTests) != 0 {
		t.Fatalf("built always_run_tests = %v, want none", built.AlwaysRunTests)
	}
	f.flag(testAlpha, "quarantined")
	r := f.stats()
	if r.Freshness != FreshnessStale || !reflect.DeepEqual(r.AlwaysRunTests, []string{testAlpha}) {
		t.Errorf("stale stats after a quarantine edit: freshness=%s always_run=%v, want stale [%s]", r.Freshness, r.AlwaysRunTests, testAlpha)
	}
}

func TestReadStatsNeverWrites(t *testing.T) {
	f := newFixture(t)
	f.standard()
	f.build()
	f.flag(testAlpha, "quarantined")
	before := f.ledgerRows()
	r := f.stats()
	if r.Freshness != FreshnessStale {
		t.Fatalf("freshness = %s, want stale", r.Freshness)
	}
	if f.ledgerRows() != before {
		t.Error("ReadStats rebuilt or rewrote the ledger")
	}
	if r.StrongCandidates != 1 {
		t.Errorf("stale stats must keep the prior frozen counts: %+v", r)
	}
}

func TestStaleWhenTheAlgorithmVersionChanges(t *testing.T) {
	f := newFixture(t)
	f.standard()
	r := f.build()
	f.exec(`DROP TRIGGER label_builds_no_identity_update`)
	f.exec(`UPDATE label_builds SET algorithm_version='labels/0' WHERE build_id=?`, r.BuildID)
	if got := f.stats(); got.Freshness != FreshnessStale {
		t.Errorf("freshness = %s, want stale for an older algorithm version", got.Freshness)
	}
}

func TestFlakyExclusionAccountingKeepsRawCountsAndShrinksTheEligibleDenominator(t *testing.T) {
	f := newFixture(t)
	f.standard() // genuine strong regression at c1
	// A known missed candidate at m1: it would be strong, but a pass at the same
	// commit in the same environment makes it flaky.
	f.obs(spec{SHA: "m0", Run: "run-m0", Test: testBeta, Status: "pass", Parents: ps("m00")})
	f.obs(spec{SHA: "m1", Run: "run-m1", Test: testBeta, Status: "fail", Parents: ps("m0")})
	f.obs(spec{SHA: "m1", Run: "run-m1p", Test: testBeta, Status: "pass", Parents: ps("m0")})
	f.obs(spec{SHA: "m2", Run: "run-m2", Test: testBeta, Status: "pass", Parents: ps("m1")})
	f.build()
	r := f.stats()
	assertCounts(t, r, counts{raw: 2, grouped: 2, candidates: 2, excluded: 1, strong: 1})
	if r.CandidateReasonCounts["flaky_same_commit"] != 1 {
		t.Errorf("stats hid the exclusion: %v", r.CandidateReasonCounts)
	}
	if r.StrongCommitsByRepository[repoKey] != 1 {
		t.Errorf("eligible denominator = %d, want 1 (excluded candidate must not count)", r.StrongCommitsByRepository[repoKey])
	}
	if !reflect.DeepEqual(r.AlwaysRunTests, []string{testBeta}) {
		t.Errorf("excluded flaky test must always run: %v", r.AlwaysRunTests)
	}
	if got := f.candidate(r.BuildID, "m1", testBeta, "linux"); got.State != "excluded" {
		t.Errorf("flaky candidate state = %s", got.State)
	}
}

func TestStrongCommitsAreDistinctTestedRevisionsPerRepository(t *testing.T) {
	f := newFixture(t)
	f.standard()
	// A second strong test at the same tested revision c1 does not add a commit.
	f.obs(spec{SHA: "p1", Run: "run-p1b", Test: testBeta, Status: "pass", Parents: ps("p0")})
	f.obs(spec{SHA: "c1", Run: "run-c1b", Test: testBeta, Status: "fail", Parents: ps("p1")})
	f.obs(spec{SHA: "d1", Run: "run-d1b", Test: testBeta, Status: "pass", Parents: ps("c1")})
	r := f.build()
	if r.StrongCandidates != 2 || r.StrongCommitsByRepository[repoKey] != 1 {
		t.Errorf("strong=%d commits=%v, want 2 candidates on 1 commit", r.StrongCandidates, r.StrongCommitsByRepository)
	}
}

func TestPublicationFailureLeavesThePriorCurrentBuildIntact(t *testing.T) {
	f := newFixture(t)
	f.standard()
	prior := f.build()
	before := f.ledgerRows()
	f.flag(testAlpha, "quarantined") // semantic input change, so a new build is attempted
	f.exec(`CREATE TRIGGER inject_publication_failure BEFORE INSERT ON label_candidate_members
		BEGIN SELECT RAISE(ABORT, 'injected publication failure'); END`)
	_, err := Build(context.Background(), f.db, Options{Before: cutoff})
	if err == nil || !strings.Contains(err.Error(), "injected publication failure") {
		t.Fatalf("Build error = %v, want the injected failure", err)
	}
	if f.ledgerRows() != before {
		t.Errorf("failed publication left rows behind: %v, want %v", f.ledgerRows(), before)
	}
	if f.currentBuild() != prior.BuildID {
		t.Error("failed publication moved the current marker")
	}
	got := f.stats()
	if got.BuildID != prior.BuildID || got.Freshness != FreshnessStale {
		t.Errorf("stats after failed publication: %+v", got)
	}
	f.exec(`DROP TRIGGER inject_publication_failure`)
	if next := f.build(); next.BuildID == prior.BuildID || next.Freshness != FreshnessCurrent {
		t.Errorf("rebuild after the failure did not publish: %+v", next)
	}
}

func TestCanceledContextPublishesNothing(t *testing.T) {
	f := newFixture(t)
	f.standard()
	prior := f.build()
	f.flag(testAlpha, "quarantined")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Build(ctx, f.db, Options{Before: cutoff}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Build error = %v, want context.Canceled", err)
	}
	if f.currentBuild() != prior.BuildID || f.count(`SELECT count(*) FROM label_builds`) != 1 {
		t.Error("canceled build changed the ledger")
	}
	if _, err := ReadStats(ctx, f.db); !errors.Is(err, context.Canceled) {
		t.Errorf("ReadStats error = %v, want context.Canceled", err)
	}
}
