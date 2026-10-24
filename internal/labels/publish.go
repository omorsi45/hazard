package labels

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var now = time.Now

const cutoffFormat = "2006-01-02T15:04:05Z"

func buildIdentity(provider, project string, cutoff time.Time, inputDigest string) string {
	return hashText(AlgorithmVersion + "\n" + provider + "\n" + project + "\n" + cutoff.UTC().Format(cutoffFormat) + "\n" + inputDigest)
}

type memberSnapshot struct {
	RunID         string  `json:"run_id"`
	JobID         *string `json:"job_id"`
	Attempt       *int    `json:"attempt"`
	ArtifactID    *string `json:"artifact_id"`
	Branch        string  `json:"branch"`
	TestedSHA     string  `json:"tested_sha"`
	EnvironmentID string  `json:"environment_id"`
	TestID        string  `json:"test_id"`
}

// publish writes one immutable build and moves the current marker to it, all
// inside the caller's transaction.
func publish(ctx context.Context, tx *sql.Tx, f *facts, cutoff time.Time, inputDigest string, decisions []*decision) (string, string, error) {
	decisionDigest, err := decisionDigest(decisions)
	if err != nil {
		return "", "", err
	}
	always, err := json.Marshal(alwaysRun(f, decisions))
	if err != nil {
		return "", "", err
	}
	id := buildIdentity(f.Provider, f.Project, cutoff, inputDigest)
	var existing, current int
	switch err := tx.QueryRowContext(ctx, "SELECT 1, is_current FROM label_builds WHERE build_id=?", id).Scan(&existing, &current); {
	case err == nil:
		// Identical semantic input and cutoff: the build is reused, never rewritten.
		if current == 0 {
			if err := moveCurrentMarker(ctx, tx, id); err != nil {
				return "", "", err
			}
		}
		return id, "", nil
	case !errors.Is(err, sql.ErrNoRows):
		return "", "", fmt.Errorf("look up build: %w", err)
	}
	exclusions, activeGaps := acquisitionAccounting(f)
	exclusionsJSON, err := json.Marshal(exclusions)
	if err != nil {
		return "", "", err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE label_builds SET is_current=0 WHERE is_current=1"); err != nil {
		return "", "", fmt.Errorf("clear current build: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO label_builds(build_id,provider,project,cutoff,algorithm_version,input_digest,decision_digest,always_run_json,acquisition_exclusions_json,active_gaps,created_at,is_current)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,1)`, id, f.Provider, f.Project, cutoff.UTC().Format(cutoffFormat), AlgorithmVersion, inputDigest, decisionDigest, string(always), string(exclusionsJSON), activeGaps,
		now().UTC().Format(time.RFC3339Nano)); err != nil {
		return "", "", fmt.Errorf("insert build: %w", err)
	}
	for _, d := range decisions {
		evidence, err := json.Marshal(d.Evidence)
		if err != nil {
			return "", "", err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO label_candidates(build_id,candidate_key,kind,state,tested_sha,test_id,environment_id,branch,member_count,evidence_json)
			VALUES(?,?,?,?,?,?,?,?,?,?)`, id, d.Key, d.Kind, d.State, nullIfEmpty(d.SHA), nullIfEmpty(d.TestID), nullIfEmpty(d.Env), nullIfEmpty(d.Branch), len(d.Members), string(evidence)); err != nil {
			return "", "", fmt.Errorf("insert candidate: %w", err)
		}
		for _, r := range d.Reasons {
			refs, err := json.Marshal(append([]string{}, r.Evidence...))
			if err != nil {
				return "", "", err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO label_candidate_exclusions(build_id,candidate_key,reason,effect,evidence_json) VALUES(?,?,?,?,?)`,
				id, d.Key, r.Name, r.Effect, string(refs)); err != nil {
				return "", "", fmt.Errorf("insert reason: %w", err)
			}
		}
		for _, m := range d.Members {
			snapshot, err := json.Marshal(memberSnapshot{RunID: m.art.RunID, JobID: m.art.JobID, Attempt: m.art.Attempt, ArtifactID: m.art.ArtifactID,
				Branch: m.art.Branch, TestedSHA: d.SHA, EnvironmentID: d.Env, TestID: d.TestID})
			if err != nil {
				return "", "", err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO label_candidate_members(build_id,provider,project,unit_key,content_sha256,artifact_entry,case_ordinal,candidate_key,status,observed_at,snapshot_json)
				VALUES(?,?,?,?,?,?,?,?,?,?,?)`, id, m.Provider, m.Project, m.UnitKey, m.ContentSHA256, m.ArtifactEntry, m.CaseOrdinal, d.Key, m.Status, m.ObservedAt, string(snapshot)); err != nil {
				return "", "", fmt.Errorf("insert member: %w", err)
			}
		}
	}
	return id, decisionDigest, nil
}

// moveCurrentMarker makes id the single current build.
func moveCurrentMarker(ctx context.Context, tx *sql.Tx, id string) error {
	if _, err := tx.ExecContext(ctx, "UPDATE label_builds SET is_current=0 WHERE is_current=1"); err != nil {
		return fmt.Errorf("clear current build: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE label_builds SET is_current=1 WHERE build_id=?", id); err != nil {
		return fmt.Errorf("set current build: %w", err)
	}
	return nil
}

// acquisitionAccounting counts acquisition exclusions by reason and active gaps.
func acquisitionAccounting(f *facts) (map[string]int, int) {
	byReason := map[string]int{}
	for _, e := range f.Exclusions {
		byReason[e.Reason]++
	}
	active := 0
	for _, g := range f.Gaps {
		if g.Active {
			active++
		}
	}
	return byReason, active
}

// readReport rebuilds the Report of a stored build from its immutable rows.
func readReport(ctx context.Context, q querier, id, freshness string) (Report, error) {
	r := newReport()
	var always, exclusions string
	err := q.QueryRowContext(ctx, `SELECT build_id,cutoff,algorithm_version,input_digest,decision_digest,always_run_json,acquisition_exclusions_json,active_gaps
		FROM label_builds WHERE build_id=?`, id).Scan(&r.BuildID, &r.Cutoff, &r.AlgorithmVersion, &r.InputDigest, &r.DecisionDigest, &always, &exclusions, &r.ActiveGaps)
	if err != nil {
		return Report{}, fmt.Errorf("read build: %w", err)
	}
	r.Freshness = freshness
	rows, err := q.QueryContext(ctx, `SELECT kind,state,count(*) FROM label_candidates WHERE build_id=? GROUP BY kind,state`, id)
	if err != nil {
		return Report{}, fmt.Errorf("read candidate counts: %w", err)
	}
	for rows.Next() {
		var kind, state string
		var n int
		if err := rows.Scan(&kind, &state, &n); err != nil {
			return Report{}, closeWith(rows, err)
		}
		if kind == kindCandidate {
			r.RawCandidates += n
			switch state {
			case stateExcluded:
				r.ExcludedCandidates += n
			case statePending:
				r.PendingCandidates += n
			case stateWeak:
				r.WeakCandidates += n
			case stateStrong:
				r.StrongCandidates += n
			}
			continue
		}
		switch state {
		case stateExcluded:
			r.UnresolvedExcluded += n
		case statePending:
			r.UnresolvedPending += n
		}
	}
	if err := closeWith(rows, rows.Err()); err != nil {
		return Report{}, fmt.Errorf("read candidate counts: %w", err)
	}
	rows, err = q.QueryContext(ctx, `SELECT c.kind,count(*) FROM label_candidate_members m
		JOIN label_candidates c ON c.build_id=m.build_id AND c.candidate_key=m.candidate_key WHERE m.build_id=? GROUP BY c.kind`, id)
	if err != nil {
		return Report{}, fmt.Errorf("read member counts: %w", err)
	}
	for rows.Next() {
		var kind string
		var n int
		if err := rows.Scan(&kind, &n); err != nil {
			return Report{}, closeWith(rows, err)
		}
		if kind == kindCandidate {
			r.GroupedMemberCount = n
		} else {
			r.UnresolvedRawMembers = n
		}
	}
	if err := closeWith(rows, rows.Err()); err != nil {
		return Report{}, fmt.Errorf("read member counts: %w", err)
	}
	r.RawFailingCases = r.GroupedMemberCount + r.UnresolvedRawMembers
	rows, err = q.QueryContext(ctx, `SELECT c.kind,e.reason,count(*) FROM label_candidate_exclusions e
		JOIN label_candidates c ON c.build_id=e.build_id AND c.candidate_key=e.candidate_key WHERE e.build_id=? GROUP BY c.kind,e.reason`, id)
	if err != nil {
		return Report{}, fmt.Errorf("read reason counts: %w", err)
	}
	for rows.Next() {
		var kind, name string
		var n int
		if err := rows.Scan(&kind, &name, &n); err != nil {
			return Report{}, closeWith(rows, err)
		}
		if kind == kindCandidate {
			r.CandidateReasonCounts[name] = n
		} else {
			r.UnresolvedReasonCounts[name] = n
		}
	}
	if err := closeWith(rows, rows.Err()); err != nil {
		return Report{}, fmt.Errorf("read reason counts: %w", err)
	}
	rows, err = q.QueryContext(ctx, `SELECT m.provider,m.project,count(DISTINCT c.tested_sha) FROM label_candidates c
		JOIN label_candidate_members m ON m.build_id=c.build_id AND m.candidate_key=c.candidate_key
		WHERE c.build_id=? AND c.kind='candidate' AND c.state='strong' GROUP BY m.provider,m.project`, id)
	if err != nil {
		return Report{}, fmt.Errorf("read strong commits: %w", err)
	}
	for rows.Next() {
		var provider, project string
		var n int
		if err := rows.Scan(&provider, &project, &n); err != nil {
			return Report{}, closeWith(rows, err)
		}
		r.StrongCommitsByRepository[provider+"/"+project] = n
	}
	if err := closeWith(rows, rows.Err()); err != nil {
		return Report{}, fmt.Errorf("read strong commits: %w", err)
	}
	if err := errors.Join(json.Unmarshal([]byte(always), &r.AlwaysRunTests), json.Unmarshal([]byte(exclusions), &r.AcquisitionExclusionsByReason)); err != nil {
		return Report{}, fmt.Errorf("decode build summary: %w", err)
	}
	r.Limitations = limitations(r)
	return r, nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func newReport() Report {
	return Report{
		CandidateReasonCounts: map[string]int{}, UnresolvedReasonCounts: map[string]int{}, StrongCommitsByRepository: map[string]int{},
		AlwaysRunTests: []string{}, AcquisitionExclusionsByReason: map[string]int{}, Limitations: []string{},
	}
}

func limitations(r Report) []string {
	if r.Freshness == FreshnessUnavailable {
		return []string{"no current label build exists: label eligibility is unavailable, which is not the same as zero regressions; run labels build with a cutoff"}
	}
	out := []string{
		"label counts describe acquired CI history only; an empty result is not evidence of no regressions",
		"provider retained-history limits are not measured by this build",
		"the cutoff selects source-event witnesses by their observation time using current provenance, not a historical database snapshot",
	}
	if r.Freshness == FreshnessStale {
		out = append(out, "this build is stale: acquisition, provenance, quarantine or policy inputs changed after it was built; rebuild before any calibration")
	}
	if r.ActiveGaps > 0 {
		out = append(out, fmt.Sprintf("%d active acquisition gap(s): scoped gaps keep affected candidates pending and a discovery gap blocks strong attribution", r.ActiveGaps))
	}
	if r.UnresolvedRawMembers > 0 {
		out = append(out, fmt.Sprintf("%d failing case(s) lack tested revision, test identity or environment facts and are never calibration units", r.UnresolvedRawMembers))
	}
	if r.StrongCandidates == 0 {
		out = append(out, "no strong candidates: eligible recall and its confidence interval are N/A, not perfect")
	}
	return out
}
