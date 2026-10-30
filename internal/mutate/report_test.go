package mutate

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/omorsi45/hazard/internal/store"
)

func sampleRecords() []Record {
	return []Record{
		{ID: "b2", File: "calc/calc.go", Line: 4, Operator: "cmp", Original: ">", Mutated: ">=", BaseSHA: "sha1", Status: "killed",
			Killers: []string{"go:m/app:TestRun", "go:m/calc:TestMax"}, UnreachableKillers: nil, BeyondMaxHopsKillers: nil, DurationMS: 120},
		{ID: "a1", File: "config/limits.yaml", Line: 2, Operator: "cfg", Original: "10", Mutated: "11", BaseSHA: "sha1", Status: "killed",
			Killers: []string{"go:m/cfgcheck:TestLimit"}, UnreachableKillers: []string{"go:m/cfgcheck:TestLimit"}, DurationMS: 80},
		{ID: "c3", File: "calc/calc.go", Line: 9, Operator: "const", Original: "1", Mutated: "2", BaseSHA: "sha1", Status: "killed",
			Killers: []string{"go:m/app:TestRun", "go:m/far:TestFar"}, BeyondMaxHopsKillers: []string{"go:m/far:TestFar"}, DurationMS: 90},
		{ID: "d4", File: "calc/calc.go", Line: 12, Operator: "cmp", Original: "<", Mutated: "<=", BaseSHA: "sha1", Status: "survived", DurationMS: 70},
		{ID: "e5", File: "calc/calc.go", Line: 13, Operator: "ret", Original: "x", Mutated: "0", BaseSHA: "sha1", Status: "build_error", DurationMS: 5},
		{ID: "f6", File: "calc/calc.go", Line: 14, Operator: "stmt", Original: "x++", Mutated: "", BaseSHA: "sha1", Status: "timeout", DurationMS: 50000},
		{ID: "g7", File: "calc/calc.go", Line: 15, Operator: "logic", Original: "&&", Mutated: "||", BaseSHA: "sha1", Status: "unconfirmed", DurationMS: 60},
	}
}

func TestSummarizeCounts(t *testing.T) {
	got := summarize("sha1", 40, 12.5, sampleRecords())
	if got.BaseSHA != "sha1" || got.Candidates != 40 || got.Executed != 7 || got.BaselineSeconds != 12.5 {
		t.Errorf("header = %+v", got)
	}
	wantStatus := map[string]int{"killed": 3, "survived": 1, "build_error": 1, "timeout": 1, "unconfirmed": 1}
	if !reflect.DeepEqual(got.ByStatus, wantStatus) {
		t.Errorf("ByStatus = %v, want %v", got.ByStatus, wantStatus)
	}
	wantOp := map[string]int{"cmp": 2, "cfg": 1, "const": 1, "ret": 1, "stmt": 1, "logic": 1}
	if !reflect.DeepEqual(got.ByOperator, wantOp) {
		t.Errorf("ByOperator = %v, want %v", got.ByOperator, wantOp)
	}
	if got.Usable != 3 {
		t.Errorf("Usable = %d, want 3 (killed only)", got.Usable)
	}
	if got.UnreachableMutants != 1 || got.FullyUnreachableMutants != 1 || got.BeyondMaxHopsMutants != 1 {
		t.Errorf("reachability counts = %d/%d/%d, want 1/1/1", got.UnreachableMutants, got.FullyUnreachableMutants, got.BeyondMaxHopsMutants)
	}
	ids := make([]string, len(got.Records))
	for i, r := range got.Records {
		ids[i] = r.ID
	}
	if !sort.StringsAreSorted(ids) || len(ids) != 7 {
		t.Errorf("Records not ordered by ID: %v", ids)
	}
}

func TestSummarizePartiallyUnreachableIsNotFullyUnreachable(t *testing.T) {
	recs := []Record{{ID: "x", Status: "killed", Operator: "cfg",
		Killers: []string{"go:m/a:TestA", "go:m/b:TestB"}, UnreachableKillers: []string{"go:m/b:TestB"}}}
	got := summarize("s", 1, 0, recs)
	if got.UnreachableMutants != 1 || got.FullyUnreachableMutants != 0 {
		t.Errorf("unreachable=%d fully=%d, want 1 and 0", got.UnreachableMutants, got.FullyUnreachableMutants)
	}
}

