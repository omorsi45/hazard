package labels

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// querier is satisfied by *sql.DB and *sql.Tx.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type artifactKey struct{ Provider, Project, UnitKey, ContentSHA256 string }

type obsKey struct {
	artifactKey
	ArtifactEntry string
	CaseOrdinal   int
}

// ref is the opaque evidence reference of a raw observation.
func (k obsKey) ref() string {
	return fmt.Sprintf("%s/%s#%s#%d", k.UnitKey, k.ContentSHA256, k.ArtifactEntry, k.CaseOrdinal)
}

// artifact holds the mutable provenance of one acquired report. NULL and empty
// values stay distinct (pointers) so the digest preserves that difference.
type artifact struct {
	artifactKey
	RunID                  string
	ArtifactID, JobID      *string
	Attempt                *int
	EnvironmentID          *string
	TestedSHA              *string
	TestedRevisionEvidence *string
	ExecutionEvidence      *string
	EnvironmentEvidence    *string
	SourceHeadSHA, Branch  string
	TestedParentsJSON      *string
	SuiteIncomplete        bool
	DiagnosticsTruncated   bool
	State, ObservedAt      string
}

type observation struct {
	obsKey
	Status, TestID, IdentityError         string
	Unstable                              bool
	InfraReason, MessageSHA256            string
	SuiteIncomplete, DiagnosticsTruncated bool
	ObservedAt                            string
	// Promoted reports that acquisition keyed this record to a canonical
	// test_results row that still exists. Collided, conflicted or withheld
	// records have none and cannot serve as comparison witnesses.
	Promoted bool

	message string
	art     *artifact
}

// gap is the semantic content of an acquisition gap. Local first/last
// encounter times, resolution timestamps and episode ids are audit links only.
type gap struct {
	Provider, Project, RunID, UnitKey, Reason, Detail string
	JobID, ArtifactID                                 *string
	Attempt                                           *int
	SourceTime, ContentSHA256                         *string
	Active                                            bool
}

type acquisitionExclusion struct {
	obsKey
	Reason, Detail string
}

// flaggedTest is a manual inventory annotation that affects eligibility.
type flaggedTest struct {
	ID                      string
	Quarantined, UnstableID bool
}

// run is the semantic content of a mining run attempt. The local observation
// clock is acquisition bookkeeping and is not read.
type run struct {
	Provider, Project, RunID, AttemptKey string
	Attempt                              *int
	SourceHeadSHA, Branch, State         string
	CreatedAt                            *string
}

// requestedWindow is one distinct acquisition window recorded on mining runs.
// NULL and empty text stay distinct, like every other provenance fact.
type requestedWindow struct{ Since, Until *string }

// facts is every classification input read from one consistent snapshot.
type facts struct {
	Provider, Project string
	Artifacts         []*artifact
	Observations      []*observation
	Runs              []*run
	Windows           []*requestedWindow
	Gaps              []*gap
	Exclusions        []*acquisitionExclusion
	Flagged           []*flaggedTest
}

func known(s *string) bool { return s != nil && strings.TrimSpace(*s) != "" }

func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid stored timestamp %q: %w", s, err)
	}
	return t.UTC(), nil
}

func hashText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func nullString(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	return &n.String
}

// windowKey keeps NULL and empty text distinct when deduplicating windows.
func windowKey(n sql.NullString) string {
	if !n.Valid {
		return "null"
	}
	return "text:" + n.String
}

func nullInt(n sql.NullInt64) *int {
	if !n.Valid {
		return nil
	}
	v := int(n.Int64)
	return &v
}

