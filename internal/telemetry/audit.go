package telemetry

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/omorsi45/hazard/internal/runner"
	"github.com/omorsi45/hazard/internal/store"
)

// ErrSinceNotFound means --since-run named a selection that is not in the
// telemetry file.
var ErrSinceNotFound = errors.New("selection not found in telemetry")

// State is the outcome of running one test at one commit.
type State int

// Probe outcomes. Unknown means the test could not be judged there (the code
// did not build, the runner failed): it never counts as a pass.
const (
	StateUnknown State = iota
	StatePass
	StateFail
)

// Since marks where an audit starts: a selection ID or a time. Exactly one is set.
type Since struct {
	ID   string
	Time time.Time
}

// ParseSince reads the --since-run argument: an RFC 3339 time, else a selection ID.
func ParseSince(arg string) (Since, error) {
	if arg == "" {
		return Since{}, errors.New("empty --since-run: want a selection ID or an RFC 3339 time")
	}
	if t, err := time.Parse(time.RFC3339, arg); err == nil {
		return Since{Time: t}, nil
	}
	return Since{ID: arg}, nil
}

// SinceRecords returns the records from since, inclusive, in file order. A
// selection ID starts at that record's position; a time keeps the records made
// at or after it.
func SinceRecords(records []Record, since Since) ([]Record, error) {
	if since.ID != "" {
		for i, r := range records {
			if r.ID == since.ID {
				return records[i:], nil
			}
		}
		return nil, fmt.Errorf("%w: %s", ErrSinceNotFound, since.ID)
	}
	var out []Record
	for _, r := range records {
		at, err := time.Parse(time.RFC3339, r.Time)
		if err != nil {
			return nil, fmt.Errorf("telemetry record %s has an unreadable time %q: %w", r.ID, r.Time, err)
		}
		if !at.Before(since.Time) {
			out = append(out, r)
		}
	}
	return out, nil
}

// SelectionAudit is one line of audits.jsonl: a selection compared with the
// failures of a full run at the same commit.
type SelectionAudit struct {
	SelectionID       string   `json:"selection_id"`
	LockDigest        string   `json:"lock_digest"`
	Commit            string   `json:"commit"`
	FullRunID         string   `json:"full_run_id"`
	Failing           []string `json:"failing"`
	Missed            []string `json:"missed"`
	RegressionBearing bool     `json:"regression_bearing"`
}

// Miss is one field_misses row.
type Miss struct {
	ID          string `json:"id"`
	SelectionID string `json:"selection_id"`
	TestID      string `json:"test_id"`
	DetectedAt  string `json:"detected_at"`
	FullRunID   string `json:"full_run_id"`
	BlameWindow string `json:"blame_window"`
	Attributed  bool   `json:"attributed"`
}

// Unattributed is a failing test the audit could not tie to a selection.
type Unattributed struct {
	TestID string `json:"test_id"`
	Reason string `json:"reason"`
}

// Report is the outcome of an audit.
type Report struct {
	FullRunID         string           `json:"full_run_id"`
	Commit            string           `json:"commit"`
	Failing           []string         `json:"failing"`
	Audited           int              `json:"audited_selections"`
	RegressionBearing int              `json:"regression_bearing"`
	Misses            int              `json:"misses"`
	Attributed        int              `json:"attributed"`
	Unattributed      int              `json:"unattributed"`
	Selections        []SelectionAudit `json:"selections"`
	MissList          []Miss           `json:"miss_list"`
	UnattributedTests []Unattributed   `json:"unattributed_tests"`
	Warnings          []string         `json:"warnings"`
}

// AuditInput is one audit request.
type AuditInput struct {
	Records      []Record
	Since        Since
	FullRunID    string
	Commit       string   // the full run's commit, a full SHA
	Failing      []string // failing tests of the full run
	Unidentified int      // failing cases that could not be given a test ID
	Bisect       bool
	Now          time.Time
}

// AuditDeps are the git and runner operations a bisect needs.
type AuditDeps struct {
	// IsAncestor reports whether a is an ancestor of b, or b itself.
	IsAncestor func(ctx context.Context, a, b string) (bool, error)
	// Range returns oldest followed by every commit of oldest..newest along the
	// ancestry path, parents first, and the parent of oldest ("" for a root).
	Range func(ctx context.Context, oldest, newest string) (commits []string, parent string, err error)
	// Probe runs one test at one commit.
	Probe func(ctx context.Context, commit, testID string) (State, error)
}

