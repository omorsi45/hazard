package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"testing/fstest"
)

func TestOpenCreatesConfiguredDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "hazard.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	closeOnCleanup(t, store)
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}

	var mode string
	if err := store.DB().QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}
	var foreignKeys, busyTimeout int
	if err := store.DB().QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if foreignKeys != 1 {
		t.Fatalf("foreign_keys = %d, want 1", foreignKeys)
	}
	if err := store.DB().QueryRow("PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		t.Fatal(err)
	}
	if busyTimeout < 1000 {
		t.Fatalf("busy_timeout = %d, want at least 1000", busyTimeout)
	}

	names := []string{"tests", "commits", "test_results", "regressions", "excluded_labels", "mutants", "mutant_kills", "judgments", "selections", "selection_members", "field_misses", "calibrations"}
	for _, name := range names {
		var got string
		if err := store.DB().QueryRow("SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?", name).Scan(&got); err != nil {
			t.Fatalf("missing table %s: %v", name, err)
		}
	}
	var version int
	if err := store.DB().QueryRow("SELECT version FROM schema_migrations").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 1 {
		t.Fatalf("migration version = %d, want 1", version)
	}
}

func TestReopenPreservesDataAndMigrationCount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hazard.db")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.DB().Exec("INSERT INTO tests(id, lang, package, name, file, first_seen, last_seen) VALUES('go:p:T', 'go', 'p', 'T', 't.go', '2026-09-29T00:00:00Z', '2026-09-29T00:00:00Z')"); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	closeOnCleanup(t, again)
	var rows, versions int
	if err := again.DB().QueryRow("SELECT count(*) FROM tests").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("test rows = %d, want 1", rows)
	}
	if err := again.DB().QueryRow("SELECT count(*) FROM schema_migrations").Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != 1 {
		t.Fatalf("migration rows = %d, want 1", versions)
	}
}

func TestMigrationFailureRollsBackTablesAndVersion(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "hazard.db"))
	if err != nil {
		t.Fatal(err)
	}
	closeOnCleanup(t, s)
	bad := "CREATE TABLE should_rollback (id INTEGER); INSERT INTO nonexistent (id) VALUES (1);"
	if err := applyMigration(s.DB(), 2, bad); err == nil {
		t.Fatal("bad migration succeeded")
	}
	var count int
	if err := s.DB().QueryRow("SELECT count(*) FROM sqlite_master WHERE name='should_rollback'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("failed migration left a table")
	}
	if err := s.DB().QueryRow("SELECT count(*) FROM schema_migrations WHERE version=2").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("failed migration recorded version 2")
	}
}

func TestResultIdentityPreservesMatrixJobsAndRejectsReplay(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "hazard.db"))
	if err != nil {
		t.Fatal(err)
	}
	closeOnCleanup(t, s)
	insert := "INSERT INTO test_results(source, project, commit_sha, test_id, status, pipeline_id, job_id, environment_id, artifact_id, observed_at, artifact_path, raw_name, raw_classname, raw_message, infra_reason) VALUES(?, ?, 'sha', 'go:p:T', 'pass', 'pipeline', ?, ?, 'junit', '2026-09-29T00:00:00Z', 'raw/junit.xml', 'TestT', 'pkg.Suite', 'failure text', 'runner error')"
	for _, pair := range [][2]string{{"linux", "go126"}, {"windows", "go126"}, {"linux", "go125"}} {
		if _, err := s.DB().Exec(insert, "github", "owner/repo", pair[0], pair[1]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DB().Exec(insert, "github", "owner/repo", "linux", "go126"); err == nil {
		t.Fatal("exact replay inserted duplicate result")
	}
	if _, err := s.DB().Exec(insert, "gitlab", "owner/repo", "linux", "go126"); err != nil {
		t.Fatalf("other source merged: %v", err)
	}
	if _, err := s.DB().Exec(insert, "github", "other/repo", "linux", "go126"); err != nil {
		t.Fatalf("other project merged: %v", err)
	}
	var artifactPath, rawName, rawClassname, rawMessage, infraReason string
	if err := s.DB().QueryRow("SELECT artifact_path, raw_name, raw_classname, raw_message, infra_reason FROM test_results WHERE source='github' AND project='owner/repo' AND job_id='linux' AND environment_id='go126'").Scan(&artifactPath, &rawName, &rawClassname, &rawMessage, &infraReason); err != nil {
		t.Fatal(err)
	}
	if artifactPath != "raw/junit.xml" || rawName != "TestT" || rawClassname != "pkg.Suite" || rawMessage != "failure text" || infraReason != "runner error" {
		t.Fatalf("raw provenance changed: %q %q %q %q %q", artifactPath, rawName, rawClassname, rawMessage, infraReason)
	}
	var count int
	if err := s.DB().QueryRow("SELECT count(*) FROM test_results").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 5 {
		t.Fatalf("result rows = %d, want 5", count)
	}
	if _, err := s.DB().Exec("INSERT INTO test_results(commit_sha,test_id,status,observed_at,job_id) VALUES('sha','go:p:T','pass','2026-09-29T00:00:00Z',NULL)"); err == nil {
		t.Fatal("NULL job identity accepted")
	}
}

func TestInvalidEnumValuesAreRejected(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "hazard.db"))
	if err != nil {
		t.Fatal(err)
	}
	closeOnCleanup(t, s)
	statements := []string{
		"INSERT INTO tests(id,lang,package,name,file,first_seen,last_seen) VALUES('x','ruby','p','T','f','now','now')",
		"INSERT INTO test_results(commit_sha,test_id,status,observed_at) VALUES('sha','t','unknown','now')",
		"INSERT INTO regressions(commit_sha,test_id,confidence,evidence) VALUES('sha','t','maybe','{}')",
		"INSERT INTO excluded_labels(commit_sha,test_id,reason) VALUES('sha','t','unknown')",
		"INSERT INTO mutants(id,file,line,operator,original,mutated,base_sha,status,created_at) VALUES('m','f',1,'op','a','b','sha','unknown','now')",
		"INSERT INTO judgments(cache_key,test_id,p_break,predictor,model_version,rubric_version,created_at) VALUES('k','t',0.5,'unknown','v','r','now')",
		"INSERT INTO selections(id,commit_sha,created_at,mode,tier,abstained,lambda,lock_digest,n_total,n_certain,n_added,est_ms_sel,est_ms_full,overhead_ms) VALUES('s','sha','now','unknown','suite',0,0.5,'x',1,1,0,1,1,0)",
		"INSERT INTO selections(id,commit_sha,created_at,mode,tier,abstained,lambda,lock_digest,n_total,n_certain,n_added,est_ms_sel,est_ms_full,overhead_ms) VALUES('s','sha','now','select','unknown',0,0.5,'x',1,1,0,1,1,0)",
		"INSERT INTO selection_members(selection_id,test_id,source) VALUES('s','t','unknown')",
		"INSERT INTO calibrations(id,created_at,alpha,delta,lambda,n_cal,emp_miss_rate,split,predictor,model_version,rubric_version,grid,pvalues) VALUES('c','now',0.01,0.05,0.5,1,0,'unknown','static','v','r','[]','{}')",
	}
	for _, statement := range statements {
		if _, err := s.DB().Exec(statement); err == nil {
			t.Errorf("invalid enum accepted: %s", statement)
		}
	}
}

