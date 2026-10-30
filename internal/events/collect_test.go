package events

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/omorsi45/hazard/internal/impact"
	"github.com/omorsi45/hazard/internal/index"
	"github.com/omorsi45/hazard/internal/mutate"
	"github.com/omorsi45/hazard/internal/store"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "hazard.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return st.DB()
}

func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func addBuild(t *testing.T, db *sql.DB, id string, current int) {
	t.Helper()
	mustExec(t, db, `INSERT INTO label_builds(build_id,provider,project,cutoff,algorithm_version,input_digest,decision_digest,always_run_json,acquisition_exclusions_json,active_gaps,created_at,is_current)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, strings.Repeat(id, 64), "github", "o/r", "2026-01-01T00:00:00Z", "labels/1",
		strings.Repeat(id, 64), strings.Repeat("d", 64), "[]", "{}", 0, "2026-01-01T00:00:00Z", current)
}

func addCandidate(t *testing.T, db *sql.DB, build, key, kind, state, sha, test, env string) {
	t.Helper()
	var shaArg, testArg, envArg any
	if sha != "" {
		shaArg, testArg, envArg = sha, test, env
	}
	mustExec(t, db, `INSERT INTO label_candidates(build_id,candidate_key,kind,state,tested_sha,test_id,environment_id,branch,member_count,evidence_json)
		VALUES(?,?,?,?,?,?,?,?,?,?)`, strings.Repeat(build, 64), key, kind, state, shaArg, testArg, envArg, "main", 1, "{}")
}

// TestReadCILabelsTakesStrongAndWeakCandidatesOfTheCurrentBuildOnly: pending and
// excluded candidates, unresolved records and every older build are not labels.
func TestReadCILabelsTakesStrongAndWeakCandidatesOfTheCurrentBuildOnly(t *testing.T) {
	db := openTestDB(t)
	addBuild(t, db, "a", 0)
	addBuild(t, db, "b", 1)
	addCandidate(t, db, "a", "old", "candidate", "strong", "sha9", "go:p:TestOld", "linux")
	addCandidate(t, db, "b", "k1", "candidate", "strong", "sha1", "go:p:TestA", "linux")
	addCandidate(t, db, "b", "k2", "candidate", "strong", "sha1", "go:p:TestA", "windows") // same test, other environment
	addCandidate(t, db, "b", "k3", "candidate", "weak", "sha1", "go:p:TestB", "linux")
	addCandidate(t, db, "b", "k4", "candidate", "weak", "sha2", "go:p:TestC", "linux")
	addCandidate(t, db, "b", "k5", "candidate", "pending", "sha3", "go:p:TestD", "linux")
	addCandidate(t, db, "b", "k6", "candidate", "excluded", "sha3", "go:p:TestE", "linux")
	addCandidate(t, db, "b", "k7", "unresolved", "pending", "", "", "")

	got, err := readCILabels(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	want := []ciLabel{
		{sha: "sha1", strong: true, failing: []string{"go:p:TestA", "go:p:TestB"}},
		{sha: "sha2", strong: false, failing: []string{"go:p:TestC"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("labels = %+v\nwant     %+v", got, want)
	}
}

func TestReadCILabelsWithoutABuildIsEmpty(t *testing.T) {
	got, err := readCILabels(context.Background(), openTestDB(t))
	if err != nil || len(got) != 0 {
		t.Errorf("labels = %+v, %v, want none", got, err)
	}
}

func TestReadMutationRecordsAttachesKillsToKilledMutantsOnly(t *testing.T) {
	db := openTestDB(t)
	insert := func(id, status string) {
		mustExec(t, db, `INSERT INTO mutants(id,file,line,operator,original,mutated,base_sha,status,created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
			id, "calc/calc.go", 3, "cmp", ">", ">=", "base1", status, "2026-01-01T00:00:00Z")
	}
	insert("m2", "survived")
	insert("m1", "killed")
	insert("m3", "build_error")
	mustExec(t, db, `INSERT INTO mutant_kills(mutant_id,test_id) VALUES('m1','go:p:TestB'),('m1','go:p:TestA'),('m2','go:p:TestStray')`)

	got, err := readMutationRecords(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	want := []mutate.Record{
		{ID: "m1", File: "calc/calc.go", Line: 3, Operator: "cmp", Original: ">", Mutated: ">=", BaseSHA: "base1", Status: "killed", Killers: []string{"go:p:TestA", "go:p:TestB"}},
		{ID: "m2", File: "calc/calc.go", Line: 3, Operator: "cmp", Original: ">", Mutated: ">=", BaseSHA: "base1", Status: "survived"},
		{ID: "m3", File: "calc/calc.go", Line: 3, Operator: "cmp", Original: ">", Mutated: ">=", BaseSHA: "base1", Status: "build_error"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("records = %+v\nwant      %+v", got, want)
	}
}

func TestModeForFollowsCoverage(t *testing.T) {
	if got := modeFor(&index.Manifest{}); got != impact.ModeGraph {
		t.Errorf("mode without coverage = %v, want graph", got)
	}
	if got := modeFor(&index.Manifest{Coverage: &index.Coverage{}}); got != impact.ModeCoverage {
		t.Errorf("mode with coverage = %v, want coverage", got)
	}
}

func TestCommitInfoReadsTimeAndFirstParent(t *testing.T) {
	dir, first := newRepo(t, mutationFiles())
	writeFiles(t, dir, map[string]string{"x.txt": "x\n"})
	second := commitAt(t, dir, "second", baseTime.Add(time.Hour))
	at, parent, err := commitInfo(context.Background(), dir, second)
	if err != nil || !at.Equal(baseTime.Add(time.Hour)) || parent != first {
		t.Errorf("commitInfo(second) = %v, %q, %v, want %v and parent %s", at, parent, err, baseTime.Add(time.Hour), first)
	}
	at, parent, err = commitInfo(context.Background(), dir, first)
	if err != nil || !at.Equal(baseTime) || parent != "" {
		t.Errorf("commitInfo(root) = %v, %q, %v, want %v and no parent", at, parent, err, baseTime)
	}
	if _, _, err := commitInfo(context.Background(), dir, strings.Repeat("0", 40)); err == nil {
		t.Error("commitInfo accepted a commit that is not in the repository")
	}
}