const reasonNoBisect = "no selection was made at the full-run commit; run with --bisect to look for the commit that introduced it"

// Audit compares the selections recorded since in.Since with the failures of a
// full run.
//
// A selection made at the full run's commit is compared directly: every failing
// test it did not select is a field miss. A failure that no same-commit
// selection explains is attributed only by a bisect, which finds the first
// commit where the test fails and blames a selection made there that left the
// test out. A selection on another commit is never blamed without it.
func Audit(ctx context.Context, in AuditInput, deps AuditDeps) (Report, error) {
	failing := slices.Clone(in.Failing)
	slices.Sort(failing)
	failing = slices.Compact(failing)
	if failing == nil {
		failing = []string{}
	}
	rep := Report{
		FullRunID: in.FullRunID, Commit: in.Commit, Failing: failing,
		Selections: []SelectionAudit{}, MissList: []Miss{}, UnattributedTests: []Unattributed{}, Warnings: []string{},
	}
	since, err := SinceRecords(in.Records, in.Since)
	if err != nil {
		return Report{}, err
	}
	var picked []Record
	for _, r := range since {
		if (r.Mode == ModeSelect || r.Mode == ModeRun) && !r.FullSuite {
			picked = append(picked, r)
		}
	}
	at := in.Now.UTC().Format(time.RFC3339)

	sameCommit := 0
	for _, r := range picked {
		if r.Commit != in.Commit {
			continue
		}
		sameCommit++
		members := memberSet(r)
		missed := []string{}
		for _, t := range failing {
			if !covers(members, t) {
				missed = append(missed, t)
				rep.MissList = append(rep.MissList, newMiss(r.ID, t, at, in.FullRunID, in.Commit+".."+in.Commit))
			}
		}
		rep.Selections = append(rep.Selections, SelectionAudit{
			SelectionID: r.ID, LockDigest: r.LockDigest, Commit: r.Commit, FullRunID: in.FullRunID,
			Failing: slices.Clone(failing), Missed: missed, RegressionBearing: len(failing) > 0,
		})
	}

	if sameCommit == 0 && len(failing) > 0 {
		if in.Bisect {
			if err := bisectAll(ctx, in, deps, picked, failing, at, &rep); err != nil {
				return Report{}, err
			}
		} else {
			for _, t := range failing {
				rep.UnattributedTests = append(rep.UnattributedTests, Unattributed{TestID: t, Reason: reasonNoBisect})
			}
		}
	}
	if in.Unidentified > 0 {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("%d failing case(s) have no usable test identity and cannot be attributed", in.Unidentified))
	}

	rep.Audited = len(rep.Selections)
	for _, s := range rep.Selections {
		if s.RegressionBearing {
			rep.RegressionBearing++
		}
	}
	rep.Misses = len(rep.MissList)
	for _, m := range rep.MissList {
		if m.Attributed {
			rep.Attributed++
		}
	}
	rep.Unattributed = len(rep.UnattributedTests) + in.Unidentified
	return rep, nil
}

func memberSet(r Record) map[string]bool {
	set := make(map[string]bool, len(r.Members))
	for _, m := range r.Members {
		set[m.TestID] = true
	}
	return set
}

// covers reports whether a selection with the given members ran the test id:
// the test itself, or for a Go subtest ("go:pkg:Parent/sub") its top-level test,
// which the runner always runs whole.
func covers(members map[string]bool, id string) bool {
	if members[id] {
		return true
	}
	pkg, ok := strings.CutPrefix(id, "go:")
	if !ok {
		return false
	}
	pkgPath, name, ok := strings.Cut(pkg, ":")
	if !ok {
		return false
	}
	parent, _, hasSub := strings.Cut(name, "/")
	return hasSub && members["go:"+pkgPath+":"+parent]
}