func readJSONL(t *testing.T, path string) []Record {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	}()
	var out []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var r Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		out = append(out, r)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestJSONLWriterPartialThenAtomicCommit(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "nested", "mutants.jsonl")
	w, err := newJSONLWriter(out)
	if err != nil {
		t.Fatal(err)
	}
	recs := sampleRecords()
	if err := w.Append(recs[0]); err != nil {
		t.Fatal(err)
	}
	if err := w.Append(recs[1]); err != nil {
		t.Fatal(err)
	}
	// An interrupted run leaves evidence in the partial file and no final file.
	if got := readJSONL(t, out+".partial"); len(got) != 2 || got[0].ID != "b2" {
		t.Fatalf("partial = %+v, want the two appended records in append order", got)
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatalf("the final file exists before Commit")
	}
	if err := w.Commit(recs); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(out + ".partial"); err == nil {
		t.Errorf("the partial file survives a successful Commit")
	}
	got := readJSONL(t, out)
	if len(got) != len(recs) {
		t.Fatalf("final file holds %d records, want %d", len(got), len(recs))
	}
	if got[0].ID != "a1" || got[len(got)-1].ID != "g7" {
		t.Errorf("final file is not ordered by ID: first=%s last=%s", got[0].ID, got[len(got)-1].ID)
	}
	for _, r := range got {
		if r.Killers == nil || r.UnreachableKillers == nil || r.BeyondMaxHopsKillers == nil {
			t.Errorf("record %s: empty lists must encode as [], not null: %+v", r.ID, r)
		}
	}
}

