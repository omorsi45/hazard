// Package labels freezes a regression-label cohort from acquired CI history.
//
// Build groups every failing raw case into a known attribution candidate or an
// unresolved record, classifies each one as pending, excluded, weak or strong
// from explicit predicates, and publishes the result atomically as an immutable
// build. ReadStats reports the current build without rebuilding.
package labels

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"
)

// AlgorithmVersion identifies the classification policy. Changing the policy
// must change this value so older builds report as stale.
const AlgorithmVersion = "labels/1"

// Freshness values of a Report.
const (
	FreshnessCurrent     = "current"
	FreshnessStale       = "stale"
	FreshnessUnavailable = "unavailable"
)

// Options selects the evaluation cutoff. Before is an exclusive UTC instant
// applied to source observation timestamps, never to acquisition clocks. It must
// be exactly a UTC midnight, the granularity the stored cutoff and the build
// identity keep, so a freshly built cohort never reads as stale.
type Options struct{ Before time.Time }

// Report is the count and identity summary of one frozen build.
type Report struct {
	BuildID          string `json:"build_id"`
	Cutoff           string `json:"cutoff"`
	AlgorithmVersion string `json:"algorithm_version"`
	InputDigest      string `json:"input_digest"`
	DecisionDigest   string `json:"decision_digest"`
	Freshness        string `json:"freshness"`

	RawFailingCases      int `json:"raw_failing_cases"`
	GroupedMemberCount   int `json:"grouped_member_count"`
	UnresolvedRawMembers int `json:"unresolved_raw_members"`
	UnresolvedPending    int `json:"unresolved_pending"`
	UnresolvedExcluded   int `json:"unresolved_excluded"`

	RawCandidates      int `json:"raw_candidates"`
	ExcludedCandidates int `json:"excluded_candidates"`
	PendingCandidates  int `json:"pending_candidates"`
	WeakCandidates     int `json:"weak_candidates"`
	StrongCandidates   int `json:"strong_candidates"`

	CandidateReasonCounts         map[string]int `json:"candidate_reason_counts"`
	UnresolvedReasonCounts        map[string]int `json:"unresolved_reason_counts"`
	StrongCommitsByRepository     map[string]int `json:"strong_commits_by_repository"`
	AlwaysRunTests                []string       `json:"always_run_tests"`
	AcquisitionExclusionsByReason map[string]int `json:"acquisition_exclusions_by_reason"`
	ActiveGaps                    int            `json:"active_gaps"`
	Limitations                   []string       `json:"limitations"`
}

// Build freezes the cohort for opts.Before and publishes it as the current build.
func Build(ctx context.Context, db *sql.DB, opts Options) (_ Report, err error) {
	if opts.Before.IsZero() {
		return Report{}, errors.New("labels: Before cutoff is required")
	}
	before := opts.Before.UTC()
	if hour, minute, second := before.Clock(); hour != 0 || minute != 0 || second != 0 || before.Nanosecond() != 0 {
		return Report{}, fmt.Errorf("labels: Before cutoff must be a UTC midnight, got %s", before.Format(time.RFC3339Nano))
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Report{}, fmt.Errorf("begin label build: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("rollback label build: %w", rollbackErr))
		}
	}()
	f, err := loadFacts(ctx, tx, before)
	if err != nil {
		return Report{}, err
	}
	inputDigest, err := f.inputDigest()
	if err != nil {
		return Report{}, err
	}
	id, err := publish(ctx, tx, f, before, inputDigest, classify(f))
	if err != nil {
		return Report{}, err
	}
	report, err := readReport(ctx, tx, id, FreshnessCurrent)
	if err != nil {
		return Report{}, err
	}
	if err := tx.Commit(); err != nil {
		return Report{}, fmt.Errorf("commit label build: %w", err)
	}
	return report, nil
}