// newMiss builds an attributed miss with an ID derived from what it says, so
// auditing the same full run twice replaces rows instead of duplicating them.
func newMiss(selectionID, testID, at, fullRunID, window string) Miss {
	sum := sha256.Sum256([]byte(selectionID + "\x00" + testID + "\x00" + fullRunID))
	return Miss{
		ID: hex.EncodeToString(sum[:16]), SelectionID: selectionID, TestID: testID,
		DetectedAt: at, FullRunID: fullRunID, BlameWindow: window, Attributed: true,
	}
}

// bisectAll attributes every failing test by bisecting the commits from the
// oldest audited selection to the full run's commit. Only a cancellation is an
// error; everything else leaves the failing test unattributed with a reason.
func bisectAll(ctx context.Context, in AuditInput, deps AuditDeps, picked []Record, failing []string, at string, rep *Report) error {
	skip := func(reason string) {
		for _, t := range failing {
			rep.UnattributedTests = append(rep.UnattributedTests, Unattributed{TestID: t, Reason: reason})
		}
	}
	var eligible []Record
	related := map[string]bool{} // commit -> an ancestor of the full-run commit; asked once per commit
	checked := map[string]bool{}
	for _, r := range picked {
		if !checked[r.Commit] {
			checked[r.Commit] = true
			ok, err := deps.IsAncestor(ctx, r.Commit, in.Commit)
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
			if err != nil {
				// A commit that no longer exists (rewritten history) cannot be related
				// to the full run; the other selections can still be bisected.
				rep.Warnings = append(rep.Warnings, fmt.Sprintf("cannot relate the selection on %s to the full-run commit: %v", r.Commit, err))
			}
			related[r.Commit] = err == nil && ok
		}
		if related[r.Commit] {
			eligible = append(eligible, r)
		}
	}
	if len(eligible) == 0 {
		skip("no audited selection precedes the full-run commit")
		return nil
	}
	oldest := eligible[0].Commit
	for _, r := range eligible[1:] {
		if r.Commit == oldest {
			continue
		}
		older, err := deps.IsAncestor(ctx, r.Commit, oldest)
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if err == nil && older {
			oldest = r.Commit
		}
	}
	commits, parent, err := deps.Range(ctx, oldest, in.Commit)
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	if err != nil || len(commits) == 0 {
		skip(fmt.Sprintf("cannot list the commits from %s to %s: %v", oldest, in.Commit, err))
		return nil
	}
	for _, t := range failing {
		bad, good, reason, err := bisectOne(ctx, deps, commits, parent, t)
		if err != nil {
			return err
		}
		if reason != "" {
			rep.UnattributedTests = append(rep.UnattributedTests, Unattributed{TestID: t, Reason: reason})
			continue
		}
		blamed := false
		for _, r := range eligible {
			if r.Commit == bad && !covers(memberSet(r), t) {
				rep.MissList = append(rep.MissList, newMiss(r.ID, t, at, in.FullRunID, blameWindow(good, bad)))
				blamed = true
			}
		}
		if !blamed {
			rep.UnattributedTests = append(rep.UnattributedTests, Unattributed{
				TestID: t, Reason: fmt.Sprintf("no selection at the first failing commit %s omitted the test", bad),
			})
		}
	}
	return nil
}

func blameWindow(good, bad string) string {
	if good == "" {
		good = "root"
	}
	return good + ".." + bad
}

