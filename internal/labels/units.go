package labels

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// reasonCommitTimeUnknown names a strong commit whose authored time is
// missing, unparseable or contradictory across the acquired reports.
const reasonCommitTimeUnknown = "commit_time_unknown"

// Unit is one calibration unit: a commit that at least one strong regression
// label blames, with the facts the replay and the split need.
type Unit struct {
	ID         string    // "ci:<tested sha>", the event ID training.json lists
	SHA        string    // the tested commit
	ParentSHA  string    // its single verified parent, the base of the replay
	CommitTime time.Time // UTC authored time of the commit
	Repository string    // provider/project, like Report.StrongCommitsByRepository
	Failing    []string  // sorted test IDs of the strong labels of the commit
}

// UnitReport is the calibration cohort of the current label build.
type UnitReport struct {
	BuildID   string
	Cutoff    string
	Freshness string
	Units     []Unit         // sorted by ID; empty unless Freshness is current
	Unusable  map[string]int // strong commits that cannot become a unit, by reason
}

// StrongUnits reads the calibration cohort of the current label build: one
// unit per commit that has at least one strong candidate. Weak, pending and
// excluded candidates, unresolved records and everything outside the label
// build (mutation records included) are never read, so none of them can count
// toward a calibration. A build that is not current (stale or unavailable) has
// no units: its freshness is reported and nothing is counted.
func StrongUnits(ctx context.Context, db *sql.DB) (UnitReport, error) {
	stats, err := ReadStats(ctx, db)
	if err != nil {
		return UnitReport{}, err
	}
	report := UnitReport{BuildID: stats.BuildID, Cutoff: stats.Cutoff, Freshness: stats.Freshness, Units: []Unit{}, Unusable: map[string]int{}}
	if stats.Freshness != FreshnessCurrent {
		return report, nil
	}

	// Everything below reads the build ReadStats described, by its ID, so the
	// freshness checked above belongs to the rows counted here.
	var provider, project string
	if err := db.QueryRowContext(ctx, `SELECT provider,project FROM label_builds WHERE build_id=?`, stats.BuildID).Scan(&provider, &project); err != nil {
		return UnitReport{}, fmt.Errorf("read build source: %w", err)
	}
	units, order, err := strongCommits(ctx, db, stats.BuildID, provider+"/"+project)
	if err != nil {
		return UnitReport{}, err
	}
	times, err := authoredTimes(ctx, db, stats.BuildID, provider, project)
	if err != nil {
		return UnitReport{}, err
	}
	for _, sha := range order {
		u := units[sha]
		at, ok := times[sha]
		if !ok {
			report.Unusable[reasonCommitTimeUnknown]++
			continue
		}
		u.CommitTime = at
		report.Units = append(report.Units, *u)
	}
	sort.Slice(report.Units, func(i, j int) bool { return report.Units[i].ID < report.Units[j].ID })
	return report, nil
}

// strongCommits groups the strong candidates of a build by tested commit. The
// parent comes from the evidence the classification kept for the candidate.
func strongCommits(ctx context.Context, db *sql.DB, buildID, repository string) (map[string]*Unit, []string, error) {
	rows, err := db.QueryContext(ctx, `SELECT tested_sha,test_id,evidence_json FROM label_candidates
		WHERE build_id=? AND kind=? AND state=? ORDER BY tested_sha,test_id`, buildID, kindCandidate, stateStrong)
	if err != nil {
		return nil, nil, fmt.Errorf("read strong candidates: %w", err)
	}
	units := map[string]*Unit{}
	var order []string
	for rows.Next() {
		var sha, test, evidenceJSON string
		if err := rows.Scan(&sha, &test, &evidenceJSON); err != nil {
			return nil, nil, closeWith(rows, fmt.Errorf("scan strong candidate: %w", err))
		}
		var evidence candidateEvidence
		if err := json.Unmarshal([]byte(evidenceJSON), &evidence); err != nil {
			return nil, nil, closeWith(rows, fmt.Errorf("decode evidence of %s: %w", sha, err))
		}
		u, ok := units[sha]
		if !ok {
			u = &Unit{ID: "ci:" + sha, SHA: sha, ParentSHA: evidence.ParentSHA, Repository: repository}
			units[sha] = u
			order = append(order, sha)
		}
		u.Failing = append(u.Failing, test)
	}
	if err := closeWith(rows, rows.Err()); err != nil {
		return nil, nil, fmt.Errorf("read strong candidates: %w", err)
	}
	return units, order, nil
}

// authoredTimes returns the authored time of every strong commit that has
// exactly one distinct, parseable time across its acquired reports. A commit
// with none, or with contradictory ones, is absent: it cannot be placed.
func authoredTimes(ctx context.Context, db *sql.DB, buildID, provider, project string) (map[string]time.Time, error) {
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT tested_sha,tested_authored_at FROM ci_artifacts
		WHERE provider=? AND project=? AND tested_authored_at IS NOT NULL
		AND tested_sha IN (SELECT tested_sha FROM label_candidates WHERE build_id=? AND kind=? AND state=?)`,
		provider, project, buildID, kindCandidate, stateStrong)
	if err != nil {
		return nil, fmt.Errorf("read authored times: %w", err)
	}
	texts := map[string][]string{}
	for rows.Next() {
		var sha, text string
		if err := rows.Scan(&sha, &text); err != nil {
			return nil, closeWith(rows, fmt.Errorf("scan authored time: %w", err))
		}
		texts[sha] = append(texts[sha], text)
	}
	if err := closeWith(rows, rows.Err()); err != nil {
		return nil, fmt.Errorf("read authored times: %w", err)
	}
	out := make(map[string]time.Time, len(texts))
	for sha, list := range texts {
		if len(list) != 1 {
			continue // contradictory reports: the commit cannot be placed
		}
		if at, err := parseTime(list[0]); err == nil {
			out[sha] = at // an unparseable time leaves the commit unplaced, which the caller reports
		}
	}
	return out, nil
}
