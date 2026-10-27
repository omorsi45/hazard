package cli

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/omorsi45/hazard/internal/labels"
	"github.com/omorsi45/hazard/internal/store"
)

type labelsCalls struct {
	builds  []labels.Options
	stats   int
	dbPaths []string
	ctxVal  []any
}

type ctxKey struct{}

func sampleReport() labels.Report {
	return labels.Report{
		BuildID: strings.Repeat("a1", 32), Cutoff: "2026-01-01T00:00:00Z", AlgorithmVersion: "labels/1",
		InputDigest: strings.Repeat("b2", 32), DecisionDigest: strings.Repeat("c3", 32), Freshness: labels.FreshnessCurrent,
		RawFailingCases: 11, GroupedMemberCount: 7, UnresolvedRawMembers: 4, UnresolvedPending: 3, UnresolvedExcluded: 1,
		RawCandidates: 5, ExcludedCandidates: 2, PendingCandidates: 1, WeakCandidates: 1, StrongCandidates: 1,
		CandidateReasonCounts:         map[string]int{"flaky_same_commit": 2, "quarantined": 1, "no_passing_descendant": 1},
		UnresolvedReasonCounts:        map[string]int{"missing_environment": 3, "unstable_identity": 1},
		StrongCommitsByRepository:     map[string]int{"github/o/r": 1},
		AlwaysRunTests:                []string{"go:p:TestAlpha", "go:p:TestBeta", "go:p:TestGamma"},
		AcquisitionExclusionsByReason: map[string]int{"unverified_execution": 2},
		ActiveGaps:                    3,
		Limitations:                   []string{"first limitation text", "second limitation text"},
	}
}

func useLabelsBackend(t *testing.T, report labels.Report, buildErr, statsErr error) *labelsCalls {
	t.Helper()
	calls := &labelsCalls{}
	previous := newLabelsBackend
	newLabelsBackend = func() labelsBackend {
		return labelsBackend{
			build: func(ctx context.Context, dbPath string, opts labels.Options) (labels.Report, error) {
				calls.builds = append(calls.builds, opts)
				calls.dbPaths = append(calls.dbPaths, dbPath)
				calls.ctxVal = append(calls.ctxVal, ctx.Value(ctxKey{}))
				return report, buildErr
			},
			stats: func(_ context.Context, dbPath string) (labels.Report, error) {
				calls.stats++
				calls.dbPaths = append(calls.dbPaths, dbPath)
				return report, statsErr
			},
		}
	}
	t.Cleanup(func() { newLabelsBackend = previous })
	return calls
}

func TestLabelsRoutesToTheEngineWithTheFixedDatabase(t *testing.T) {
	calls := useLabelsBackend(t, sampleReport(), nil, nil)
	ctx := context.WithValue(context.Background(), ctxKey{}, "marker")
	for _, name := range []string{"build", "flaky"} {
		if code, _, errOut := invoke(ctx, "labels", name, "--before", "2026-01-02"); code != 0 || errOut != "" {
			t.Fatalf("labels %s: code=%d stderr=%q", name, code, errOut)
		}
	}
	want := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	if len(calls.builds) != 2 || calls.stats != 0 {
		t.Fatalf("build/flaky must share the build engine: builds=%d stats=%d", len(calls.builds), calls.stats)
	}
	for i, opts := range calls.builds {
		if !opts.Before.Equal(want) || opts.Before.Location() != time.UTC {
			t.Errorf("call %d cutoff = %v, want %v UTC", i, opts.Before, want)
		}
		if calls.ctxVal[i] != "marker" {
			t.Errorf("call %d did not receive the command context", i)
		}
	}
	if code, _, errOut := invoke(ctx, "labels", "stats"); code != 0 || errOut != "" {
		t.Fatalf("labels stats: code=%d stderr=%q", code, errOut)
	}
	if len(calls.builds) != 2 || calls.stats != 1 {
		t.Errorf("stats must not build: builds=%d stats=%d", len(calls.builds), calls.stats)
	}
	for _, path := range calls.dbPaths {
		if path != ".hazard/hazard.db" {
			t.Errorf("database path = %q, want .hazard/hazard.db", path)
		}
	}
}

