package telemetry

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/omorsi45/hazard/internal/engine"
	"github.com/omorsi45/hazard/internal/store"
)

// newDB creates the hazard database the way every other command does and
// returns its path.
func newDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".hazard", "hazard.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

type selectionRow struct {
	Commit, CreatedAt, Mode, Tier, Reason, LockDigest              string
	Abstained, NTotal, NCertain, NAdded, EstSel, EstFull, Overhead int
	Lambda, Cost                                                   float64
}

func readSelection(t *testing.T, dbPath, id string) (selectionRow, bool) {
	t.Helper()
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	}()
	var row selectionRow
	var reason *string
	err = s.DB().QueryRow(`SELECT commit_sha, created_at, mode, tier, abstained, abstain_reason, lambda, lock_digest,
		n_total, n_certain, n_added, est_ms_sel, est_ms_full, overhead_ms, jev_cost_usd FROM selections WHERE id = ?`, id).Scan(
		&row.Commit, &row.CreatedAt, &row.Mode, &row.Tier, &row.Abstained, &reason, &row.Lambda, &row.LockDigest,
		&row.NTotal, &row.NCertain, &row.NAdded, &row.EstSel, &row.EstFull, &row.Overhead, &row.Cost)
	if err != nil {
		return selectionRow{}, false
	}
	if reason != nil {
		row.Reason = *reason
	}
	return row, true
}

func readMembers(t *testing.T, dbPath, id string) []Member {
	t.Helper()
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	}()
	rows, err := s.DB().Query(`SELECT test_id, source, p_break FROM selection_members WHERE selection_id = ? ORDER BY test_id`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Error(err)
		}
	}()
	var out []Member
	for rows.Next() {
		var m Member
		var p *float64
		if err := rows.Scan(&m.TestID, &m.Source, &p); err != nil {
			t.Fatal(err)
		}
		m.P = p
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestWriteMirrorsASelectionIntoAnExistingDatabase(t *testing.T) {
	db := newDB(t)
	jsonl := filepath.Join(filepath.Dir(db), "runs.jsonl")
	rec := FromDecision(narrowed(), ModeRun, "v", fixedTime)
	if err := Write(jsonl, db, rec); err != nil {
		t.Fatal(err)
	}
	row, ok := readSelection(t, db, "dec-1")
	if !ok {
		t.Fatal("no selections row")
	}
	want := selectionRow{
		Commit: "head1", CreatedAt: "2026-10-30T02:15:30Z", Mode: "run", Tier: "module", Abstained: 0,
		Lambda: 0.125, LockDigest: "sha256:lock",
		NTotal: 5, NCertain: 2, NAdded: 1, EstSel: 120, EstFull: 900, Overhead: 37, Cost: 0.002,
	}
	if row != want {
		t.Errorf("selections row = %+v, want %+v (always_run counts as certain: it is part of the deterministic floor)", row, want)
	}
	got := readMembers(t, db, "dec-1")
	wantMembers := []Member{
		{TestID: "go:m/a:TestA", Source: "certain"},
		{TestID: "go:m/b:TestC", Source: "added", P: fp(0.4)},
		{TestID: "py:t.x:test_a", Source: "certain"},
		{TestID: "py:t.x:test_b", Source: "tier"},
	}
	if !reflect.DeepEqual(got, wantMembers) {
		t.Errorf("selection_members = %+v, want %+v", got, wantMembers)
	}
	if recs, err := Read(jsonl); err != nil || len(recs) != 1 {
		t.Errorf("the JSONL line must be written too: %d records, err %v", len(recs), err)
	}
}

func TestWriteRecordsAFullSuiteAsASuiteWithoutMembers(t *testing.T) {
	db := newDB(t)
	d := narrowed()
	d.FullSuite, d.Abstained, d.AbstainReason, d.Tests = true, true, "no_lock", nil
	d.Hypothetical = []engine.Selected{{TestID: "go:m/a:TestA", Source: "certain"}}
	if err := Write(filepath.Join(filepath.Dir(db), "runs.jsonl"), db, FromDecision(d, ModeRun, "v", fixedTime)); err != nil {
		t.Fatal(err)
	}
	row, ok := readSelection(t, db, "dec-1")
	if !ok || row.Tier != "suite" || row.Abstained != 1 || row.Reason != "no_lock" {
		t.Errorf("selections row = %+v (found %v), want tier suite, abstained 1, reason no_lock", row, ok)
	}
	if got := readMembers(t, db, "dec-1"); len(got) != 0 {
		t.Errorf("a full-suite row has no members in the database (the JSONL keeps the hypothetical set): %+v", got)
	}
}

func TestWriteDoesNotCreateTheDatabase(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, ".hazard", "hazard.db")
	if err := Write(filepath.Join(dir, "runs.jsonl"), db, FromDecision(narrowed(), ModeRun, "v", fixedTime)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(db); !os.IsNotExist(err) {
		t.Errorf("the database must be mirrored only when it already exists: stat err = %v", err)
	}
	if recs, err := Read(filepath.Join(dir, "runs.jsonl")); err != nil || len(recs) != 1 {
		t.Errorf("JSONL: %d records, err %v", len(recs), err)
	}
}

func TestWriteReportsEveryFailureAndStillTriesTheOtherSink(t *testing.T) {
	db := newDB(t)
	dir := filepath.Dir(db)
	// The JSONL path is a directory: appending fails, mirroring still happens.
	if err := Write(dir, db, FromDecision(narrowed(), ModeRun, "v", fixedTime)); err == nil {
		t.Fatal("want the JSONL failure reported")
	}
	if _, ok := readSelection(t, db, "dec-1"); !ok {
		t.Error("the database row must be written even when the JSONL append fails")
	}
	// The same ID twice: the database refuses, the JSONL line is still appended.
	jsonl := filepath.Join(dir, "runs.jsonl")
	if err := Write(jsonl, db, FromDecision(narrowed(), ModeRun, "v", fixedTime)); err == nil {
		t.Fatal("want the duplicate selection ID reported")
	}
	if recs, err := Read(jsonl); err != nil || len(recs) != 1 {
		t.Errorf("JSONL: %d records, err %v", len(recs), err)
	}
	if got := readMembers(t, db, "dec-1"); len(got) != 4 {
		t.Errorf("the failed duplicate must not change the members: %+v", got)
	}
}