// ReadStats reports the current build and whether it is still fresh. It never
// writes: a changed semantic input reports stale (with the prior frozen build
// identity and counts), and a missing build reports unavailable rather than zero.
func ReadStats(ctx context.Context, db *sql.DB) (_ Report, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Report{}, fmt.Errorf("begin label stats: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("rollback label stats: %w", rollbackErr))
		}
	}()
	var id, cutoffText, algorithm, inputDigest, provider, project string
	err = tx.QueryRowContext(ctx, "SELECT build_id,cutoff,algorithm_version,input_digest,provider,project FROM label_builds WHERE is_current=1").
		Scan(&id, &cutoffText, &algorithm, &inputDigest, &provider, &project)
	if errors.Is(err, sql.ErrNoRows) {
		return unavailableReport(ctx, tx)
	}
	if err != nil {
		return Report{}, fmt.Errorf("read current build: %w", err)
	}
	cutoff, err := time.Parse(cutoffFormat, cutoffText)
	if err != nil {
		return Report{}, fmt.Errorf("invalid stored cutoff %q: %w", cutoffText, err)
	}
	f, err := loadFacts(ctx, tx, cutoff)
	if err != nil {
		return Report{}, err
	}
	digest, err := f.inputDigest()
	if err != nil {
		return Report{}, err
	}
	freshness := FreshnessCurrent
	if algorithm != AlgorithmVersion || digest != inputDigest || provider != f.Provider || project != f.Project {
		freshness = FreshnessStale
	}
	report, err := readReport(ctx, tx, id, freshness)
	if err != nil {
		return Report{}, err
	}
	if freshness == FreshnessStale {
		// The frozen set may predate a manual flag, which is often the very edit
		// that made the build stale. Never report fewer always-run tests than the
		// live inventory flags demand.
		report.AlwaysRunTests = unionSorted(report.AlwaysRunTests, flaggedIDs(f.Flagged))
	}
	return report, nil
}

// flaggedIDs lists the IDs of manually quarantined or unstable-identity tests.
func flaggedIDs(flagged []*flaggedTest) []string {
	ids := make([]string, 0, len(flagged))
	for _, t := range flagged {
		ids = append(ids, t.ID)
	}
	return ids
}

// unionSorted returns the sorted, de-duplicated union of two ID lists.
func unionSorted(a, b []string) []string {
	set := make(map[string]bool, len(a)+len(b))
	for _, id := range append(append([]string{}, a...), b...) {
		set[id] = true
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// unavailableReport reports live acquisition facts and manual always-run flags
// only. Eligibility counts are zero because no cohort exists, which Freshness
// and Limitations say explicitly. Always-run membership is independent of label
// eligibility, so the readable manual flags are still listed.
func unavailableReport(ctx context.Context, q querier) (Report, error) {
	r := newReport()
	r.Freshness = FreshnessUnavailable
	flagged, err := readFlagged(ctx, q)
	if err != nil {
		return Report{}, err
	}
	r.AlwaysRunTests = unionSorted(nil, flaggedIDs(flagged))
	if err := q.QueryRowContext(ctx, "SELECT count(*) FROM acquisition_gaps WHERE resolved_at IS NULL").Scan(&r.ActiveGaps); err != nil {
		return Report{}, fmt.Errorf("count active gaps: %w", err)
	}
	rows, err := q.QueryContext(ctx, "SELECT reason,count(*) FROM acquisition_exclusions GROUP BY reason")
	if err != nil {
		return Report{}, fmt.Errorf("count acquisition exclusions: %w", err)
	}
	for rows.Next() {
		var reason string
		var n int
		if err := rows.Scan(&reason, &n); err != nil {
			return Report{}, closeWith(rows, err)
		}
		r.AcquisitionExclusionsByReason[reason] = n
	}
	if err := closeWith(rows, rows.Err()); err != nil {
		return Report{}, fmt.Errorf("count acquisition exclusions: %w", err)
	}
	r.Limitations = limitations(r)
	return r, nil
}