func TestLabelsUsageErrorsExitTwoBeforeTheDatabaseIsTouched(t *testing.T) {
	calls := useLabelsBackend(t, sampleReport(), nil, nil)
	invalid := []string{"", "2026-1-1", "2026-13-01", "2026-02-30", "2026-01-01T00:00:00Z", "yesterday", " 2026-01-01", "2026-01-01 ", "20260101", "0000-00-00"}
	for _, name := range []string{"build", "flaky"} {
		cases := [][]string{{"labels", name}, {"labels", name, "--before", "2026-01-01", "extra"}}
		for _, v := range invalid {
			cases = append(cases, []string{"labels", name, "--before", v})
		}
		for _, args := range cases {
			code, out, errOut := invoke(context.Background(), args...)
			if code != 2 || out != "" || !strings.HasPrefix(errOut, "error:") {
				t.Errorf("%q: code=%d stdout=%q stderr=%q, want usage error 2", args, code, out, errOut)
			}
			jsonCode, jsonOut, jsonErr := invoke(context.Background(), append(args, "--json")...)
			var payload struct {
				Error string `json:"error"`
				Code  int    `json:"code"`
			}
			if err := json.Unmarshal([]byte(jsonErr), &payload); err != nil || jsonCode != 2 || jsonOut != "" || payload.Code != 2 || payload.Error == "" {
				t.Errorf("%q --json: code=%d stdout=%q stderr=%q (%v)", args, jsonCode, jsonOut, jsonErr, err)
			}
		}
	}
	for _, args := range [][]string{{"labels", "stats", "--before", "2026-01-01"}, {"labels", "stats", "extra"}, {"labels", "build", "--nope"}} {
		if code, out, _ := invoke(context.Background(), args...); code != 2 || out != "" {
			t.Errorf("%q: code=%d stdout=%q, want 2", args, code, out)
		}
	}
	if len(calls.builds) != 0 || calls.stats != 0 || len(calls.dbPaths) != 0 {
		t.Errorf("usage errors reached the engine: %+v", calls)
	}
}

func TestLabelsJSONEmitsTheReportOnStdoutOnly(t *testing.T) {
	want := sampleReport()
	useLabelsBackend(t, want, nil, nil)
	keys := []string{"build_id", "cutoff", "algorithm_version", "input_digest", "decision_digest", "freshness", "raw_failing_cases", "grouped_member_count",
		"unresolved_raw_members", "unresolved_pending", "unresolved_excluded", "raw_candidates", "excluded_candidates", "pending_candidates", "weak_candidates",
		"strong_candidates", "candidate_reason_counts", "unresolved_reason_counts", "strong_commits_by_repository", "always_run_tests",
		"acquisition_exclusions_by_reason", "active_gaps", "limitations"}
	for _, args := range [][]string{
		{"labels", "build", "--before", "2026-01-01", "--json"},
		{"--json", "labels", "flaky", "--before", "2026-01-01"},
		{"labels", "stats", "--json"},
	} {
		code, out, errOut := invoke(context.Background(), args...)
		if code != 0 || errOut != "" {
			t.Fatalf("%q: code=%d stderr=%q", args, code, errOut)
		}
		var got labels.Report
		if err := json.Unmarshal([]byte(out), &got); err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%q: report mismatch (%v):\n%s", args, err, out)
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(out), &raw); err != nil {
			t.Fatal(err)
		}
		for _, key := range keys {
			if _, ok := raw[key]; !ok {
				t.Errorf("%q: JSON omits %q", args, key)
			}
		}
		if len(raw) != len(keys) {
			t.Errorf("%q: JSON has %d keys, want %d", args, len(raw), len(keys))
		}
	}
}

func TestLabelsHumanOutputPrintsEveryCountAndLimitation(t *testing.T) {
	r := sampleReport()
	useLabelsBackend(t, r, nil, nil)
	common := []string{
		"build_id=" + r.BuildID, "freshness=current", "cutoff: " + r.Cutoff, "algorithm_version: labels/1", "input_digest: " + r.InputDigest, "decision_digest: " + r.DecisionDigest,
		"raw_failing_cases=11", "grouped_member_count=7", "unresolved_raw_members=4", "unresolved_pending=3", "unresolved_excluded=1",
		"raw_candidates=5", "excluded_candidates=2", "pending_candidates=1", "weak_candidates=1", "strong_candidates=1",
		"candidate_reason_counts (overlapping, not additive)", "flaky_same_commit=2", "quarantined=1", "no_passing_descendant=1",
		"unresolved_reason_counts (overlapping, not additive)", "missing_environment=3", "unstable_identity=1",
		"strong_commits_by_repository", "github/o/r=1", "always_run_tests (3)", "go:p:TestAlpha", "go:p:TestBeta", "go:p:TestGamma",
		"acquisition_exclusions_by_reason", "unverified_execution=2", "active_gaps=3", "first limitation text", "second limitation text",
	}
	for _, args := range [][]string{{"labels", "build", "--before", "2026-01-01"}, {"labels", "stats"}} {
		code, out, errOut := invoke(context.Background(), args...)
		if code != 0 || errOut != "" {
			t.Fatalf("%q: code=%d stderr=%q", args, code, errOut)
		}
		for _, want := range common {
			if !strings.Contains(out, want) {
				t.Errorf("%q: output omits %q:\n%s", args, want, out)
			}
		}
		if strings.ContainsRune(out, '\u2014') {
			t.Errorf("%q: output contains an em dash", args)
		}
	}
}

