package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// Always-run reasons recorded in the history database.
const (
	reasonQuarantined = "quarantined"
	reasonUnstableID  = "unstable_id"
)

// historyPath is the history database of a repository.
func historyPath(repoDir string) string {
	return filepath.Join(repoDir, ".hazard", "hazard.db")
}

// alwaysRun returns the tests the history database marks as quarantined or
// unstable, keyed by test ID with the reason as value. A missing database is
// an empty set. A database that exists but cannot be read is an error: the
// caller must run the full suite rather than treat it as an empty set.
func alwaysRun(ctx context.Context, repoDir string) (map[string]string, error) {
	file := historyPath(repoDir)
	info, err := os.Stat(file)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("stat history database: %w", err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("history database %s is a directory", file)
	}
	db, err := sql.Open("sqlite", readOnlyURI(file))
	if err != nil {
		return nil, fmt.Errorf("open history database: %w", err)
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()
	rows, err := db.QueryContext(ctx, "SELECT id, quarantined, unstable_id FROM tests WHERE quarantined = 1 OR unstable_id = 1 ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("query history database: %w", err)
	}
	defer func() { _ = rows.Close() }()
	always := map[string]string{}
	for rows.Next() {
		var id string
		var quarantined, unstable int
		if err := rows.Scan(&id, &quarantined, &unstable); err != nil {
			return nil, fmt.Errorf("read history database: %w", err)
		}
		if quarantined == 1 {
			always[id] = reasonQuarantined
		} else {
			always[id] = reasonUnstableID
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read history database: %w", err)
	}
	return always, nil
}

// readOnlyURI is the SQLite URI that opens file read-only.
func readOnlyURI(file string) string {
	p := filepath.ToSlash(file)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	u := url.URL{Scheme: "file", Path: p}
	u.RawQuery = url.Values{"mode": {"ro"}, "_pragma": {"busy_timeout(5000)"}}.Encode()
	return u.String()
}

// testDurations returns the median duration in milliseconds of each test with
// a measurement in the history database, keyed by test ID. A missing database
// is no durations (the controller then imputes them and says so). Skipped
// runs and missing or negative durations are not measurements. The median of
// an even count is the mean of the middle pair rounded up, like the
// controller's own estimate, so a test never looks cheaper than it is. A
// database that exists but cannot be queried is an error.
func testDurations(ctx context.Context, repoDir string) (map[string]int, error) {
	file := historyPath(repoDir)
	if _, err := os.Stat(file); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("stat history database: %w", err)
	}
	db, err := sql.Open("sqlite", readOnlyURI(file))
	if err != nil {
		return nil, fmt.Errorf("open history database: %w", err)
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()
	rows, err := db.QueryContext(ctx, `SELECT test_id, duration_ms FROM test_results
		WHERE duration_ms IS NOT NULL AND duration_ms >= 0 AND status <> 'skip'
		ORDER BY test_id, duration_ms`)
	if err != nil {
		return nil, fmt.Errorf("query test durations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	samples := map[string][]int{} // each list is ascending, from the ORDER BY
	for rows.Next() {
		var id string
		var ms int
		if err := rows.Scan(&id, &ms); err != nil {
			return nil, fmt.Errorf("read test durations: %w", err)
		}
		samples[id] = append(samples[id], ms)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read test durations: %w", err)
	}
	out := make(map[string]int, len(samples))
	for id, ms := range samples {
		mid := len(ms) / 2
		if len(ms)%2 == 1 {
			out[id] = ms[mid]
		} else {
			out[id] = ms[mid-1] + (ms[mid]-ms[mid-1]+1)/2
		}
	}
	return out, nil
}
