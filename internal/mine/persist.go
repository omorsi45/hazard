package mine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
)

const observationTimeFormat = "2006-01-02T15:04:05.000000000Z"

// resultKey groups raw records by the immutable 0001 primary key. Entry and
// ordinal belong to the separate raw key, not this canonical projection.
type resultKey struct {
	Source, Project, SHA, TestID, RunID, JobID, EnvironmentID, ArtifactID string
	Attempt                                                               int
}

func (k resultKey) args() []any {
	return []any{k.Source, k.Project, k.SHA, k.TestID, k.RunID, k.JobID, k.EnvironmentID, k.Attempt, k.ArtifactID}
}

const resultWhere = `source=? AND project=? AND commit_sha=? AND test_id=? AND pipeline_id=? AND job_id=? AND environment_id=? AND attempt=? AND artifact_id=?`

func persistObservation(ctx context.Context, db *sql.DB, obs ProviderObservation) (err error) {
	if (obs.Provider != "github" && obs.Provider != "gitlab") || strings.TrimSpace(obs.Project) == "" || obs.RunID == "" || obs.UnitKey == "" || obs.ContentSHA256 == "" || obs.ObservedAt.IsZero() {
		return errors.New("invalid acquisition identity or observation time")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("rollback acquisition: %w", rollbackErr))
		}
	}()
	observed := obs.ObservedAt.UTC().Format(observationTimeFormat)
	if _, err := tx.ExecContext(ctx, `INSERT INTO mining_sources(singleton,provider,project,observed_at) VALUES(1,?,?,?) ON CONFLICT(singleton) DO NOTHING`, obs.Provider, obs.Project, observed); err != nil {
		return err
	}
	var provider, project string
	if err := tx.QueryRowContext(ctx, "SELECT provider,project FROM mining_sources WHERE singleton=1").Scan(&provider, &project); err != nil {
		return err
	}
	if provider != obs.Provider || project != obs.Project {
		return errors.New("mining database is bound to a different provider/project")
	}
	if err := mergeEnvelope(ctx, tx, &obs); err != nil {
		return err
	}
	if err := persistEnvelope(ctx, tx, obs, observed); err != nil {
		return err
	}
	if obs.ExclusionReason != "" {
		if err := persistExclusion(ctx, tx, obs, "", -1, obs.ExclusionReason, obs.ExclusionDetail, observed); err != nil {
			return err
		}
	}
	type candidate struct {
		key      resultKey
		entry    string
		record   ObservationCase
		observed string
		fresh    bool
	}
	candidates := map[string]candidate{}
	replays := 0
	for _, entry := range obs.Entries {
		if entry.Name == "" || entry.SHA256 == "" {
			return errors.New("missing report entry identity")
		}
		for ordinal, record := range entry.Cases {
			oldKey, caseTime, replay, err := mergeCase(ctx, tx, obs, entry, ordinal, &record)
			if err != nil {
				return err
			}
			if caseTime == "" {
				caseTime = observed
			}
			if replay {
				replays++
			}
			key, reason := factualKey(obs, record)
			var encodedKey any
			keyJSON := ""
			if reason == "" {
				data, err := json.Marshal(key)
				if err != nil {
					return err
				}
				keyJSON, encodedKey = string(data), string(data)
			}
			properties, err := json.Marshal(record.Case.Properties)
			if err != nil {
				return err
			}
			if oldKey.Valid && oldKey.String != keyJSON {
				var previous resultKey
				if err := json.Unmarshal([]byte(oldKey.String), &previous); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, "DELETE FROM test_results WHERE "+resultWhere, previous.args()...); err != nil {
					return err
				}
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO ci_observations(
				provider,project,unit_key,content_sha256,artifact_entry,entry_sha256,case_ordinal,
				raw_suite,raw_name,raw_classname,raw_file,status,duration_ms,raw_message,raw_type,raw_properties_json,
				suite_incomplete,diagnostics_truncated,test_id,identity_language,identity_package,identity_name,identity_file,identity_runner_id,
				unstable_id,identity_reason,identity_error,identity_evidence,infra_reason,result_key,observed_at)
				VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(provider,project,unit_key,content_sha256,artifact_entry,case_ordinal)
				DO UPDATE SET test_id=excluded.test_id,identity_language=excluded.identity_language,identity_package=excluded.identity_package,identity_name=excluded.identity_name,
				identity_file=excluded.identity_file,identity_runner_id=excluded.identity_runner_id,unstable_id=max(ci_observations.unstable_id,excluded.unstable_id),
				identity_reason=excluded.identity_reason,identity_error=excluded.identity_error,identity_evidence=coalesce(ci_observations.identity_evidence,excluded.identity_evidence),result_key=excluded.result_key`,
				obs.Provider, obs.Project, obs.UnitKey, obs.ContentSHA256, entry.Name, entry.SHA256, ordinal,
				record.Case.Suite, record.Case.Name, record.Case.Classname, record.Case.File, record.Case.Status, record.Case.DurationMS, record.Case.Message, record.Case.Type, string(properties),
				record.Case.SuiteIncomplete || obs.SuiteIncomplete, record.Case.DiagnosticsTruncated || obs.DiagnosticsTruncated,
				record.Identity.ID, record.Identity.Language, record.Identity.Package, record.Identity.Name, record.Identity.File, record.Identity.RunnerID,
				record.Identity.Unstable, record.Identity.IdentityReason, record.IdentityError, nullableText(record.IdentityEvidence), record.InfraReason, encodedKey, caseTime)
			if err != nil {
				return err
			}
			if reason != "" {
				if err := persistExclusion(ctx, tx, obs, entry.Name, ordinal, reason, "factual promotion lacks required identity or provenance", caseTime); err != nil {
					return err
				}
			} else {
				prior := candidates[keyJSON]
				candidates[keyJSON] = candidate{key, entry.Name, record, caseTime, !replay || !oldKey.Valid || oldKey.String != keyJSON || prior.fresh}
			}
			if record.ExclusionReason != "" {
				if err := persistExclusion(ctx, tx, obs, entry.Name, ordinal, record.ExclusionReason, record.ExclusionDetail, caseTime); err != nil {
					return err
				}
			}
		}
	}
	if _, err := tx.ExecContext(ctx, "UPDATE ci_artifacts SET replay_cases=replay_cases+? WHERE provider=? AND project=? AND unit_key=? AND content_sha256=?", replays, obs.Provider, obs.Project, obs.UnitKey, obs.ContentSHA256); err != nil {
		return err
	}
	// Preflight all entries together, including earlier acquisition envelopes.
	for encoded, candidate := range candidates {
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM ci_observations WHERE result_key=?", encoded).Scan(&count); err != nil {
			return err
		}
		if count > 1 {
			if _, err := tx.ExecContext(ctx, `INSERT INTO acquisition_exclusions(provider,project,unit_key,content_sha256,artifact_entry,case_ordinal,reason,detail,observed_at)
				SELECT provider,project,unit_key,content_sha256,artifact_entry,case_ordinal,'duplicate_test_identity','distinct raw records share a legacy result key',observed_at
				FROM ci_observations WHERE result_key=? ON CONFLICT DO NOTHING`, encoded); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "DELETE FROM test_results WHERE "+resultWhere, candidate.key.args()...); err != nil {
				return err
			}
			continue
		}
		if count != 1 {
			continue
		}
		if err := persistCommit(ctx, tx, obs, candidate.observed); err != nil {
			return err
		}
		if !candidate.fresh {
			continue
		}
		if err := persistTest(ctx, tx, candidate.record, candidate.observed); err != nil {
			return err
		}
		args := candidate.key.args()
		args = append(args, candidate.record.Case.Status, candidate.record.Case.DurationMS, candidate.observed, candidate.entry,
			candidate.record.Case.Name, candidate.record.Case.Classname, candidate.record.Case.Message, nullableText(candidate.record.InfraReason))
		if _, err := tx.ExecContext(ctx, `INSERT INTO test_results(source,project,commit_sha,test_id,pipeline_id,job_id,environment_id,attempt,artifact_id,
			status,duration_ms,observed_at,artifact_path,raw_name,raw_classname,raw_message,infra_reason) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`, args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func factualKey(obs ProviderObservation, record ObservationCase) (resultKey, string) {
	id := record.Identity
	if record.IdentityError != "" || id.ID == "" || (id.Language != "go" && id.Language != "python") || id.Package == "" || id.Name == "" {
		return resultKey{}, "unsupported_test_identity"
	}
	if record.Case.Status != "pass" && record.Case.Status != "fail" && record.Case.Status != "error" && record.Case.Status != "skip" {
		return resultKey{}, "unsupported_status"
	}
	if !knownText(obs.TestedSHA) || !knownText(obs.TestedRevisionEvidence) {
		return resultKey{}, "unverified_tested_revision"
	}
	if !knownText(obs.JobID) || !knownText(obs.ExecutionEvidence) || (obs.Provider == "github" && (obs.Attempt == nil || *obs.Attempt < 1)) {
		return resultKey{}, "unverified_execution"
	}
	attempt := 1 // Legacy GitLab placeholder; raw ordinal remains NULL.
	if obs.Attempt != nil {
		attempt = *obs.Attempt
	}
	return resultKey{Source: obs.Provider, Project: obs.Project, SHA: *obs.TestedSHA, TestID: id.ID, RunID: obs.RunID, JobID: *obs.JobID,
		EnvironmentID: canonicalEnvironment(obs), Attempt: attempt, ArtifactID: stringValue(obs.ArtifactID)}, ""
}

func canonicalEnvironment(obs ProviderObservation) string {
	if !knownText(obs.EnvironmentID) || !knownText(obs.EnvironmentEvidence) {
		return ""
	}
	return *obs.EnvironmentID
}

func knownText(s *string) bool { return s != nil && strings.TrimSpace(*s) != "" }
func stringValue(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
func nullableText(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(observationTimeFormat)
}

func mergeKnown[T comparable](incoming **T, stored *T) error {
	if stored == nil {
		return nil
	}
	if *incoming != nil && **incoming != *stored {
		return &AcquisitionConflictError{Reason: "known_provenance"}
	}
	*incoming = stored
	return nil
}

func mergeEnvelope(ctx context.Context, tx *sql.Tx, obs *ProviderObservation) error {
	var old ProviderObservation
	var authored, parents sql.NullString
	c := &CommitMetadata{}
	var kinds string
	err := tx.QueryRowContext(ctx, `SELECT run_id,artifact_id,job_id,attempt,environment_id,tested_sha,tested_revision_evidence,execution_evidence,environment_evidence,
		tested_authored_at,tested_parent_shas_json,tested_files_changed,tested_lines_added,tested_lines_removed,tested_kinds_json FROM ci_artifacts
		WHERE provider=? AND project=? AND unit_key=? AND content_sha256=?`, obs.Provider, obs.Project, obs.UnitKey, obs.ContentSHA256).Scan(
		&old.RunID, &old.ArtifactID, &old.JobID, &old.Attempt, &old.EnvironmentID, &old.TestedSHA, &old.TestedRevisionEvidence, &old.ExecutionEvidence, &old.EnvironmentEvidence,
		&authored, &parents, &c.FilesChanged, &c.LinesAdded, &c.LinesRemoved, &kinds)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if old.RunID != obs.RunID {
		return &AcquisitionConflictError{Reason: "known_run"}
	}
	for _, field := range []struct {
		incoming **string
		stored   *string
	}{{&obs.ArtifactID, old.ArtifactID}, {&obs.JobID, old.JobID}, {&obs.EnvironmentID, old.EnvironmentID}, {&obs.TestedSHA, old.TestedSHA}} {
		if err := mergeKnown(field.incoming, field.stored); err != nil {
			return err
		}
	}
	if err := mergeKnown(&obs.Attempt, old.Attempt); err != nil {
		return err
	}
	if old.TestedRevisionEvidence != nil {
		obs.TestedRevisionEvidence = old.TestedRevisionEvidence
	}
	if old.ExecutionEvidence != nil {
		obs.ExecutionEvidence = old.ExecutionEvidence
	}
	if old.EnvironmentEvidence != nil {
		obs.EnvironmentEvidence = old.EnvironmentEvidence
	}
	if authored.Valid {
		value, err := time.Parse(time.RFC3339Nano, authored.String)
		if err != nil {
			return err
		}
		c.AuthoredAt = &value
	}
	if parents.Valid {
		if err := json.Unmarshal([]byte(parents.String), &c.ParentSHAs); err != nil {
			return err
		}
	}
	if err := json.Unmarshal([]byte(kinds), &c.Kinds); err != nil {
		return err
	}
	if obs.Commit == nil {
		obs.Commit = c
		return nil
	}
	if c.AuthoredAt != nil {
		if obs.Commit.AuthoredAt != nil && !c.AuthoredAt.Equal(*obs.Commit.AuthoredAt) {
			return &AcquisitionConflictError{Reason: "commit_authored_time"}
		}
		obs.Commit.AuthoredAt = c.AuthoredAt
	}
	for _, field := range []struct {
		incoming **int
		stored   *int
	}{{&obs.Commit.FilesChanged, c.FilesChanged}, {&obs.Commit.LinesAdded, c.LinesAdded}, {&obs.Commit.LinesRemoved, c.LinesRemoved}} {
		if err := mergeKnown(field.incoming, field.stored); err != nil {
			return err
		}
	}
	if c.ParentSHAs != nil {
		if obs.Commit.ParentSHAs != nil && !reflect.DeepEqual(obs.Commit.ParentSHAs, c.ParentSHAs) {
			return &AcquisitionConflictError{Reason: "commit_parents"}
		}
		obs.Commit.ParentSHAs = c.ParentSHAs
	}
	if c.Kinds != nil {
		if obs.Commit.Kinds != nil && !reflect.DeepEqual(obs.Commit.Kinds, c.Kinds) {
			return &AcquisitionConflictError{Reason: "commit_kinds"}
		}
		obs.Commit.Kinds = c.Kinds
	}
	return nil
}

func mergeCase(ctx context.Context, tx *sql.Tx, obs ProviderObservation, entry ObservationEntry, ordinal int, record *ObservationCase) (sql.NullString, string, bool, error) {
	var old ObservationCase
	var key sql.NullString
	var observed, entryHash, properties string
	err := tx.QueryRowContext(ctx, `SELECT entry_sha256,raw_suite,raw_name,raw_classname,raw_file,status,duration_ms,raw_message,raw_type,raw_properties_json,
		suite_incomplete,diagnostics_truncated,test_id,identity_language,identity_package,identity_name,identity_file,identity_runner_id,unstable_id,identity_reason,identity_error,result_key,observed_at
		FROM ci_observations WHERE provider=? AND project=? AND unit_key=? AND content_sha256=? AND artifact_entry=? AND case_ordinal=?`,
		obs.Provider, obs.Project, obs.UnitKey, obs.ContentSHA256, entry.Name, ordinal).Scan(&entryHash, &old.Case.Suite, &old.Case.Name, &old.Case.Classname, &old.Case.File, &old.Case.Status, &old.Case.DurationMS, &old.Case.Message, &old.Case.Type, &properties,
		&old.Case.SuiteIncomplete, &old.Case.DiagnosticsTruncated, &old.Identity.ID, &old.Identity.Language, &old.Identity.Package, &old.Identity.Name, &old.Identity.File, &old.Identity.RunnerID, &old.Identity.Unstable, &old.Identity.IdentityReason, &old.IdentityError, &key, &observed)
	if errors.Is(err, sql.ErrNoRows) {
		return key, "", false, nil
	}
	if err != nil {
		return key, "", false, err
	}
	if err := json.Unmarshal([]byte(properties), &old.Case.Properties); err != nil {
		return key, "", false, err
	}
	current := record.Case
	current.SuiteIncomplete = current.SuiteIncomplete || obs.SuiteIncomplete
	current.DiagnosticsTruncated = current.DiagnosticsTruncated || obs.DiagnosticsTruncated
	if entryHash != entry.SHA256 || !reflect.DeepEqual(current, old.Case) {
		return key, "", false, &AcquisitionConflictError{Reason: "immutable_raw_case"}
	}
	if old.Identity.ID != "" && old.IdentityError == "" {
		if record.Identity.ID != "" && (record.Identity.ID != old.Identity.ID || record.Identity.Language != old.Identity.Language || record.Identity.Package != old.Identity.Package || record.Identity.Name != old.Identity.Name) {
			return key, "", false, &AcquisitionConflictError{Reason: "known_test_identity"}
		}
		record.Identity = old.Identity
		record.IdentityError = ""
	}
	return key, observed, true, nil
}

func persistEnvelope(ctx context.Context, tx *sql.Tx, obs ProviderObservation, observed string) error {
	attemptKey := "unknown"
	if obs.RunAttempt != nil {
		attemptKey = strconv.Itoa(*obs.RunAttempt)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO mining_runs(provider,project,run_id,attempt_key,attempt,source_head_sha,branch,created_at,requested_since,requested_until,state,observed_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(provider,project,run_id,attempt_key) DO UPDATE SET state=excluded.state,observed_at=excluded.observed_at`,
		obs.Provider, obs.Project, obs.RunID, attemptKey, obs.RunAttempt, obs.SourceHeadSHA, obs.Branch, nullableTime(obs.RunCreatedAt), nullableTime(obs.RequestedSince), nullableTime(obs.RequestedUntil), "incomplete", observed); err != nil {
		return err
	}
	var parents, authored, files, added, removed any
	kinds := "null"
	if obs.Commit != nil {
		if obs.Commit.ParentSHAs != nil {
			data, err := json.Marshal(obs.Commit.ParentSHAs)
			if err != nil {
				return err
			}
			parents = string(data)
		}
		data, err := json.Marshal(obs.Commit.Kinds)
		if err != nil {
			return err
		}
		kinds = string(data)
		authored, files, added, removed = nullableTime(obs.Commit.AuthoredAt), obs.Commit.FilesChanged, obs.Commit.LinesAdded, obs.Commit.LinesRemoved
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO ci_artifacts(provider,project,unit_key,content_sha256,run_id,artifact_id,job_id,attempt,environment_id,tested_sha,
		tested_revision_evidence,execution_evidence,environment_evidence,raw_blob_path,source_head_sha,branch,tested_authored_at,tested_parent_shas_json,
		tested_files_changed,tested_lines_added,tested_lines_removed,tested_kinds_json,suite_incomplete,diagnostics_truncated,state,observed_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(provider,project,unit_key,content_sha256) DO UPDATE SET
		artifact_id=excluded.artifact_id,job_id=excluded.job_id,attempt=excluded.attempt,environment_id=excluded.environment_id,tested_sha=excluded.tested_sha,
		tested_revision_evidence=excluded.tested_revision_evidence,execution_evidence=excluded.execution_evidence,environment_evidence=excluded.environment_evidence,
		tested_authored_at=excluded.tested_authored_at,tested_parent_shas_json=excluded.tested_parent_shas_json,tested_files_changed=excluded.tested_files_changed,
		tested_lines_added=excluded.tested_lines_added,tested_lines_removed=excluded.tested_lines_removed,tested_kinds_json=excluded.tested_kinds_json,state=excluded.state`,
		obs.Provider, obs.Project, obs.UnitKey, obs.ContentSHA256, obs.RunID, obs.ArtifactID, obs.JobID, obs.Attempt, obs.EnvironmentID, obs.TestedSHA,
		obs.TestedRevisionEvidence, obs.ExecutionEvidence, obs.EnvironmentEvidence, obs.RawBlobPath, obs.SourceHeadSHA, obs.Branch, authored, parents, files, added, removed, kinds,
		obs.SuiteIncomplete, obs.DiagnosticsTruncated, obs.State, observed)
	return err
}

func persistExclusion(ctx context.Context, tx *sql.Tx, obs ProviderObservation, entry string, ordinal int, reason, detail, observed string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO acquisition_exclusions(provider,project,unit_key,content_sha256,artifact_entry,case_ordinal,reason,detail,observed_at) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`, obs.Provider, obs.Project, obs.UnitKey, obs.ContentSHA256, entry, ordinal, reason, detail, observed)
	return err
}

func persistCommit(ctx context.Context, tx *sql.Tx, obs ProviderObservation, observed string) error {
	c := obs.Commit
	if c == nil || c.AuthoredAt == nil || c.AuthoredAt.IsZero() || c.FilesChanged == nil || c.LinesAdded == nil || c.LinesRemoved == nil {
		return persistExclusion(ctx, tx, obs, "", -1, "commit_metadata_unavailable", "required authored time or change totals unavailable", observed)
	}
	var parent any
	if len(c.ParentSHAs) == 1 {
		parent = c.ParentSHAs[0]
	}
	kinds, err := json.Marshal(c.Kinds)
	if err != nil {
		return err
	}
	// Legacy NULL parent also represents verified roots/merges. Fill only
	// unknown projections that agree with every retained known envelope.
	_, err = tx.ExecContext(ctx, `INSERT INTO commits(sha,parent_sha,authored_at,branch,files_changed,lines_added,lines_removed,kinds) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(sha) DO UPDATE SET
		parent_sha=CASE WHEN commits.parent_sha IS NULL AND excluded.parent_sha IS NOT NULL AND NOT EXISTS (
			SELECT 1 FROM ci_artifacts WHERE tested_sha=excluded.sha AND tested_parent_shas_json IS NOT NULL
			AND tested_parent_shas_json!=json_array(excluded.parent_sha)) THEN excluded.parent_sha ELSE commits.parent_sha END,
		kinds=CASE WHEN commits.kinds='null' AND excluded.kinds!='null' AND NOT EXISTS (
			SELECT 1 FROM ci_artifacts WHERE tested_sha=excluded.sha AND tested_kinds_json!='null'
			AND tested_kinds_json!=excluded.kinds) THEN excluded.kinds ELSE commits.kinds END`,
		obs.TestedSHA, parent, nullableTime(c.AuthoredAt), obs.Branch, c.FilesChanged, c.LinesAdded, c.LinesRemoved, string(kinds))
	return err
}

func persistTest(ctx context.Context, tx *sql.Tx, record ObservationCase, observed string) error {
	id := record.Identity
	first, last := observed, observed
	var priorFirst, priorLast string
	err := tx.QueryRowContext(ctx, "SELECT first_seen,last_seen FROM tests WHERE id=?", id.ID).Scan(&priorFirst, &priorLast)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		oldFirst, err := time.Parse(time.RFC3339Nano, priorFirst)
		if err != nil {
			return errors.New("invalid existing first observation time")
		}
		oldLast, err := time.Parse(time.RFC3339Nano, priorLast)
		if err != nil {
			return errors.New("invalid existing last observation time")
		}
		current, err := time.Parse(time.RFC3339Nano, observed)
		if err != nil {
			return err
		}
		if oldFirst.Before(current) {
			first = priorFirst
		}
		if oldLast.After(current) {
			last = priorLast
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO tests(id,lang,package,name,file,first_seen,last_seen,unstable_id) VALUES(?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET first_seen=excluded.first_seen,last_seen=excluded.last_seen,unstable_id=max(tests.unstable_id,excluded.unstable_id)`, id.ID, id.Language, id.Package, id.Name, id.File, first, last, id.Unstable)
	return err
}

// persistenceStats is a current snapshot; canonical counts reflect withdrawals.
func persistenceStats(ctx context.Context, db *sql.DB) (Stats, error) {
	s := Stats{AcquisitionExclusions: map[string]int{}}
	queries := []struct {
		query  string
		target *int
	}{
		{"SELECT count(DISTINCT run_id) FROM mining_runs", &s.InspectedRuns},
		{"SELECT count(*) FROM mining_runs WHERE attempt IS NOT NULL", &s.InspectedAttempts},
		{"SELECT count(DISTINCT unit_key) FROM ci_artifacts", &s.InspectedArtifacts},
		{"SELECT count(*) FROM ci_observations", &s.RawCases},
		{"SELECT count(*) FROM test_results WHERE (source,project) IN (SELECT provider,project FROM mining_sources)", &s.FactualResults},
		{"SELECT count(DISTINCT commit_sha) FROM test_results WHERE (source,project) IN (SELECT provider,project FROM mining_sources)", &s.FactualResultBearingCommits},
		{"SELECT coalesce(sum(replay_cases),0) FROM ci_artifacts", &s.ReplayDuplicates},
		{"SELECT count(*) FROM ci_artifacts WHERE state='capped'", &s.CappedUnits},
		{"SELECT count(*) FROM ci_artifacts WHERE state='incomplete'", &s.IncompleteUnits},
	}
	for _, q := range queries {
		if err := db.QueryRowContext(ctx, q.query).Scan(q.target); err != nil {
			return Stats{}, err
		}
	}
	var since, until, observedSince, observedUntil sql.NullString
	if err := db.QueryRowContext(ctx, "SELECT min(requested_since),max(requested_until) FROM mining_runs").Scan(&since, &until); err != nil {
		return Stats{}, err
	}
	if err := db.QueryRowContext(ctx, "SELECT min(observed_at),max(observed_at) FROM test_results WHERE (source,project) IN (SELECT provider,project FROM mining_sources)").Scan(&observedSince, &observedUntil); err != nil {
		return Stats{}, err
	}
	for _, field := range []struct {
		value  sql.NullString
		target **time.Time
	}{{observedSince, &s.ObservedSince}, {observedUntil, &s.ObservedUntil}} {
		if field.value.Valid {
			value, err := time.Parse(time.RFC3339Nano, field.value.String)
			if err != nil {
				return Stats{}, err
			}
			*field.target = &value
		}
	}
	if since.Valid {
		value, err := time.Parse(time.RFC3339Nano, since.String)
		if err != nil {
			return Stats{}, err
		}
		s.RequestedSince = value
	}
	if until.Valid {
		value, err := time.Parse(time.RFC3339Nano, until.String)
		if err != nil {
			return Stats{}, err
		}
		s.RequestedUntil = value
	}
	rows, err := db.QueryContext(ctx, "SELECT reason,count(*) FROM acquisition_exclusions GROUP BY reason ORDER BY reason")
	if err != nil {
		return Stats{}, err
	}
	for rows.Next() {
		var reason string
		var count int
		if err := rows.Scan(&reason, &count); err != nil {
			return Stats{}, errors.Join(err, rows.Close())
		}
		s.AcquisitionExclusions[reason] = count
	}
	return s, errors.Join(rows.Err(), rows.Close())
}