func TestCloseReleasesFileAndInvalidPathFails(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(dir); err == nil {
		t.Fatal("directory accepted as database file")
	}
	path := filepath.Join(dir, "hazard.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().Ping(); err == nil {
		t.Fatal("closed database remained usable")
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("database remained open: %v", err)
	}
}

func TestNumberedMigrationsApplyInOrderAndRetryFailedVersion(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "hazard.db"))
	if err != nil {
		t.Fatal(err)
	}
	closeOnCleanup(t, s)
	scripts := fstest.MapFS{
		"migrations/0002_extra.sql": &fstest.MapFile{Data: []byte("CREATE TABLE extra (id INTEGER);")},
		"migrations/0003_bad.sql":   &fstest.MapFile{Data: []byte("CREATE TABLE partial (id INTEGER); INSERT INTO no_table VALUES (1);")},
	}
	if err := migrate(s.DB(), scripts); err == nil {
		t.Fatal("bad third migration succeeded")
	}
	var count int
	if err := s.DB().QueryRow("SELECT count(*) FROM sqlite_master WHERE name='extra'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("second migration did not apply")
	}
	if err := s.DB().QueryRow("SELECT count(*) FROM sqlite_master WHERE name='partial'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("third migration partially applied")
	}
	scripts["migrations/0003_bad.sql"] = &fstest.MapFile{Data: []byte("CREATE TABLE recovered (id INTEGER);")}
	if err := migrate(s.DB(), scripts); err != nil {
		t.Fatal(err)
	}
	if err := migrate(s.DB(), scripts); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRow("SELECT count(*) FROM schema_migrations").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("migration versions = %d, want 3", count)
	}
}

func TestReplacementConnectionPreservesPragmas(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "hazard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	}()

	conn, err := s.DB().Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Raw(func(any) error { return driver.ErrBadConn }); !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("retire connection: %v", err)
	}
	if err := conn.Close(); err != nil && !errors.Is(err, sql.ErrConnDone) {
		t.Fatal(err)
	}

	var foreignKeys, busyTimeout int
	var mode string
	if err := s.DB().QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRow("PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if foreignKeys != 1 || busyTimeout != 5000 || mode != "wal" {
		t.Fatalf("replacement pragmas: foreign_keys=%d busy_timeout=%d journal_mode=%s", foreignKeys, busyTimeout, mode)
	}
}

func TestOpenPreservesSpecialFilename(t *testing.T) {
	name := "store # é % &.sqlite"
	if runtime.GOOS != "windows" {
		name = "store ? # é % &.sqlite"
	}
	path := filepath.Join(t.TempDir(), name)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec("PRAGMA user_version=7"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database missing at requested path: %v", err)
	}
	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := again.Close(); err != nil {
			t.Error(err)
		}
	}()
	var version int
	if err := again.DB().QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 7 {
		t.Fatalf("reopened user_version=%d, want 7", version)
	}
}

func closeOnCleanup(t *testing.T, s *Store) {
	t.Helper()
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
}
