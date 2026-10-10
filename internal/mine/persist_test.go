package mine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omorsi45/hazard/internal/runner"
	"github.com/omorsi45/hazard/internal/store"
)

func TestMiningMigrationUpgradesExistingStoreAndReopens(t *testing.T) {
	file := filepath.Join(t.TempDir(), "hazard.db")
	db, err := sql.Open("sqlite", file)
	if err != nil {
		t.Fatal(err)
	}
	schema, err := os.ReadFile("../store/migrations/0001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL);
		INSERT INTO schema_migrations VALUES(1, '2026-09-29T00:00:00Z');
		INSERT INTO tests(id,lang,package,name,file,first_seen,last_seen)
		VALUES('go:p:T','go','p','T','t.go','2026-09-29T00:00:00Z','2026-09-29T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		s, err := store.Open(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"mining_sources", "mining_runs", "ci_artifacts", "ci_observations", "acquisition_exclusions"} {
			var count int
			if err := s.DB().QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", name).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 1 {
				t.Errorf("migration table %s count = %d, want 1", name, count)
			}
		}
		var tests, versions int
		if err := s.DB().QueryRow("SELECT count(*) FROM tests").Scan(&tests); err != nil {
			t.Fatal(err)
		}
		if err := s.DB().QueryRow("SELECT count(*) FROM schema_migrations").Scan(&versions); err != nil {
			t.Fatal(err)
		}
		if tests != 1 || versions != 2 {
			t.Errorf("preserved tests/migrations = %d/%d, want 1/2", tests, versions)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func persistDB(t *testing.T) *sql.DB {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "hazard.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s.DB()
}

func persistPtr[T any](v T) *T { return &v }

func persistFixture() ProviderObservation {
	observed := time.Date(2026, 10, 1, 9, 0, 0, 0, time.FixedZone("EDT", -4*3600))
	authored := observed.Add(-time.Hour)
	return ProviderObservation{
		Provider: "github", Project: "owner/repo", RunID: "run-1", UnitKey: "artifact-1",
		ContentSHA256: strings.Repeat("a", 64), RawBlobPath: ".hazard/raw/aaaaaaaa", State: "complete",
		SourceHeadSHA: "source-head", Branch: "main", ArtifactID: persistPtr("artifact-1"),
		JobID: persistPtr("job-1"), Attempt: persistPtr(1), RunAttempt: persistPtr(1),
		EnvironmentID: persistPtr("go126-linux"), TestedSHA: persistPtr("tested-merge"),
		TestedRevisionEvidence: persistPtr("verified checkout SHA"), ExecutionEvidence: persistPtr("verified artifact producer"),
		EnvironmentEvidence: persistPtr("verified runtime and build configuration"), ObservedAt: observed,
		RequestedSince: persistPtr(observed.Add(-24 * time.Hour)), RequestedUntil: persistPtr(observed.Add(time.Hour)),
		Commit: &CommitMetadata{AuthoredAt: &authored, ParentSHAs: []string{"parent-a", "parent-b"},
			FilesChanged: persistPtr(2), LinesAdded: persistPtr(3), LinesRemoved: persistPtr(4), Kinds: map[string]int{"source": 2}},
		Entries: []ObservationEntry{{Name: "reports/junit.xml", SHA256: strings.Repeat("b", 64), Cases: []ObservationCase{{
			Case: runner.JUnitCase{Suite: "suite", Name: "TestT", Classname: "example.org/p", File: "raw_test.go", Status: "fail",
				DurationMS: persistPtr(int64(123)), Message: "assertion failed", Type: "AssertionError", Properties: map[string]string{"runner": "go"}},
			Identity: runner.TestIdentity{ID: "go:example.org/p:TestT", Language: "go", Package: "example.org/p", Name: "TestT", File: "identity_test.go", RunnerID: "TestT"},
		}}}},
	}
}

func persistCount(t *testing.T, db *sql.DB, table string, want int) {
	t.Helper()
	var got int
	if err := db.QueryRow("SELECT count(*) FROM " + table).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s rows = %d, want %d", table, got, want)
	}
}

func persistOK(t *testing.T, db *sql.DB, obs ProviderObservation) {
	t.Helper()
	if err := persistObservation(context.Background(), db, obs); err != nil {
		t.Fatal(err)
	}
}

func TestPersistVerifiedFactualMapping(t *testing.T) {
	db := persistDB(t)
	obs := persistFixture()
	obs.Entries[0].Cases[0].Case.SuiteIncomplete = true
	obs.Entries[0].Cases[0].Case.DiagnosticsTruncated = true
	obs.Entries[0].Cases[0].IdentityEvidence = "operator language go; tested module example.org/p"
	persistOK(t, db, obs)
	for _, table := range []string{"mining_sources", "mining_runs", "ci_artifacts", "ci_observations", "tests", "commits", "test_results"} {
		persistCount(t, db, table, 1)
	}
	var sha, pipeline, job, env, artifact, path, observed, status, rawName, rawClass, message string
	var duration, attempt int
	if err := db.QueryRow(`SELECT commit_sha,pipeline_id,job_id,environment_id,artifact_id,artifact_path,observed_at,status,duration_ms,attempt,raw_name,raw_classname,raw_message FROM test_results`).Scan(
		&sha, &pipeline, &job, &env, &artifact, &path, &observed, &status, &duration, &attempt, &rawName, &rawClass, &message); err != nil {
		t.Fatal(err)
	}
	if sha != "tested-merge" || pipeline != "run-1" || job != "job-1" || env != "go126-linux" || artifact != "artifact-1" || path != "reports/junit.xml" ||
		observed != "2026-10-01T13:00:00.000000000Z" || status != "fail" || duration != 123 || attempt != 1 || rawName != "TestT" || rawClass != "example.org/p" || message != "assertion failed" {
		t.Fatal("factual result mapping lost tested revision, exact execution, raw fields or UTC time")
	}
	var file, properties, rawFile, runnerID string
	var incomplete, truncated, runs, fails, quarantined int
	var median sql.NullInt64
	if err := db.QueryRow("SELECT file,runs,fails,median_ms,quarantined FROM tests").Scan(&file, &runs, &fails, &median, &quarantined); err != nil {
		t.Fatal(err)
	}
	if file != "identity_test.go" || runs != 0 || fails != 0 || median.Valid || quarantined != 0 {
		t.Fatal("ingestion fabricated derived test fields")
	}
	if err := db.QueryRow("SELECT raw_file,raw_properties_json,identity_runner_id,suite_incomplete,diagnostics_truncated FROM ci_observations").Scan(&rawFile, &properties, &runnerID, &incomplete, &truncated); err != nil {
		t.Fatal(err)
	}
	if rawFile != "raw_test.go" || properties != `{"runner":"go"}` || runnerID != "TestT" || incomplete != 1 || truncated != 1 {
		t.Fatal("raw evidence/completeness was not preserved")
	}
	var identityEvidence string
	if err := db.QueryRow("SELECT identity_evidence FROM ci_observations").Scan(&identityEvidence); err != nil {
		t.Fatal(err)
	}
	if identityEvidence != "operator language go; tested module example.org/p" {
		t.Fatal("language/tested-module identity evidence was lost")
	}
}

func TestPersistUnverifiedProvenanceRemainsRaw(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*ProviderObservation)
	}{
		{"tested_sha", func(o *ProviderObservation) { o.TestedSHA = nil }},
		{"revision_evidence", func(o *ProviderObservation) { o.TestedRevisionEvidence = nil }},
		{"job", func(o *ProviderObservation) { o.JobID = nil }},
		{"execution_evidence", func(o *ProviderObservation) { o.ExecutionEvidence = nil }},
		{"github_attempt", func(o *ProviderObservation) { o.Attempt = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := persistDB(t)
			obs := persistFixture()
			tc.change(&obs)
			persistOK(t, db, obs)
			persistCount(t, db, "ci_observations", 1)
			persistCount(t, db, "test_results", 0)
			persistCount(t, db, "tests", 0)
			persistCount(t, db, "commits", 0)
			persistCount(t, db, "acquisition_exclusions", 1)
		})
	}
}