// loadFacts reads the bound source, every artifact referenced by an observation
// before the cutoff, and those observations. The cutoff selects source-event
// witnesses by their source observation time using current knowledge.
func loadFacts(ctx context.Context, q querier, cutoff time.Time) (_ *facts, err error) {
	f := &facts{}
	err = q.QueryRowContext(ctx, "SELECT provider,project FROM mining_sources WHERE singleton=1").Scan(&f.Provider, &f.Project)
	if err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("read mining source: %w", err)
	}
	all := map[artifactKey]*artifact{}
	rows, err := q.QueryContext(ctx, `SELECT provider,project,unit_key,content_sha256,run_id,artifact_id,job_id,attempt,environment_id,tested_sha,
		tested_revision_evidence,execution_evidence,environment_evidence,source_head_sha,branch,tested_parent_shas_json,suite_incomplete,diagnostics_truncated,state,observed_at
		FROM ci_artifacts ORDER BY provider,project,unit_key,content_sha256`)
	if err != nil {
		return nil, fmt.Errorf("read artifacts: %w", err)
	}
	for rows.Next() {
		a := &artifact{}
		var artifactID, jobID, env, sha, revEvidence, execEvidence, envEvidence, parents sql.NullString
		var attempt sql.NullInt64
		if err := rows.Scan(&a.Provider, &a.Project, &a.UnitKey, &a.ContentSHA256, &a.RunID, &artifactID, &jobID, &attempt, &env, &sha,
			&revEvidence, &execEvidence, &envEvidence, &a.SourceHeadSHA, &a.Branch, &parents, &a.SuiteIncomplete, &a.DiagnosticsTruncated, &a.State, &a.ObservedAt); err != nil {
			return nil, closeWith(rows, fmt.Errorf("scan artifact: %w", err))
		}
		a.ArtifactID, a.JobID, a.Attempt = nullString(artifactID), nullString(jobID), nullInt(attempt)
		a.EnvironmentID, a.TestedSHA, a.TestedRevisionEvidence = nullString(env), nullString(sha), nullString(revEvidence)
		a.ExecutionEvidence, a.EnvironmentEvidence, a.TestedParentsJSON = nullString(execEvidence), nullString(envEvidence), nullString(parents)
		all[a.artifactKey] = a
	}
	if err := closeWith(rows, rows.Err()); err != nil {
		return nil, fmt.Errorf("read artifacts: %w", err)
	}

	rows, err = q.QueryContext(ctx, `SELECT o.provider,o.project,o.unit_key,o.content_sha256,o.artifact_entry,o.case_ordinal,o.status,o.test_id,o.identity_error,o.unstable_id,
		o.infra_reason,o.raw_message,o.suite_incomplete,o.diagnostics_truncated,o.observed_at,
		(o.result_key IS NOT NULL AND EXISTS(SELECT 1 FROM ci_artifacts a JOIN test_results r ON r.source=o.provider AND r.project=o.project AND r.commit_sha=a.tested_sha
			AND r.test_id=o.test_id AND r.pipeline_id=a.run_id AND r.job_id=a.job_id AND r.environment_id=a.environment_id AND r.attempt=coalesce(a.attempt,1) AND r.artifact_id=coalesce(a.artifact_id,'')
			WHERE a.provider=o.provider AND a.project=o.project AND a.unit_key=o.unit_key AND a.content_sha256=o.content_sha256))
		FROM ci_observations o
		ORDER BY o.provider,o.project,o.unit_key,o.content_sha256,o.artifact_entry,o.case_ordinal`)
	if err != nil {
		return nil, fmt.Errorf("read observations: %w", err)
	}
	used := map[artifactKey]bool{}
	for rows.Next() {
		o := &observation{}
		var message string
		if err := rows.Scan(&o.Provider, &o.Project, &o.UnitKey, &o.ContentSHA256, &o.ArtifactEntry, &o.CaseOrdinal, &o.Status, &o.TestID, &o.IdentityError,
			&o.Unstable, &o.InfraReason, &message, &o.SuiteIncomplete, &o.DiagnosticsTruncated, &o.ObservedAt, &o.Promoted); err != nil {
			return nil, closeWith(rows, fmt.Errorf("scan observation: %w", err))
		}
		at, err := parseTime(o.ObservedAt)
		if err != nil {
			return nil, closeWith(rows, err)
		}
		if !at.Before(cutoff) {
			continue
		}
		o.art = all[o.artifactKey]
		if o.art == nil {
			return nil, closeWith(rows, fmt.Errorf("observation %s has no artifact", o.ref()))
		}
		if o.Status == "fail" || o.Status == "error" {
			o.message = strings.ToLower(message)
			o.MessageSHA256 = hashText(message)
		}
		used[o.artifactKey] = true
		f.Observations = append(f.Observations, o)
	}
	if err := closeWith(rows, rows.Err()); err != nil {
		return nil, fmt.Errorf("read observations: %w", err)
	}
	for _, o := range f.Observations {
		if used[o.artifactKey] {
			f.Artifacts = append(f.Artifacts, o.art)
			delete(used, o.artifactKey)
		}
	}
	if err := f.loadAnnotations(ctx, q, cutoff); err != nil {
		return nil, err
	}
	return f, nil
}