func TestLabelsFlakyPrintsTheFlakySubset(t *testing.T) {
	r := sampleReport()
	useLabelsBackend(t, r, nil, nil)
	code, out, errOut := invoke(context.Background(), "labels", "flaky", "--before", "2026-01-01")
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	for _, want := range []string{
		"labels flaky: build_id=" + r.BuildID, "decision_digest: " + r.DecisionDigest,
		"flaky_same_commit: candidates=2 unresolved=0", "unstable_identity: candidates=0 unresolved=1", "quarantined: candidates=1 unresolved=0",
		"raw_candidates=5", "excluded_candidates=2", "always_run_tests (3)", "go:p:TestBeta", "first limitation text",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("flaky output omits %q:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"strong_candidates", "weak_candidates", "no_passing_descendant"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("flaky output leaked non-flaky %q:\n%s", unwanted, out)
		}
	}
}

func TestLabelsUnavailableAndStaleAreLabeled(t *testing.T) {
	r := labels.Report{Freshness: labels.FreshnessUnavailable, Limitations: []string{"no current label build exists"},
		CandidateReasonCounts: map[string]int{}, UnresolvedReasonCounts: map[string]int{}, StrongCommitsByRepository: map[string]int{},
		AlwaysRunTests: []string{}, AcquisitionExclusionsByReason: map[string]int{}}
	useLabelsBackend(t, r, nil, nil)
	code, out, _ := invoke(context.Background(), "labels", "stats")
	if code != 0 || !strings.Contains(out, "freshness=unavailable") || !strings.Contains(out, "build_id=none") || !strings.Contains(out, "no current label build exists") {
		t.Errorf("unavailable output: code=%d\n%s", code, out)
	}
}

func TestLabelsRuntimeFailuresExitSeventy(t *testing.T) {
	useLabelsBackend(t, sampleReport(), errors.New("injected build failure"), errors.New("injected stats failure"))
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"labels", "build", "--before", "2026-01-01"}, "injected build failure"},
		{[]string{"labels", "flaky", "--before", "2026-01-01"}, "injected build failure"},
		{[]string{"labels", "stats"}, "injected stats failure"},
	} {
		code, out, errOut := invoke(context.Background(), tc.args...)
		if code != 70 || out != "" || !strings.Contains(errOut, tc.want) {
			t.Errorf("%q: code=%d stdout=%q stderr=%q", tc.args, code, out, errOut)
		}
		jsonCode, jsonOut, jsonErr := invoke(context.Background(), append(tc.args, "--json")...)
		var payload struct {
			Error string `json:"error"`
			Code  int    `json:"code"`
		}
		if err := json.Unmarshal([]byte(jsonErr), &payload); err != nil || jsonCode != 70 || jsonOut != "" || payload.Code != 70 || !strings.Contains(payload.Error, tc.want) {
			t.Errorf("%q --json: code=%d stdout=%q stderr=%q (%v)", tc.args, jsonCode, jsonOut, jsonErr, err)
		}
	}
}

func TestLabelsOutputWriteFailureExitsSeventy(t *testing.T) {
	useLabelsBackend(t, sampleReport(), nil, nil)
	for _, args := range [][]string{{"labels", "build", "--before", "2026-01-01"}, {"labels", "stats", "--json"}} {
		var errOut strings.Builder
		if code := Execute(context.Background(), args, failingWriter{}, &errOut); code != 70 {
			t.Errorf("%q: code=%d, want 70 (stderr %q)", args, code, errOut.String())
		}
	}
}

func TestLabelsHelpDocumentsTheCutoff(t *testing.T) {
	useLabelsBackend(t, sampleReport(), nil, nil)
	for _, name := range []string{"build", "flaky"} {
		code, out, errOut := invoke(context.Background(), "labels", name, "--help")
		if code != 0 || errOut != "" || !strings.Contains(out, "--before") || !strings.Contains(out, "YYYY-MM-DD") {
			t.Errorf("labels %s --help: code=%d err=%q\n%s", name, code, errOut, out)
		}
	}
}

