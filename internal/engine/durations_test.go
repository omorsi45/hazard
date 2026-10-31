package engine

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/omorsi45/hazard/internal/store"
)

// durationRow is one row of test_results.
type durationRow struct {
	test, status string
	ms           any // an int, or nil for NULL
}

// seedDurations opens the history database of repo (creating it) and inserts
// the rows, one attempt each so they never collide on the primary key.
func seedDurations(t *testing.T, repo string, rows []durationRow) {
	t.Helper()
	s, err := store.Open(filepath.Join(repo, ".hazard", "hazard.db"))
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range rows {
		if _, err := s.DB().Exec(`INSERT INTO test_results (commit_sha, test_id, status, duration_ms, attempt, observed_at)
			VALUES ('c', ?, ?, ?, ?, '2026-01-01T00:00:00Z')`, r.test, r.status, r.ms, i+1); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestTestDurationsAreTheMedianPerTest(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	seedDurations(t, repo, []durationRow{
		{"odd", "pass", 10}, {"odd", "pass", 30}, {"odd", "fail", 20},
		{"even", "pass", 10}, {"even", "pass", 21},
		{"single", "error", 7},
		{"skipped too", "skip", 0}, {"skipped too", "pass", 100},
		{"unmeasured", "pass", nil}, {"unmeasured", "pass", nil},
		{"negative", "pass", -5}, {"negative", "pass", 40},
	})

	got, err := testDurations(context.Background(), repo)

	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{
		"odd":         20,
		"even":        16, // the mean of 10 and 21, rounded up
		"single":      7,
		"skipped too": 100, // a skipped test measured nothing
		"negative":    40,  // a negative duration is not a measurement
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("durations\n got: %v\nwant: %v", got, want)
	}
}

func TestTestDurationsWithoutADatabaseAreUnknown(t *testing.T) {
	t.Parallel()
	got, err := testDurations(context.Background(), t.TempDir())
	if err != nil || len(got) != 0 {
		t.Errorf("got %v, %v; want no durations and no error", got, err)
	}
}

func TestTestDurationsOfADatabaseWithoutTheTableIsAnError(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	path := filepath.Join(repo, ".hazard", "hazard.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE unrelated (x INTEGER)"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if got, err := testDurations(context.Background(), repo); err == nil {
		t.Errorf("durations = %v, nil error; a database that cannot answer must be an error, not an empty history", got)
	}
}