func TestPersistUnknownEnvironmentIsFactual(t *testing.T) {
	db := persistDB(t)
	obs := persistFixture()
	obs.EnvironmentID, obs.EnvironmentEvidence = nil, nil
	persistOK(t, db, obs)
	persistCount(t, db, "test_results", 1)
	var rawEnv, evidence sql.NullString
	var legacy string
	if err := db.QueryRow("SELECT environment_id,environment_evidence FROM ci_artifacts").Scan(&rawEnv, &evidence); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT environment_id FROM test_results").Scan(&legacy); err != nil {
		t.Fatal(err)
	}
	if rawEnv.Valid || evidence.Valid || legacy != "" {
		t.Fatal("unknown environment was invented or treated as verified equivalence")
	}
}

func TestPersistUnverifiedEnvironmentIsNotComparableKey(t *testing.T) {
	db := persistDB(t)
	obs := persistFixture()
	obs.EnvironmentEvidence = nil
	persistOK(t, db, obs)
	var legacy string
	if err := db.QueryRow("SELECT environment_id FROM test_results").Scan(&legacy); err != nil {
		t.Fatal(err)
	}
	if legacy != "" {
		t.Fatal("unverified environment became a canonical environment key")
	}
	var evidence sql.NullString
	if err := db.QueryRow("SELECT environment_evidence FROM ci_artifacts").Scan(&evidence); err != nil {
		t.Fatal(err)
	}
	if evidence.Valid {
		t.Fatal("unknown environment evidence was invented")
	}
}