// loadAnnotations reads gaps, acquisition exclusions and manual test flags.
// None of them is filtered by the cutoff: a gap or flag that may be relevant
// cannot be silently dropped because its local time is unknown. Acquisition
// exclusions belong to a source report, so they follow that report's time.
func (f *facts) loadAnnotations(ctx context.Context, q querier, cutoff time.Time) error {
	rows, err := q.QueryContext(ctx, `SELECT provider,project,run_id,attempt_key,attempt,source_head_sha,branch,created_at,requested_since,requested_until,state FROM mining_runs
		ORDER BY provider,project,run_id,attempt_key`)
	if err != nil {
		return fmt.Errorf("read mining runs: %w", err)
	}
	windows := map[[2]string]bool{}
	for rows.Next() {
		r := &run{}
		var attempt sql.NullInt64
		var created, since, until sql.NullString
		if err := rows.Scan(&r.Provider, &r.Project, &r.RunID, &r.AttemptKey, &attempt, &r.SourceHeadSHA, &r.Branch, &created, &since, &until, &r.State); err != nil {
			return closeWith(rows, fmt.Errorf("scan mining run: %w", err))
		}
		r.Attempt, r.CreatedAt = nullInt(attempt), nullString(created)
		f.Runs = append(f.Runs, r)
		// The window is classification input only as the set of distinct
		// windows, so which attempt carries which request does not matter.
		if key := [2]string{windowKey(since), windowKey(until)}; !windows[key] {
			windows[key] = true
			f.Windows = append(f.Windows, &requestedWindow{Since: nullString(since), Until: nullString(until)})
		}
	}
	if err := closeWith(rows, rows.Err()); err != nil {
		return fmt.Errorf("read mining runs: %w", err)
	}
	rows, err = q.QueryContext(ctx, `SELECT provider,project,run_id,unit_key,reason,detail,job_id,artifact_id,attempt,source_time,content_sha256,resolved_at IS NULL
		FROM acquisition_gaps ORDER BY provider,project,run_id,unit_key,reason,coalesce(content_sha256,''),id`)
	if err != nil {
		return fmt.Errorf("read acquisition gaps: %w", err)
	}
	for rows.Next() {
		g := &gap{}
		var jobID, artifactID, sourceTime, content sql.NullString
		var attempt sql.NullInt64
		if err := rows.Scan(&g.Provider, &g.Project, &g.RunID, &g.UnitKey, &g.Reason, &g.Detail, &jobID, &artifactID, &attempt, &sourceTime, &content, &g.Active); err != nil {
			return closeWith(rows, fmt.Errorf("scan acquisition gap: %w", err))
		}
		g.JobID, g.ArtifactID, g.Attempt, g.SourceTime, g.ContentSHA256 = nullString(jobID), nullString(artifactID), nullInt(attempt), nullString(sourceTime), nullString(content)
		f.Gaps = append(f.Gaps, g)
	}
	if err := closeWith(rows, rows.Err()); err != nil {
		return fmt.Errorf("read acquisition gaps: %w", err)
	}
	rows, err = q.QueryContext(ctx, `SELECT e.provider,e.project,e.unit_key,e.content_sha256,e.artifact_entry,e.case_ordinal,e.reason,e.detail,a.observed_at
		FROM acquisition_exclusions e JOIN ci_artifacts a ON a.provider=e.provider AND a.project=e.project AND a.unit_key=e.unit_key AND a.content_sha256=e.content_sha256
		ORDER BY e.provider,e.project,e.unit_key,e.content_sha256,e.artifact_entry,e.case_ordinal,e.reason`)
	if err != nil {
		return fmt.Errorf("read acquisition exclusions: %w", err)
	}
	for rows.Next() {
		e := &acquisitionExclusion{}
		var reportTime string
		if err := rows.Scan(&e.Provider, &e.Project, &e.UnitKey, &e.ContentSHA256, &e.ArtifactEntry, &e.CaseOrdinal, &e.Reason, &e.Detail, &reportTime); err != nil {
			return closeWith(rows, fmt.Errorf("scan acquisition exclusion: %w", err))
		}
		at, err := parseTime(reportTime)
		if err != nil {
			return closeWith(rows, err)
		}
		if at.Before(cutoff) {
			f.Exclusions = append(f.Exclusions, e)
		}
	}
	if err := closeWith(rows, rows.Err()); err != nil {
		return fmt.Errorf("read acquisition exclusions: %w", err)
	}
	f.Flagged, err = readFlagged(ctx, q)
	return err
}

// readFlagged reads the manual inventory flags that force a test to always run.
func readFlagged(ctx context.Context, q querier) ([]*flaggedTest, error) {
	rows, err := q.QueryContext(ctx, `SELECT id,quarantined,unstable_id FROM tests WHERE quarantined!=0 OR unstable_id!=0 ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("read test flags: %w", err)
	}
	var out []*flaggedTest
	for rows.Next() {
		t := &flaggedTest{}
		if err := rows.Scan(&t.ID, &t.Quarantined, &t.UnstableID); err != nil {
			return nil, closeWith(rows, fmt.Errorf("scan test flag: %w", err))
		}
		out = append(out, t)
	}
	if err := closeWith(rows, rows.Err()); err != nil {
		return nil, fmt.Errorf("read test flags: %w", err)
	}
	return out, nil
}

func closeWith(rows *sql.Rows, err error) error {
	if closeErr := rows.Close(); closeErr != nil && err == nil {
		return closeErr
	}
	return err
}