// seedLabelsHistory creates .hazard/hazard.db in the working directory with one
// strong regression at c1: parent p1 passes, c1 fails, descendant d1 passes.
func seedLabelsHistory(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(labelsDBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	db := s.DB()
	stamp := "2025-06-01T00:00:00.000000000Z"
	mustExec(t, db, `INSERT INTO mining_sources(singleton,provider,project,observed_at) VALUES(1,'github','o/r',?)`, stamp)
	for i, c := range []struct{ sha, parents, status string }{{"p1", `["p0"]`, "pass"}, {"c1", `["p1"]`, "fail"}, {"d1", `["c1"]`, "pass"}} {
		unit := fmt.Sprintf("unit-%d", i+1)
		sum := sha256.Sum256([]byte(unit))
		content := hex.EncodeToString(sum[:])
		at := time.Date(2025, 6, 1+i, 0, 0, 0, 0, time.UTC).Format("2006-01-02T15:04:05.000000000Z")
		run := "run-" + c.sha
		mustExec(t, db, `INSERT INTO mining_runs(provider,project,run_id,attempt_key,attempt,source_head_sha,branch,state,observed_at) VALUES('github','o/r',?,'1',1,?,'main','complete',?)`, run, c.sha, stamp)
		mustExec(t, db, `INSERT INTO ci_artifacts(provider,project,unit_key,content_sha256,run_id,artifact_id,job_id,attempt,environment_id,tested_sha,tested_revision_evidence,
			execution_evidence,environment_evidence,raw_blob_path,source_head_sha,branch,tested_parent_shas_json,tested_kinds_json,suite_incomplete,diagnostics_truncated,state,observed_at)
			VALUES('github','o/r',?,?,?,?,?,1,'linux',?,'rev','exec','env','raw/x',?,'main',?,'null',0,0,'complete',?)`,
			unit, content, run, "artifact-"+unit, "job-"+unit, c.sha, c.sha, c.parents, at)
		// Mining keys every verified record to a canonical test_results row;
		// only such promoted passes can complete a comparison.
		key := struct {
			Source, Project, SHA, TestID, RunID, JobID, EnvironmentID, ArtifactID string
			Attempt                                                               int
		}{"github", "o/r", c.sha, "go:p:TestAlpha", run, "job-" + unit, "linux", "artifact-" + unit, 1}
		encoded, err := json.Marshal(key)
		if err != nil {
			t.Fatal(err)
		}
		mustExec(t, db, `INSERT INTO ci_observations(provider,project,unit_key,content_sha256,artifact_entry,entry_sha256,case_ordinal,raw_suite,raw_name,raw_classname,raw_file,status,
			raw_message,raw_type,raw_properties_json,suite_incomplete,diagnostics_truncated,test_id,identity_language,identity_package,identity_name,identity_file,identity_runner_id,
			unstable_id,identity_reason,identity_error,infra_reason,result_key,observed_at)
			VALUES('github','o/r',?,?,'report.xml',?,0,'s','n','c','f.go',?,'assertion','','{}',0,0,'go:p:TestAlpha','go','p','TestAlpha','f.go','',0,'','','',?,?)`,
			unit, content, content, c.status, string(encoded), at)
		mustExec(t, db, `INSERT INTO test_results(source,project,commit_sha,test_id,pipeline_id,job_id,environment_id,attempt,artifact_id,status,observed_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?)`, key.Source, key.Project, key.SHA, key.TestID, key.RunID, key.JobID, key.EnvironmentID, key.Attempt, key.ArtifactID, c.status, at)
	}
	return s
}

func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func runLabelsJSON(t *testing.T, args ...string) labels.Report {
	t.Helper()
	code, out, errOut := invoke(context.Background(), append(args, "--json")...)
	if code != 0 {
		t.Fatalf("%q: code=%d stderr=%q", args, code, errOut)
	}
	var r labels.Report
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("%q: %v\n%s", args, err, out)
	}
	return r
}