func TestPersistGitLabExecutionWithoutOrdinal(t *testing.T) {
	db := persistDB(t)
	obs := persistFixture()
	obs.Provider, obs.UnitKey = "gitlab", "job-1:junit"
	obs.ArtifactID, obs.Attempt, obs.RunAttempt = nil, nil, nil
	persistOK(t, db, obs)
	persistCount(t, db, "test_results", 1)
	var rawAttempt sql.NullInt64
	var attempt int
	var artifact string
	if err := db.QueryRow("SELECT attempt FROM ci_artifacts").Scan(&rawAttempt); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT attempt,artifact_id FROM test_results").Scan(&attempt, &artifact); err != nil {
		t.Fatal(err)
	}
	if rawAttempt.Valid || attempt != 1 || artifact != "" {
		t.Fatal("GitLab unknown ordinal/artifact was invented")
	}
}

func TestPersistCommitMetadataDeferralAndParents(t *testing.T) {
	for _, tc := range []struct {
		name                string
		parents             []string
		wantRaw, wantLegacy string
	}{
		{"merge", []string{"parent-a", "parent-b"}, `["parent-a","parent-b"]`, ""},
		{"root", []string{}, `[]`, ""},
		{"unknown", nil, "", ""},
		{"single", []string{"parent-a"}, `["parent-a"]`, "parent-a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := persistDB(t)
			obs := persistFixture()
			obs.Commit.ParentSHAs, obs.Commit.Kinds = tc.parents, nil
			persistOK(t, db, obs)
			var parents, parent sql.NullString
			var kinds string
			if err := db.QueryRow("SELECT tested_parent_shas_json FROM ci_artifacts").Scan(&parents); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow("SELECT parent_sha,kinds FROM commits").Scan(&parent, &kinds); err != nil {
				t.Fatal(err)
			}
			if parents.Valid != (tc.parents != nil) || parents.String != tc.wantRaw || parent.Valid != (tc.wantLegacy != "") || parent.String != tc.wantLegacy || kinds != "null" {
				t.Fatal("parents or unknown kind classification were fabricated/lost")
			}
		})
	}
	for _, field := range []string{"commit", "authored", "files", "added", "removed"} {
		t.Run("missing_"+field, func(t *testing.T) {
			db := persistDB(t)
			obs := persistFixture()
			switch field {
			case "commit":
				obs.Commit = nil
			case "authored":
				obs.Commit.AuthoredAt = nil
			case "files":
				obs.Commit.FilesChanged = nil
			case "added":
				obs.Commit.LinesAdded = nil
			case "removed":
				obs.Commit.LinesRemoved = nil
			}
			persistOK(t, db, obs)
			persistCount(t, db, "test_results", 1)
			persistCount(t, db, "commits", 0)
			var gaps int
			if err := db.QueryRow("SELECT count(*) FROM acquisition_exclusions WHERE reason='commit_metadata_unavailable'").Scan(&gaps); err != nil {
				t.Fatal(err)
			}
			if gaps != 1 {
				t.Fatal("metadata-only deferral was not explicit")
			}
			if obs.Commit != nil {
				var parents string
				if err := db.QueryRow("SELECT tested_parent_shas_json FROM ci_artifacts").Scan(&parents); err != nil {
					t.Fatal(err)
				}
				if parents != `["parent-a","parent-b"]` {
					t.Fatal("partial metadata lost all parents")
				}
			}
		})
	}
}