func TestJSONLWriterCloseKeepsPartialEvidence(t *testing.T) {
	out := filepath.Join(t.TempDir(), "m.jsonl")
	w, err := newJSONLWriter(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Append(sampleRecords()[0]); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got := readJSONL(t, out+".partial"); len(got) != 1 {
		t.Fatalf("partial after Close = %d records, want 1", len(got))
	}
	if _, err := os.Stat(out); err == nil {
		t.Errorf("Close published the final file")
	}
}

func TestWriteBaselinePublishesSortedKeysNextToTheJSONL(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "mutants.jsonl")
	if got, want := baselinePath(out), out+".baseline.json"; got != want {
		t.Fatalf("baselinePath = %q, want %q", got, want)
	}
	tests := map[string]int64{"go:m/c:TestC": 2000, "go:m/a:TestA": 15, "go:m/b:TestB": 0}
	if err := writeBaseline(out, "sha1", tests); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(out + ".baseline.json")
	if err != nil {
		t.Fatal(err)
	}
	want := `{"base_sha":"sha1","tests":{"go:m/a:TestA":15,"go:m/b:TestB":0,"go:m/c:TestC":2000}}` + "\n"
	if string(got) != want {
		t.Errorf("baseline file = %q\nwant            %q", got, want)
	}
	if _, err := os.Stat(out + ".baseline.json.tmp"); err == nil {
		t.Errorf("the temporary file survives a published baseline")
	}
	// A rerun replaces the file, and a suite without tests is {}, never null.
	if err := writeBaseline(out, "sha2", nil); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(out + ".baseline.json")
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"base_sha":"sha2","tests":{}}` + "\n"; string(got) != want {
		t.Errorf("baseline file after a rerun = %q, want %q", got, want)
	}
}

// A baseline that cannot be written completely leaves the previous file whole
// and reports the failure.
func TestWriteBaselineFailureKeepsThePreviousFile(t *testing.T) {
	out := filepath.Join(t.TempDir(), "mutants.jsonl")
	if err := writeBaseline(out, "old", map[string]int64{"go:m/a:TestA": 1}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(out + ".baseline.json")
	if err != nil {
		t.Fatal(err)
	}
	// A directory where the temporary file belongs makes the write fail.
	if err := os.Mkdir(out+".baseline.json.tmp", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeBaseline(out, "new", map[string]int64{"go:m/a:TestA": 2}); err == nil {
		t.Fatalf("writeBaseline succeeded although the temporary file cannot be created")
	}
	after, err := os.ReadFile(out + ".baseline.json")
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf("a failed write changed the baseline file to %q, want %q", after, before)
	}
}

func dbRows(t *testing.T, path, query string) [][2]string {
	t.Helper()
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	}()
	rows, err := s.DB().Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Error(err)
		}
	}()
	var out [][2]string
	for rows.Next() {
		var a, b string
		if err := rows.Scan(&a, &b); err != nil {
			t.Fatal(err)
		}
		out = append(out, [2]string{a, b})
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPersistMutantsWritesRowsAndKills(t *testing.T) {
	db := filepath.Join(t.TempDir(), ".hazard", "hazard.db")
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	if err := persistMutants(context.Background(), db, sampleRecords(), now); err != nil {
		t.Fatal(err)
	}
	got := dbRows(t, db, "SELECT id, status FROM mutants ORDER BY id")
	want := [][2]string{{"a1", "killed"}, {"b2", "killed"}, {"c3", "killed"}, {"d4", "survived"}, {"e5", "build_error"}, {"f6", "timeout"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mutants = %v, want %v (unconfirmed is never stored)", got, want)
	}
	kills := dbRows(t, db, "SELECT mutant_id, test_id FROM mutant_kills ORDER BY mutant_id, test_id")
	wantKills := [][2]string{
		{"a1", "go:m/cfgcheck:TestLimit"},
		{"b2", "go:m/app:TestRun"}, {"b2", "go:m/calc:TestMax"},
		{"c3", "go:m/app:TestRun"}, {"c3", "go:m/far:TestFar"},
	}
	if !reflect.DeepEqual(kills, wantKills) {
		t.Errorf("mutant_kills = %v, want %v", kills, wantKills)
	}
	meta := dbRows(t, db, "SELECT base_sha, created_at FROM mutants WHERE id = 'b2'")
	if len(meta) != 1 || meta[0][0] != "sha1" || meta[0][1] != "2026-10-26T12:00:00Z" {
		t.Errorf("base_sha/created_at = %v", meta)
	}
}

func TestPersistMutantsRerunReplacesKills(t *testing.T) {
	db := filepath.Join(t.TempDir(), "hazard.db")
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	first := []Record{{ID: "m1", File: "f.go", Line: 1, Operator: "cmp", Original: ">", Mutated: ">=", BaseSHA: "s1", Status: "killed", Killers: []string{"go:m:TestA", "go:m:TestB"}}}
	if err := persistMutants(context.Background(), db, first, now); err != nil {
		t.Fatal(err)
	}
	second := []Record{{ID: "m1", File: "f.go", Line: 1, Operator: "cmp", Original: ">", Mutated: ">=", BaseSHA: "s2", Status: "killed", Killers: []string{"go:m:TestB"}}}
	if err := persistMutants(context.Background(), db, second, now); err != nil {
		t.Fatal(err)
	}
	kills := dbRows(t, db, "SELECT mutant_id, test_id FROM mutant_kills")
	if !reflect.DeepEqual(kills, [][2]string{{"m1", "go:m:TestB"}}) {
		t.Errorf("kills after rerun = %v, want only TestB", kills)
	}
	third := []Record{{ID: "m1", File: "f.go", Line: 1, Operator: "cmp", Original: ">", Mutated: ">=", BaseSHA: "s3", Status: "survived"}}
	if err := persistMutants(context.Background(), db, third, now); err != nil {
		t.Fatal(err)
	}
	if kills := dbRows(t, db, "SELECT mutant_id, test_id FROM mutant_kills"); len(kills) != 0 {
		t.Errorf("a survived rerun left stale kills: %v", kills)
	}
	if rows := dbRows(t, db, "SELECT status, base_sha FROM mutants"); !reflect.DeepEqual(rows, [][2]string{{"survived", "s3"}}) {
		t.Errorf("mutants after rerun = %v", rows)
	}
}

func TestPersistMutantsIsAtomic(t *testing.T) {
	db := filepath.Join(t.TempDir(), "hazard.db")
	bad := []Record{
		{ID: "ok", File: "f.go", Line: 1, Operator: "cmp", Original: ">", Mutated: ">=", BaseSHA: "s", Status: "killed", Killers: []string{"go:m:T"}},
		{ID: "bad", File: "f.go", Line: 2, Operator: "cmp", Original: ">", Mutated: ">=", BaseSHA: "s", Status: "exploded"},
	}
	if err := persistMutants(context.Background(), db, bad, time.Now()); err == nil {
		t.Fatalf("a status the schema forbids must be an error")
	}
	if rows := dbRows(t, db, "SELECT id, status FROM mutants"); len(rows) != 0 {
		t.Errorf("a failed transaction left rows: %v", rows)
	}
	if rows := dbRows(t, db, "SELECT mutant_id, test_id FROM mutant_kills"); len(rows) != 0 {
		t.Errorf("a failed transaction left kills: %v", rows)
	}
}