// bisectOne finds the first commit of commits where testID fails. The state at
// both ends is verified first: the test must fail at the last commit and pass
// at parent (the commit before the range, when there is one). A non-empty
// reason means the test could not be attributed; the error is only a
// cancellation.
func bisectOne(ctx context.Context, deps AuditDeps, commits []string, parent, testID string) (bad, good, reason string, err error) {
	probe := func(commit string) (State, string, error) {
		st, perr := deps.Probe(ctx, commit, testID)
		if cerr := ctx.Err(); cerr != nil {
			return StateUnknown, "", cerr
		}
		switch {
		case perr != nil:
			return StateUnknown, fmt.Sprintf("bisect failed at %s: %v", commit, perr), nil
		case st == StateUnknown:
			return StateUnknown, fmt.Sprintf("inconclusive at %s: the test could not be judged there", commit), nil
		}
		return st, "", nil
	}
	hi := len(commits) - 1
	st, why, err := probe(commits[hi])
	switch {
	case err != nil:
		return "", "", "", err
	case why != "":
		return "", "", why, nil
	case st != StateFail:
		return "", "", fmt.Sprintf("the test does not fail when run again at the full-run commit %s; cannot reproduce it", commits[hi]), nil
	}
	if parent != "" {
		st, why, err = probe(parent)
		switch {
		case err != nil:
			return "", "", "", err
		case why != "":
			return "", "", why, nil
		case st == StateFail:
			return "", "", fmt.Sprintf("the test already fails at %s, before the first audited selection", parent), nil
		}
	}
	lo := -1
	for hi-lo > 1 {
		mid := lo + (hi-lo)/2
		st, why, err = probe(commits[mid])
		switch {
		case err != nil:
			return "", "", "", err
		case why != "":
			return "", "", why, nil
		case st == StateFail:
			hi = mid
		default:
			lo = mid
		}
	}
	good = parent
	if lo >= 0 {
		good = commits[lo]
	}
	return commits[hi], good, "", nil
}

// FailingTests returns the sorted IDs of the failing cases (failure or error)
// of a JUnit report, and the number of failing cases that have no usable
// identity. known holds test IDs seen elsewhere, used to tell a Go case from a
// Python one when the report does not say.
func FailingTests(cases []runner.JUnitCase, known map[string]bool) (ids []string, unidentified int) {
	seen := map[string]bool{}
	for _, c := range cases {
		if c.Status != "fail" && c.Status != "error" {
			continue
		}
		id, ok := identify(c, known)
		switch {
		case !ok:
			unidentified++
		case !seen[id]:
			seen[id] = true
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids, unidentified
}

// identify names a failing case. A JUnit case does not say which language
// produced it, so the evidence is weighed in order: the ID is a known test, the
// suite is pytest's or the file is a .py file, the class name is a Go import
// path (it contains a slash). With none of these the case is read as Python.
func identify(c runner.JUnitCase, known map[string]bool) (string, bool) {
	goID, goErr := runner.NormalizeJUnitCase("go", c.Classname, c)
	pyID, pyErr := runner.NormalizeJUnitCase("python", "", c)
	switch {
	case goErr != nil && pyErr != nil:
		return "", false
	case goErr != nil:
		return pyID.ID, true
	case pyErr != nil:
		return goID.ID, true
	case known[goID.ID] != known[pyID.ID]:
		if known[goID.ID] {
			return goID.ID, true
		}
		return pyID.ID, true
	case c.Suite == "pytest" || strings.HasSuffix(c.File, ".py"):
		return pyID.ID, true
	case strings.Contains(c.Classname, "/"):
		return goID.ID, true
	}
	return pyID.ID, true
}

// Persist writes the field misses of rep to the database at dbPath (created
// when missing) and one line per audited selection to the JSONL file at
// auditsPath. Re-persisting the same report replaces the rows.
func Persist(dbPath, auditsPath string, rep Report) (err error) {
	for _, s := range rep.Selections {
		line, merr := json.Marshal(s)
		if merr != nil {
			return fmt.Errorf("telemetry: encode audit of %s: %w", s.SelectionID, merr)
		}
		if err := appendLine(auditsPath, append(line, '\n')); err != nil {
			return err
		}
	}
	if len(rep.MissList) == 0 {
		return nil
	}
	s, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("telemetry: open database: %w", err)
	}
	defer func() {
		if cerr := s.Close(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("telemetry: close database: %w", cerr))
		}
	}()
	return insertMisses(s.DB(), rep.MissList)
}

func insertMisses(db *sql.DB, misses []Miss) (err error) {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("telemetry: begin: %w", err)
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("telemetry: rollback: %w", rbErr))
		}
	}()
	for _, m := range misses {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO field_misses (id, selection_id, test_id, detected_at, full_run_id, blame_window, attributed)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, m.ID, m.SelectionID, m.TestID, m.DetectedAt, m.FullRunID, m.BlameWindow, b2i(m.Attributed)); err != nil {
			return fmt.Errorf("telemetry: insert field miss %s of %s: %w", m.TestID, m.SelectionID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("telemetry: commit field misses: %w", err)
	}
	return nil
}