func TestLabelsEndToEndAgainstTheRealStore(t *testing.T) {
	t.Chdir(t.TempDir())
	s := seedLabelsHistory(t)

	if r := runLabelsJSON(t, "labels", "stats"); r.Freshness != labels.FreshnessUnavailable || r.StrongCandidates != 0 || r.BuildID != "" {
		t.Fatalf("stats before any build must be unavailable: %+v", r)
	}
	built := runLabelsJSON(t, "labels", "build", "--before", "2026-01-01")
	if built.Freshness != labels.FreshnessCurrent || built.RawFailingCases != 1 || built.RawCandidates != 1 || built.StrongCandidates != 1 ||
		built.StrongCommitsByRepository["github/o/r"] != 1 || built.Cutoff != "2026-01-01T00:00:00Z" {
		t.Fatalf("build report: %+v", built)
	}
	if stats := runLabelsJSON(t, "labels", "stats"); !reflect.DeepEqual(stats, built) {
		t.Errorf("stats differ from the build:\n%+v\n%+v", stats, built)
	}
	if flaky := runLabelsJSON(t, "labels", "flaky", "--before", "2026-01-01"); !reflect.DeepEqual(flaky, built) {
		t.Errorf("flaky must reuse the identical build:\n%+v\n%+v", flaky, built)
	}
	mustExec(t, s.DB(), `INSERT INTO tests(id,lang,package,name,file,first_seen,last_seen,quarantined) VALUES('go:p:TestAlpha','go','p','TestAlpha','f.go','2025-01-01T00:00:00Z','2025-01-01T00:00:00Z',1)`)
	stale := runLabelsJSON(t, "labels", "stats")
	if stale.Freshness != labels.FreshnessStale || stale.BuildID != built.BuildID || stale.StrongCandidates != 1 {
		t.Errorf("quarantine edit must make the build stale without rebuilding: %+v", stale)
	}
	if !reflect.DeepEqual(stale.AlwaysRunTests, []string{"go:p:TestAlpha"}) {
		t.Errorf("stale stats must still list the newly quarantined test: %v", stale.AlwaysRunTests)
	}
	rebuilt := runLabelsJSON(t, "labels", "build", "--before", "2026-01-01")
	if rebuilt.BuildID == built.BuildID || rebuilt.StrongCandidates != 0 || rebuilt.ExcludedCandidates != 1 || !reflect.DeepEqual(rebuilt.AlwaysRunTests, []string{"go:p:TestAlpha"}) {
		t.Errorf("rebuild after quarantine: %+v", rebuilt)
	}
	code, out, _ := invoke(context.Background(), "labels", "stats")
	if code != 0 || !strings.Contains(out, "build_id="+rebuilt.BuildID) || !strings.Contains(out, "freshness=current") {
		t.Errorf("human stats: code=%d\n%s", code, out)
	}
}

func TestLabelsCommandOrderConvergesThroughTheCLI(t *testing.T) {
	digests := func(order [][]string) labels.Report {
		t.Chdir(t.TempDir())
		seedLabelsHistory(t)
		for _, args := range order {
			runLabelsJSON(t, args...)
		}
		return runLabelsJSON(t, "labels", "stats")
	}
	build := []string{"labels", "build", "--before", "2026-01-01"}
	flaky := []string{"labels", "flaky", "--before", "2026-01-01"}
	a := digests([][]string{build, flaky, build})
	b := digests([][]string{flaky, build})
	if a.DecisionDigest != b.DecisionDigest || a.InputDigest != b.InputDigest || a.BuildID != b.BuildID || a.RawCandidates != b.RawCandidates {
		t.Errorf("build/flaky/build and flaky/build diverged:\n%+v\n%+v", a, b)
	}
}

func TestLabelsMissingDatabaseAndUsageErrorsLeaveNoFiles(t *testing.T) {
	t.Chdir(t.TempDir())
	if code, _, errOut := invoke(context.Background(), "labels", "stats"); code != 70 || errOut == "" {
		t.Errorf("stats without a database: code=%d stderr=%q, want 70", code, errOut)
	}
	if code, _, _ := invoke(context.Background(), "labels", "build"); code != 2 {
		t.Errorf("build without --before: code=%d, want 2", code)
	}
	if _, err := os.Stat(".hazard"); !os.IsNotExist(err) {
		t.Errorf("failed commands created .hazard: %v", err)
	}
	if code, _, errOut := invoke(context.Background(), "labels", "build", "--before", "2026-01-01"); code != 0 {
		t.Fatalf("build on a fresh directory: code=%d stderr=%q", code, errOut)
	}
	if info, err := os.Stat(labelsDBPath); err != nil || info.IsDir() {
		t.Errorf("build did not create %s: %v", labelsDBPath, err)
	}
	r := runLabelsJSON(t, "labels", "stats")
	if r.Freshness != labels.FreshnessCurrent || r.RawFailingCases != 0 || r.RawCandidates != 0 {
		t.Errorf("empty cohort stats: %+v", r)
	}
}