func TestPersistReplayAndMatrixAttempts(t *testing.T) {
	db := persistDB(t)
	obs := persistFixture()
	persistOK(t, db, obs)
	persistOK(t, db, obs)
	for _, change := range []func(*ProviderObservation){
		func(o *ProviderObservation) { o.UnitKey = "unit-job2"; o.JobID = persistPtr("job-2") },
		func(o *ProviderObservation) {
			o.UnitKey = "unit-attempt2"
			o.Attempt = persistPtr(2)
			o.RunAttempt = persistPtr(2)
		},
		func(o *ProviderObservation) { o.UnitKey = "unit-env2"; o.EnvironmentID = persistPtr("go125-linux") },
	} {
		next := persistFixture()
		change(&next)
		persistOK(t, db, next)
	}
	persistCount(t, db, "ci_observations", 4)
	persistCount(t, db, "test_results", 4)
	persistCount(t, db, "tests", 1)
	persistCount(t, db, "commits", 1)
	stats, err := persistenceStats(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if stats.RawCases != 4 || stats.FactualResults != 4 || stats.FactualResultBearingCommits != 1 || stats.ReplayDuplicates != 1 || stats.InspectedArtifacts != 4 || stats.InspectedAttempts != 2 || stats.InspectedRuns != 1 {
		t.Fatalf("incorrect raw/canonical/replay/coverage units: %+v", stats)
	}
}

func TestPersistCrossEntryCollisionWithholdsCanonical(t *testing.T) {
	db := persistDB(t)
	obs := persistFixture()
	duplicate := obs.Entries[0]
	duplicate.Name = "shard/junit.xml"
	obs.Entries = append(obs.Entries, duplicate)
	persistOK(t, db, obs)
	persistCount(t, db, "ci_observations", 2)
	persistCount(t, db, "test_results", 0)
	persistCount(t, db, "acquisition_exclusions", 2)
	persistOK(t, db, obs)
	persistCount(t, db, "ci_observations", 2)
	persistCount(t, db, "acquisition_exclusions", 2)
}

func TestPersistLateCollisionWithdrawsCanonical(t *testing.T) {
	db := persistDB(t)
	obs := persistFixture()
	persistOK(t, db, obs)
	persistCount(t, db, "test_results", 1)
	obs.ContentSHA256 = strings.Repeat("c", 64)
	obs.Entries[0].Cases[0].Case.Status = "pass"
	persistOK(t, db, obs)
	persistCount(t, db, "ci_observations", 2)
	persistCount(t, db, "test_results", 0)
	persistCount(t, db, "acquisition_exclusions", 2)
	stats, err := persistenceStats(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if stats.RawCases != 2 || stats.FactualResults != 0 || stats.FactualResultBearingCommits != 0 || stats.AcquisitionExclusions["duplicate_test_identity"] != 2 {
		t.Fatalf("withdrawal counts = %+v", stats)
	}
}

func TestPersistBindingRejectsOtherRepositoryBeforeMutation(t *testing.T) {
	for _, field := range []string{"project", "provider"} {
		t.Run(field, func(t *testing.T) {
			db := persistDB(t)
			obs := persistFixture()
			persistOK(t, db, obs)
			persistOK(t, db, obs)
			if field == "project" {
				obs.Project = "other/repo"
			} else {
				obs.Provider = "gitlab"
			}
			obs.Entries[0].Cases[0].Identity.Name = "must not overwrite"
			err := persistObservation(context.Background(), db, obs)
			for _, table := range []string{"mining_sources", "mining_runs", "ci_artifacts", "ci_observations", "tests", "commits", "test_results"} {
				persistCount(t, db, table, 1)
			}
			var name string
			if queryErr := db.QueryRow("SELECT name FROM tests").Scan(&name); queryErr != nil {
				t.Fatal(queryErr)
			}
			if name != "TestT" {
				t.Fatal("wrong repository mutated existing test metadata")
			}
			if err == nil {
				t.Fatal("wrong repository binding accepted")
			}
		})
	}
}

func TestPersistUnsupportedIdentityAndRawStatus(t *testing.T) {
	db := persistDB(t)
	obs := persistFixture()
	obs.Entries[0].Cases[0].Identity = runner.TestIdentity{}
	obs.Entries[0].Cases[0].IdentityError = "missing verified Go package"
	obs.Entries[0].Cases[0].Case.DurationMS = nil
	persistOK(t, db, obs)
	persistCount(t, db, "ci_observations", 1)
	persistCount(t, db, "test_results", 0)
	persistCount(t, db, "acquisition_exclusions", 1)
	var duration sql.NullInt64
	if err := db.QueryRow("SELECT duration_ms FROM ci_observations").Scan(&duration); err != nil {
		t.Fatal(err)
	}
	if duration.Valid {
		t.Fatal("unknown duration replaced by zero")
	}
}

func TestPersistFactualTimesAndPolicyFields(t *testing.T) {
	db := persistDB(t)
	obs := persistFixture()
	obs.Entries[0].Cases[0].Identity.Unstable = true
	persistOK(t, db, obs)
	if _, err := db.Exec("UPDATE tests SET runs=9,fails=3,median_ms=77,flaky_score=0.5,quarantined=1"); err != nil {
		t.Fatal(err)
	}
	next := persistFixture()
	next.UnitKey, next.ArtifactID = "artifact-2", persistPtr("artifact-2")
	next.ObservedAt = next.ObservedAt.Add(-time.Hour)
	persistOK(t, db, next)
	next = persistFixture()
	next.UnitKey, next.ArtifactID = "artifact-3", persistPtr("artifact-3")
	next.ObservedAt = next.ObservedAt.Add(time.Hour)
	persistOK(t, db, next)
	var first, last string
	var unstable, runs, fails, median, quarantined int
	var flaky float64
	if err := db.QueryRow("SELECT first_seen,last_seen,unstable_id,runs,fails,median_ms,flaky_score,quarantined FROM tests").Scan(&first, &last, &unstable, &runs, &fails, &median, &flaky, &quarantined); err != nil {
		t.Fatal(err)
	}
	if first != "2026-10-01T12:00:00.000000000Z" || last != "2026-10-01T14:00:00.000000000Z" || unstable != 1 || runs != 9 || fails != 3 || median != 77 || flaky != .5 || quarantined != 1 {
		t.Fatal("factual observation times or existing policy fields were lost")
	}
}

func TestPersistFailureRollsBackWholeEnvelope(t *testing.T) {
	db := persistDB(t)
	if _, err := db.Exec("CREATE TRIGGER reject_result BEFORE INSERT ON test_results BEGIN SELECT RAISE(ABORT, 'fixture rejection'); END"); err != nil {
		t.Fatal(err)
	}
	if err := persistObservation(context.Background(), db, persistFixture()); err == nil {
		t.Fatal("result write unexpectedly succeeded")
	}
	for _, table := range []string{"mining_sources", "mining_runs", "ci_artifacts", "ci_observations", "acquisition_exclusions", "tests", "commits", "test_results"} {
		persistCount(t, db, table, 0)
	}
}

func TestPersistStatsRenderingOrdersExclusions(t *testing.T) {
	s := Stats{RawCases: 5, FactualResults: 2, AcquisitionExclusions: map[string]int{"z_gap": 2, "a_gap": 1}}
	if got := s.String(); !strings.HasSuffix(got, " a_gap=1 z_gap=2") || !strings.Contains(got, "raw_cases=5 factual_results=2") {
		t.Fatalf("unordered/ambiguous stats text: %q", got)
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"acquisition_exclusions":{"a_gap":1,"z_gap":2}`) {
		t.Fatal("JSON exclusions are nondeterministic")
	}
	s.RequestedSince = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	s.RequestedUntil = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if !strings.Contains(s.String(), "requested_since=2026-09-01T00:00:00Z requested_until=2026-10-01T00:00:00Z") || !strings.Contains(s.String(), "observed_since=unknown observed_until=unknown") {
		t.Fatal("text stats omitted requested/observed bounds or invented coverage")
	}
}

func TestPersistRawOrdinalsAndCoverage(t *testing.T) {
	db := persistDB(t)
	obs := persistFixture()
	obs.Entries[0].Cases = append(obs.Entries[0].Cases, obs.Entries[0].Cases[0])
	persistOK(t, db, obs)
	persistCount(t, db, "ci_observations", 2)
	persistCount(t, db, "test_results", 0)
	var ordinals int
	if err := db.QueryRow("SELECT count(DISTINCT case_ordinal) FROM ci_observations").Scan(&ordinals); err != nil {
		t.Fatal(err)
	}
	if ordinals != 2 {
		t.Fatal("duplicate raw testcase ordinals collapsed")
	}
	gap := persistFixture()
	gap.UnitKey, gap.ContentSHA256, gap.Entries = "no-report", strings.Repeat("c", 64), nil
	gap.State, gap.ExclusionReason = "incomplete", "no_matching_reports"
	persistOK(t, db, gap)
	gap.ContentSHA256, gap.State, gap.ExclusionReason = strings.Repeat("d", 64), "capped", "source_cap"
	persistOK(t, db, gap)
	stats, err := persistenceStats(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if stats.InspectedArtifacts != 2 || stats.RawCases != 2 || stats.IncompleteUnits != 1 || stats.CappedUnits != 1 || stats.AcquisitionExclusions["no_matching_reports"] != 1 || stats.AcquisitionExclusions["source_cap"] != 1 {
		t.Fatal("coverage counted content versions as distinct artifacts or fabricated raw cases")
	}
}

func TestPersistUnknownEnvironmentEnrichmentRekeysSameRawCase(t *testing.T) {
	db := persistDB(t)
	obs := persistFixture()
	obs.EnvironmentID, obs.EnvironmentEvidence = nil, nil
	persistOK(t, db, obs)
	persistOK(t, db, persistFixture())
	persistCount(t, db, "ci_observations", 1)
	persistCount(t, db, "test_results", 1)
	var environment string
	if err := db.QueryRow("SELECT environment_id FROM test_results").Scan(&environment); err != nil {
		t.Fatal(err)
	}
	if environment != "go126-linux" {
		t.Fatal("verified enrichment left a stale unknown canonical result")
	}
}

func TestPersistReplayEnrichesUnknownFacts(t *testing.T) {
	db := persistDB(t)
	unknown := persistFixture()
	unknown.TestedSHA, unknown.TestedRevisionEvidence, unknown.JobID, unknown.ExecutionEvidence, unknown.Attempt = nil, nil, nil, nil, nil
	unknown.Entries[0].Cases[0].Identity = runner.TestIdentity{}
	unknown.Entries[0].Cases[0].IdentityError = "identity unavailable"
	persistOK(t, db, unknown)
	persistCount(t, db, "test_results", 0)
	verified := persistFixture()
	verified.ObservedAt = verified.ObservedAt.Add(24 * time.Hour)
	verified.Entries[0].Cases[0].IdentityEvidence = "verified tested module"
	persistOK(t, db, verified)
	persistCount(t, db, "ci_observations", 1)
	persistCount(t, db, "test_results", 1)
	var observed, evidence string
	if err := db.QueryRow("SELECT observed_at FROM test_results").Scan(&observed); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT identity_evidence FROM ci_observations").Scan(&evidence); err != nil {
		t.Fatal(err)
	}
	if observed != "2026-10-01T13:00:00.000000000Z" || evidence != "verified tested module" {
		t.Fatal("enrichment changed original factual time or lost identity evidence")
	}
	// An unknown replay cannot erase known provenance or derived eligibility.
	persistOK(t, db, unknown)
	persistCount(t, db, "test_results", 1)
}

func TestPersistReplayRejectsContradictoryKnownFacts(t *testing.T) {
	for _, field := range []string{"tested_sha", "job", "attempt", "artifact", "environment", "test_id", "raw_status"} {
		t.Run(field, func(t *testing.T) {
			db := persistDB(t)
			obs := persistFixture()
			persistOK(t, db, obs)
			switch field {
			case "tested_sha":
				obs.TestedSHA = persistPtr("conflicting-tested")
			case "job":
				obs.JobID = persistPtr("conflicting-job")
			case "attempt":
				obs.Attempt = persistPtr(2)
			case "artifact":
				obs.ArtifactID = persistPtr("conflicting-artifact")
			case "environment":
				obs.EnvironmentID = persistPtr("conflicting-env")
			case "test_id":
				obs.Entries[0].Cases[0].Identity.ID = "go:other:TestT"
			case "raw_status":
				obs.Entries[0].Cases[0].Case.Status = "pass"
			}
			err := persistObservation(context.Background(), db, obs)
			var conflict *AcquisitionConflictError
			if !errors.As(err, &conflict) || conflict.Reason == "" {
				t.Fatal("contradictory replay did not return a recognizable safe conflict reason")
			}
			persistCount(t, db, "ci_observations", 1)
			persistCount(t, db, "test_results", 1)
			var sha, status string
			if err := db.QueryRow("SELECT commit_sha,status FROM test_results").Scan(&sha, &status); err != nil {
				t.Fatal(err)
			}
			if sha != "tested-merge" || status != "fail" {
				t.Fatal("contradictory replay mutated prior canonical facts")
			}
		})
	}
}

func TestPersistReplayRefreshesArtifactAcquisitionState(t *testing.T) {
	db := persistDB(t)
	obs := persistFixture()
	obs.State = "incomplete"
	obs.ExclusionReason = "partial_acquisition"
	persistOK(t, db, obs)
	obs.State, obs.ExclusionReason = "complete", ""
	obs.ObservedAt = obs.ObservedAt.Add(24 * time.Hour)
	persistOK(t, db, obs)
	var state, observed string
	if err := db.QueryRow("SELECT state,observed_at FROM ci_artifacts").Scan(&state, &observed); err != nil {
		t.Fatal(err)
	}
	if state != "complete" || observed != "2026-10-01T13:00:00.000000000Z" {
		t.Fatal("successful content replay did not refresh acquisition state while preserving source time")
	}
	stats, err := persistenceStats(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if stats.IncompleteUnits != 0 || stats.AcquisitionExclusions["partial_acquisition"] != 1 || stats.RawCases != 1 || stats.FactualResults != 1 {
		t.Fatal("current acquisition state was confused with historical exclusions or replay counts")
	}
}

func TestPersistArtifactDoesNotCertifyRunCompletion(t *testing.T) {
	db := persistDB(t)
	obs := persistFixture()
	persistOK(t, db, obs)
	var state string
	if err := db.QueryRow("SELECT state FROM mining_runs").Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "incomplete" {
		t.Fatal("artifact persistence certified an unfinalized run as complete")
	}
	// A separately finalized run must be rechecked when another unit arrives.
	if _, err := db.Exec("UPDATE mining_runs SET state='complete'"); err != nil {
		t.Fatal(err)
	}
	obs.UnitKey, obs.ArtifactID = "artifact-2", persistPtr("artifact-2")
	persistOK(t, db, obs)
	if err := db.QueryRow("SELECT state FROM mining_runs").Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "incomplete" {
		t.Fatal("new artifact left an unchecked run checkpoint complete")
	}
}
