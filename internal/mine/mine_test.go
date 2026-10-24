package mine

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omorsi45/hazard/internal/store"
)

func mineRequest(t *testing.T) Request {
	t.Helper()
	dir := t.TempDir()
	return Request{Provider: "github", Project: "owner/repo", Branch: "main", Language: "go", Since: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Until: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC), MaxPipelines: 10, Concurrency: 2, DBPath: filepath.Join(dir, "db.sqlite"), RawDir: filepath.Join(dir, "raw")}
}

func TestMineValidationBeforeStore(t *testing.T) {
	cases := map[string]func(*Request){
		"provider": func(r *Request) { r.Provider = "other" }, "project": func(r *Request) { r.Project = "../repo" }, "language": func(r *Request) { r.Language = "rust" },
		"limit": func(r *Request) { r.MaxPipelines = 0 }, "concurrency": func(r *Request) { r.Concurrency = 0 }, "db": func(r *Request) { r.DBPath = "" }, "raw": func(r *Request) { r.RawDir = "" },
		"glob": func(r *Request) { r.JUnitGlob = "[" }, "equal": func(r *Request) { r.Until = r.Since }, "reversed": func(r *Request) { r.Until = r.Since.Add(-time.Hour) },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			r := mineRequest(t)
			db := r.DBPath
			raw := r.RawDir
			change(&r)
			if _, err := Mine(context.Background(), r, Options{}); err == nil {
				t.Fatal("invalid request accepted before store")
			}
			if _, err := os.Stat(db); !os.IsNotExist(err) {
				t.Fatal("invalid request touched store")
			}
			if _, err := os.Stat(raw); !os.IsNotExist(err) {
				t.Fatal("invalid request touched raw")
			}
		})
	}
	b, err := json.Marshal(Options{Token: "private", APIBase: "private"})
	if err != nil || string(b) != "{}" {
		t.Fatalf("transport options serialized: %s %v", b, err)
	}
}

func TestMineBindingBeforeSource(t *testing.T) {
	r := mineRequest(t)
	s, err := store.Open(r.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.DB().Exec(`INSERT INTO mining_sources VALUES(1,'gitlab','other/repo','original')`)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++; w.WriteHeader(500) }))
	defer server.Close()
	if _, err = Mine(context.Background(), r, Options{APIBase: server.URL, HTTPClient: server.Client()}); err == nil {
		t.Fatal("binding mismatch accepted")
	}
	if calls != 0 {
		t.Fatalf("binding mismatch requested source: %d", calls)
	}
	if _, err = os.Stat(r.RawDir); !os.IsNotExist(err) {
		t.Fatal("binding mismatch touched raw")
	}
}

func TestMineEmptyDiscoveryBindsLocal(t *testing.T) {
	r := mineRequest(t)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"total_count":0,"workflow_runs":[]}`))
	}))
	defer server.Close()
	before := time.Now().UTC()
	stats, err := Mine(context.Background(), r, Options{APIBase: server.URL, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("discovery calls=%d", calls)
	}
	if !stats.RequestedSince.Equal(r.Since) || !stats.RequestedUntil.Equal(r.Until) {
		t.Fatal("current bounds missing")
	}
	s, err := store.Open(r.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	var p, project, stamp string
	if err = s.DB().QueryRow(`SELECT provider,project,observed_at FROM mining_sources`).Scan(&p, &project, &stamp); err != nil {
		t.Fatal(err)
	}
	acquired, err := time.Parse(observationTimeFormat, stamp)
	if err != nil || acquired.Before(before) || acquired.After(time.Now().UTC()) {
		t.Fatal("binding did not use local acquisition time")
	}
	if p != r.Provider || project != r.Project {
		t.Fatal("wrong binding")
	}
	persistCount(t, s.DB(), "ci_artifacts", 0)
	if _, err = os.Stat(r.RawDir); !os.IsNotExist(err) {
		t.Fatal("empty discovery invented raw")
	}
}

type mineProvider struct {
	list    func(context.Context) ([]sourceRun, []acquisitionGap, error)
	acquire func(context.Context, sourceRun, acquisitionSink) (runOutcome, error)
}

func (p mineProvider) listRuns(c context.Context) ([]sourceRun, []acquisitionGap, error) {
	if p.list == nil {
		return nil, nil, nil
	}
	return p.list(c)
}
func (p mineProvider) acquireRun(c context.Context, r sourceRun, s acquisitionSink) (runOutcome, error) {
	if p.acquire == nil {
		return runOutcome{State: "complete"}, nil
	}
	return p.acquire(c, r, s)
}
func mineCheck(t *testing.T, r Request, check func(*sql.DB)) {
	t.Helper()
	s, err := store.Open(r.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	check(s.DB())
}
func mineScalar(t *testing.T, db *sql.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
func mineGapProvider(gaps ...acquisitionGap) mineProvider {
	return mineProvider{list: func(context.Context) ([]sourceRun, []acquisitionGap, error) { return nil, gaps, nil }}
}
func mineDo(t *testing.T, r Request, p acquisitionProvider) Stats {
	t.Helper()
	s, err := mineWithProvider(context.Background(), r, p)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestMineGapEpisodesAndActualBytes(t *testing.T) {
	r := mineRequest(t)
	stamp := r.Since
	later := stamp.Add(time.Hour)
	gaps := []acquisitionGap{{UnitKey: "runs", Reason: "discovery_capped", Detail: "request_limit"}, {UnitKey: "empty", Reason: "parse_failed", Raw: []byte{}}, {UnitKey: "body", Reason: "parse_failed", Raw: []byte("bad"), SourceTime: &stamp, JobID: persistPtr("original")}}
	mineDo(t, r, mineGapProvider(gaps...))
	mineCheck(t, r, func(db *sql.DB) {
		if n := mineScalar(t, db, `SELECT count(*) FROM acquisition_gaps`); n != 3 {
			t.Fatalf("gap-only evidence missing: %d", n)
		}
		if n := mineScalar(t, db, `SELECT count(*) FROM acquisition_gaps WHERE content_sha256 IS NULL AND source_time IS NULL AND job_id IS NULL AND attempt IS NULL`); n != 1 {
			t.Fatalf("unavailable bytes/source facts fabricated: %d", n)
		}
		rows, err := db.Query(`SELECT content_sha256,raw_blob_path FROM acquisition_gaps WHERE content_sha256 IS NOT NULL`)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var digest, p string
			if err = rows.Scan(&digest, &p); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprintf("%x", sha256.Sum256(data)) != digest {
				t.Fatal("gap raw hash mismatch")
			}
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		persistCount(t, db, "ci_artifacts", 0)
		persistCount(t, db, "ci_observations", 0)
	})
	gaps[2].SourceTime = &later
	gaps[2].JobID = persistPtr("changed")
	mineDo(t, r, mineGapProvider(gaps...))
	mineCheck(t, r, func(db *sql.DB) {
		persistCount(t, db, "acquisition_gaps", 3)
		var first, last, source, job string
		if err := db.QueryRow(`SELECT first_acquired_at,last_acquired_at,source_time,job_id FROM acquisition_gaps WHERE job_id IS NOT NULL`).Scan(&first, &last, &source, &job); err != nil {
			t.Fatal(err)
		}
		if last < first || source != stamp.Format(observationTimeFormat) || job != "original" {
			t.Fatal("active repeat replaced original source facts")
		}
	})
	gaps[2].Raw = []byte("different")
	mineDo(t, r, mineGapProvider(gaps[2]))
	mineCheck(t, r, func(db *sql.DB) { persistCount(t, db, "acquisition_gaps", 4) })
	mineDo(t, r, mineProvider{})
	mineCheck(t, r, func(db *sql.DB) {
		if n := mineScalar(t, db, `SELECT count(*) FROM acquisition_gaps WHERE resolved_at IS NOT NULL`); n != 4 {
			t.Fatalf("complete discovery did not resolve history: %d", n)
		}
	})
	mineDo(t, r, mineGapProvider(gaps[0]))
	mineCheck(t, r, func(db *sql.DB) { persistCount(t, db, "acquisition_gaps", 5) })
}

func TestMineDiscoveryRecoveryExactQuery(t *testing.T) {
	r := mineRequest(t)
	mineDo(t, r, mineGapProvider(acquisitionGap{UnitKey: "runs", Reason: "discovery_capped"}))
	narrow := r
	narrow.Until = r.Since.Add(time.Hour)
	mineDo(t, narrow, mineProvider{})
	branch := r
	branch.Branch = "other"
	mineDo(t, branch, mineProvider{})
	mineCheck(t, r, func(db *sql.DB) {
		if n := mineScalar(t, db, `SELECT count(*) FROM acquisition_gaps WHERE resolved_at IS NULL`); n != 1 {
			t.Fatal("narrower/different discovery resolved wider gap")
		}
	})
	r.MaxPipelines++
	mineDo(t, r, mineProvider{})
	mineCheck(t, r, func(db *sql.DB) {
		if n := mineScalar(t, db, `SELECT count(*) FROM acquisition_gaps WHERE resolved_at IS NULL`); n != 0 {
			t.Fatal("same query with higher cap failed recovery")
		}
	})
}

func mineArtifact() acquiredArtifact {
	a := acquiredArtifact{Observation: persistFixture(), Raw: []byte("actual report bytes")}
	a.Observation.ContentSHA256 = fmt.Sprintf("%x", sha256.Sum256(a.Raw))
	return a
}
func mineRunProvider(a acquiredArtifact) mineProvider {
	return mineProvider{list: func(context.Context) ([]sourceRun, []acquisitionGap, error) {
		return []sourceRun{{ID: a.Observation.RunID, SourceHeadSHA: a.Observation.SourceHeadSHA, Branch: a.Observation.Branch, RunAttempt: a.Observation.RunAttempt}}, nil, nil
	}, acquire: func(_ context.Context, _ sourceRun, s acquisitionSink) (runOutcome, error) {
		err := s.Artifact(a)
		return runOutcome{State: "complete", Attempts: []int{1}}, err
	}}
}

func TestMineRawBeforeObservationAndAcknowledgment(t *testing.T) {
	r := mineRequest(t)
	a := mineArtifact()
	p := mineRunProvider(a)
	ack := false
	p.acquire = func(_ context.Context, _ sourceRun, s acquisitionSink) (runOutcome, error) {
		if err := s.Artifact(a); err != nil {
			return runOutcome{}, err
		}
		mineCheck(t, r, func(db *sql.DB) {
			persistCount(t, db, "ci_observations", 1)
			var rawPath string
			if err := db.QueryRow(`SELECT raw_blob_path FROM ci_artifacts`).Scan(&rawPath); err != nil {
				t.Fatal(err)
			}
			if rawPath != filepath.Join(r.RawDir, a.Observation.ContentSHA256) {
				t.Fatal("observation did not retain published raw path")
			}
		})
		data, err := os.ReadFile(filepath.Join(r.RawDir, a.Observation.ContentSHA256))
		if err != nil || string(data) != string(a.Raw) {
			t.Fatal("callback acknowledged before durable evidence")
		}
		ack = true
		return runOutcome{State: "complete", Attempts: []int{1}}, nil
	}
	stats := mineDo(t, r, p)
	if !ack || stats.RawCases != 1 || stats.FactualResults != 1 {
		t.Fatalf("artifact not persisted/acknowledged: %+v", stats)
	}
}

func TestMineRawAndDBFailures(t *testing.T) {
	for _, kind := range []string{"digest", "directory", "corrupt", "database"} {
		t.Run(kind, func(t *testing.T) {
			r := mineRequest(t)
			a := mineArtifact()
			switch kind {
			case "digest":
				a.Observation.ContentSHA256 = strings.Repeat("0", 64)
			case "directory":
				if err := os.WriteFile(r.RawDir, []byte("file"), 0600); err != nil {
					t.Fatal(err)
				}
			case "corrupt":
				if err := os.MkdirAll(r.RawDir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(r.RawDir, a.Observation.ContentSHA256), []byte("corrupt"), 0600); err != nil {
					t.Fatal(err)
				}
			case "database":
				mineCheck(t, r, func(db *sql.DB) {
					if _, err := db.Exec(`CREATE TRIGGER fail_observation BEFORE INSERT ON ci_observations BEGIN SELECT RAISE(ABORT,'fixture database failure'); END`); err != nil {
						t.Fatal(err)
					}
				})
			}
			callbackErr := error(nil)
			p := mineRunProvider(a)
			p.acquire = func(_ context.Context, _ sourceRun, s acquisitionSink) (runOutcome, error) {
				callbackErr = s.Artifact(a)
				return runOutcome{State: "complete", Attempts: []int{1}}, callbackErr
			}
			_, err := mineWithProvider(context.Background(), r, p)
			if err == nil || callbackErr == nil || err != callbackErr {
				t.Fatalf("writer error identity lost: returned=%v callback=%v", err, callbackErr)
			}
			mineCheck(t, r, func(db *sql.DB) {
				persistCount(t, db, "ci_observations", 0)
				if n := mineScalar(t, db, `SELECT count(*) FROM mining_runs WHERE state='complete'`); n != 0 {
					t.Fatal("failed callback completed run")
				}
			})
			if kind == "database" {
				data, readErr := os.ReadFile(filepath.Join(r.RawDir, a.Observation.ContentSHA256))
				if readErr != nil || string(data) != string(a.Raw) {
					t.Fatal("failed database lost prior durable raw")
				}
			}
			if kind == "digest" {
				if _, statErr := os.Stat(r.RawDir); !os.IsNotExist(statErr) {
					t.Fatal("digest disagreement wrote raw")
				}
			}
		})
	}
}

func TestMineBoundedWorkersCancelJoin(t *testing.T) {
	r := mineRequest(t)
	r.Concurrency = 2
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{}, 5)
	left := make(chan struct{}, 5)
	var active, maximum atomic.Int32
	p := mineProvider{list: func(context.Context) ([]sourceRun, []acquisitionGap, error) {
		runs := make([]sourceRun, 5)
		for i := range runs {
			runs[i] = sourceRun{ID: strconv.Itoa(i + 1), RunAttempt: persistPtr(3)}
		}
		return runs, nil, nil
	}, acquire: func(c context.Context, _ sourceRun, _ acquisitionSink) (runOutcome, error) {
		n := active.Add(1)
		for old := maximum.Load(); n > old && !maximum.CompareAndSwap(old, n); old = maximum.Load() {
		}
		entered <- struct{}{}
		<-c.Done()
		active.Add(-1)
		left <- struct{}{}
		return runOutcome{State: "incomplete"}, c.Err()
	}}
	done := make(chan error, 1)
	var got Stats
	go func() { var err error; got, err = mineWithProvider(ctx, r, p); done <- err }()
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("bounded workers never entered")
		}
	}
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("original context error lost: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("workers not joined")
	}
	if got.Activity.InspectedRuns != 2 || got.Activity.InspectedAttempts != 0 || got.Activity.IncompleteRuns != 2 {
		t.Fatalf("unstarted/failed work counted as inspected: %+v", got.Activity)
	}
	if maximum.Load() != 2 || active.Load() != 0 || len(left) != 2 || len(entered) != 0 {
		t.Fatalf("worker bound/join violated: max=%d active=%d left=%d", maximum.Load(), active.Load(), len(left))
	}
	mineCheck(t, r, func(db *sql.DB) {
		if n := mineScalar(t, db, `SELECT count(*) FROM mining_runs WHERE state='incomplete' AND attempt=3`); n != 5 {
			t.Fatalf("known attempts lost or invented: %d", n)
		}
	})
}

func mineSequence(artifacts ...acquiredArtifact) mineProvider {
	p := mineRunProvider(artifacts[0])
	p.acquire = func(_ context.Context, _ sourceRun, s acquisitionSink) (runOutcome, error) {
		for _, a := range artifacts {
			if err := s.Artifact(a); err != nil {
				return runOutcome{State: "incomplete"}, err
			}
		}
		return runOutcome{State: "complete", Attempts: []int{1}}, nil
	}
	return p
}
func mineVariant(a acquiredArtifact, raw, testID string) acquiredArtifact {
	a.Raw = []byte(raw)
	a.Observation.ContentSHA256 = fmt.Sprintf("%x", sha256.Sum256(a.Raw))
	a.Observation.Entries = append([]ObservationEntry(nil), a.Observation.Entries...)
	a.Observation.Entries[0].Cases = append([]ObservationCase(nil), a.Observation.Entries[0].Cases...)
	a.Observation.Entries[0].Cases[0].Identity.ID = testID
	return a
}
func TestMineTypedConflictStoredFullKeys(t *testing.T) {
	r := mineRequest(t)
	a := mineArtifact()
	b := mineVariant(a, "second version", "go:example.org/p:TestOther")
	other := a
	other.Observation.UnitKey = "unrelated"
	other.Observation.ArtifactID = persistPtr("unrelated")
	mineDo(t, r, mineSequence(a, b, other))
	mineCheck(t, r, func(db *sql.DB) { persistCount(t, db, "test_results", 3) })
	rejected := a
	rejected.Observation.RunID = "rejected-run"
	rejected.Observation.TestedSHA = persistPtr("rejected-sha")
	stats := mineDo(t, r, mineRunProvider(rejected))
	if stats.FactualResults != 1 {
		t.Fatalf("stored full keys not withdrawn: results=%d", stats.FactualResults)
	}
	mineCheck(t, r, func(db *sql.DB) {
		persistCount(t, db, "ci_observations", 3)
		persistCount(t, db, "tests", 2)
		persistCount(t, db, "commits", 1)
		if n := mineScalar(t, db, `SELECT count(*) FROM test_results WHERE artifact_id='unrelated'`); n != 1 {
			t.Fatal("unrelated full result key withdrawn")
		}
		if n := mineScalar(t, db, `SELECT count(*) FROM acquisition_gaps WHERE reason='acquisition_conflict' AND resolved_at IS NULL AND content_sha256 IS NOT NULL`); n != 1 {
			t.Fatal("typed conflict not durable")
		}
		if n := mineScalar(t, db, `SELECT count(*) FROM mining_runs WHERE state='complete'`); n != 0 {
			t.Fatal("stored unit run checkpoint not invalidated")
		}
	})
	mineDo(t, r, mineSequence(a, b))
	mineCheck(t, r, func(db *sql.DB) {
		persistCount(t, db, "test_results", 1)
		if n := mineScalar(t, db, `SELECT count(*) FROM acquisition_gaps WHERE reason='acquisition_conflict' AND resolved_at IS NULL`); n != 1 {
			t.Fatal("ordinary replay cleared conflict")
		}
	})
}

func TestMineExplicitConflictBeforeReplay(t *testing.T) {
	r := mineRequest(t)
	a := mineArtifact()
	mineDo(t, r, mineRunProvider(a))
	p := mineRunProvider(a)
	p.acquire = func(_ context.Context, run sourceRun, s acquisitionSink) (runOutcome, error) {
		if err := s.Gap(acquisitionGap{RunID: run.ID, UnitKey: a.Observation.UnitKey, Reason: "acquisition_conflict", Detail: "multiple_verified_producers"}); err != nil {
			return runOutcome{}, err
		}
		mineCheck(t, r, func(db *sql.DB) {
			persistCount(t, db, "test_results", 0)
			if n := mineScalar(t, db, `SELECT count(*) FROM mining_runs WHERE state='complete'`); n != 0 {
				t.Fatal("conflict callback acknowledged before invalidation")
			}
		})
		unknown := a
		unknown.Observation.TestedSHA = nil
		unknown.Observation.ExecutionEvidence = nil
		for _, v := range []acquiredArtifact{unknown, a} {
			if err := s.Artifact(v); err != nil {
				return runOutcome{}, err
			}
		}
		return runOutcome{State: "complete", Attempts: []int{1}}, nil
	}
	mineDo(t, r, p)
	mineCheck(t, r, func(db *sql.DB) {
		persistCount(t, db, "test_results", 0)
		if n := mineScalar(t, db, `SELECT count(*) FROM acquisition_gaps WHERE reason='acquisition_conflict' AND resolved_at IS NULL`); n != 1 {
			t.Fatal("explicit conflict was resolved")
		}
	})
}

func TestMineConflictTransactionRollback(t *testing.T) {
	r := mineRequest(t)
	a := mineArtifact()
	mineDo(t, r, mineRunProvider(a))
	mineCheck(t, r, func(db *sql.DB) {
		if _, err := db.Exec(`CREATE TRIGGER fail_withdraw BEFORE DELETE ON test_results BEGIN SELECT RAISE(ABORT,'fixture withdrawal failure'); END`); err != nil {
			t.Fatal(err)
		}
	})
	rejected := a
	rejected.Observation.RunID = "rejected-run"
	rejected.Observation.JobID = persistPtr("different")
	if _, err := mineWithProvider(context.Background(), r, mineRunProvider(rejected)); err == nil {
		t.Fatal("failed conflict withdrawal acknowledged")
	}
	mineCheck(t, r, func(db *sql.DB) {
		persistCount(t, db, "test_results", 1)
		persistCount(t, db, "acquisition_gaps", 0)
		if n := mineScalar(t, db, `SELECT count(*) FROM mining_runs WHERE run_id='run-1' AND state='complete'`); n != 1 {
			t.Fatal("conflict failure did not roll back stored checkpoint")
		}
	})
}

func TestMineRunRecoveryConservative(t *testing.T) {
	r := mineRequest(t)
	a := mineArtifact()
	p := mineRunProvider(a)
	p.acquire = func(_ context.Context, run sourceRun, s acquisitionSink) (runOutcome, error) {
		err := s.Gap(acquisitionGap{RunID: run.ID, UnitKey: "jobs", Reason: "jobs_unavailable", Attempt: persistPtr(1)})
		return runOutcome{State: "incomplete", Attempts: []int{1}}, err
	}
	mineDo(t, r, p)
	incomplete := mineRunProvider(a)
	incomplete.acquire = func(_ context.Context, _ sourceRun, s acquisitionSink) (runOutcome, error) {
		err := s.Artifact(a)
		return runOutcome{State: "incomplete", Attempts: []int{1}}, err
	}
	mineDo(t, r, incomplete)
	mineCheck(t, r, func(db *sql.DB) {
		if n := mineScalar(t, db, `SELECT count(*) FROM acquisition_gaps WHERE resolved_at IS NULL`); n != 1 {
			t.Fatal("one successful artifact erased unfinished gap")
		}
		if n := mineScalar(t, db, `SELECT count(*) FROM mining_runs WHERE state='complete'`); n != 0 {
			t.Fatal("incomplete outcome completed checkpoint")
		}
	})
	complete := mineRunProvider(a)
	complete.acquire = func(context.Context, sourceRun, acquisitionSink) (runOutcome, error) {
		return runOutcome{State: "complete", Attempts: []int{1, 2}}, nil
	}
	mineDo(t, r, complete)
	mineCheck(t, r, func(db *sql.DB) {
		if n := mineScalar(t, db, `SELECT count(*) FROM acquisition_gaps WHERE resolved_at IS NOT NULL`); n != 1 {
			t.Fatal("complete run failed to resolve recoverable episode")
		}
		if n := mineScalar(t, db, `SELECT count(*) FROM mining_runs WHERE state='complete' AND attempt IN (1,2)`); n != 2 {
			t.Fatal("known no-artifact attempts not finalized")
		}
	})
	mineDo(t, r, p)
	mineCheck(t, r, func(db *sql.DB) { persistCount(t, db, "acquisition_gaps", 2) })
}

func TestMineCappedKnownAttempts(t *testing.T) {
	r := mineRequest(t)
	p := mineProvider{list: func(context.Context) ([]sourceRun, []acquisitionGap, error) {
		return []sourceRun{{ID: "run", RunAttempt: persistPtr(3)}}, nil, nil
	}, acquire: func(_ context.Context, run sourceRun, s acquisitionSink) (runOutcome, error) {
		err := s.Gap(acquisitionGap{RunID: run.ID, UnitKey: "jobs", Reason: "jobs_capped", Attempt: persistPtr(1)})
		return runOutcome{State: "capped"}, err
	}}
	stats := mineDo(t, r, p)
	if stats.Activity.InspectedRuns != 1 || stats.Activity.InspectedAttempts != 0 || stats.Activity.CappedRuns != 1 {
		t.Fatalf("failed/unreached capped ordinals counted inspected: %+v", stats.Activity)
	}
	mineCheck(t, r, func(db *sql.DB) {
		if n := mineScalar(t, db, `SELECT count(*) FROM mining_runs WHERE state='capped' AND attempt IN (1,3)`); n != 2 {
			t.Fatalf("known failed/capped attempt facts not preserved: %d", n)
		}
		if n := mineScalar(t, db, `SELECT count(*) FROM mining_runs WHERE attempt=2`); n != 0 {
			t.Fatal("unreached attempt ordinal invented")
		}
	})
}

func TestMineCompletionTransactionRollback(t *testing.T) {
	r := mineRequest(t)
	a := mineArtifact()
	mineDo(t, r, mineProvider{list: func(context.Context) ([]sourceRun, []acquisitionGap, error) {
		return []sourceRun{{ID: a.Observation.RunID, RunAttempt: persistPtr(1)}}, nil, nil
	}, acquire: func(_ context.Context, run sourceRun, s acquisitionSink) (runOutcome, error) {
		err := s.Gap(acquisitionGap{RunID: run.ID, UnitKey: "jobs", Reason: "jobs_unavailable"})
		return runOutcome{State: "incomplete"}, err
	}})
	mineCheck(t, r, func(db *sql.DB) {
		if _, err := db.Exec(`CREATE TRIGGER fail_complete BEFORE UPDATE ON mining_runs WHEN NEW.state='complete' BEGIN SELECT RAISE(ABORT,'fixture complete failure'); END`); err != nil {
			t.Fatal(err)
		}
	})
	p := mineRunProvider(a)
	p.acquire = func(context.Context, sourceRun, acquisitionSink) (runOutcome, error) {
		return runOutcome{State: "complete", Attempts: []int{1}}, nil
	}
	if _, err := mineWithProvider(context.Background(), r, p); err == nil {
		t.Fatal("failed checkpoint acknowledged")
	}
	mineCheck(t, r, func(db *sql.DB) {
		if n := mineScalar(t, db, `SELECT count(*) FROM acquisition_gaps WHERE resolved_at IS NULL`); n != 1 {
			t.Fatal("failed checkpoint committed gap resolution")
		}
	})
}

func TestMineGitLabKnownMetadataExclusionComplete(t *testing.T) {
	r := mineRequest(t)
	r.Provider = "gitlab"
	r.Project = "group/repo"
	a := mineArtifact()
	a.Observation.Provider = r.Provider
	a.Observation.Project = r.Project
	a.Observation.Attempt = nil
	a.Observation.RunAttempt = nil
	a.Observation.Commit.FilesChanged = nil
	a.Observation.ExclusionReason = "commit_file_count_unverified"
	p := mineRunProvider(a)
	p.acquire = func(_ context.Context, _ sourceRun, s acquisitionSink) (runOutcome, error) {
		err := s.Artifact(a)
		return runOutcome{State: "complete"}, err
	}
	stats := mineDo(t, r, p)
	if stats.Activity.InspectedAttempts != 0 {
		t.Fatal("GitLab unknown ordinal counted as inspected")
	}
	if stats.FactualResults != 1 {
		t.Fatal("known metadata limitation suppressed factual results")
	}
	mineCheck(t, r, func(db *sql.DB) {
		persistCount(t, db, "acquisition_gaps", 0)
		if n := mineScalar(t, db, `SELECT count(*) FROM mining_runs WHERE attempt IS NOT NULL`); n != 0 {
			t.Fatal("GitLab ordinal invented")
		}
		persistCount(t, db, "commits", 0)
		if n := mineScalar(t, db, `SELECT count(*) FROM mining_runs WHERE state='complete' AND attempt IS NULL`); n != 1 {
			t.Fatal("known metadata limitation became acquisition failure or ordinal")
		}
	})
}

func TestMineSourceErrorsRetainAcknowledgedPartial(t *testing.T) {
	for _, sourceErr := range []error{errors.New("fixture source failure"), context.Canceled, context.DeadlineExceeded, &httpError{Reason: "auth"}, &httpError{Reason: "rate_limited"}} {
		t.Run(sourceErr.Error(), func(t *testing.T) {
			r := mineRequest(t)
			r.Concurrency = 1
			a := mineArtifact()
			calls := 0
			p := mineRunProvider(a)
			p.list = func(context.Context) ([]sourceRun, []acquisitionGap, error) {
				return []sourceRun{{ID: a.Observation.RunID, RunAttempt: persistPtr(3)}, {ID: "later"}}, nil, nil
			}
			p.acquire = func(_ context.Context, run sourceRun, s acquisitionSink) (runOutcome, error) {
				calls++
				if run.ID == "later" {
					return runOutcome{State: "complete"}, nil
				}
				if err := s.Artifact(a); err != nil {
					return runOutcome{}, err
				}
				return runOutcome{State: "incomplete", Attempts: []int{1}}, sourceErr
			}
			stats, err := mineWithProvider(context.Background(), r, p)
			if err != sourceErr || stats.RawCases != 1 || stats.FactualResults != 1 {
				t.Fatalf("partial source evidence/original error lost: %v %+v", err, stats)
			}
			want := 2
			if mineFatalSource(sourceErr) {
				want = 1
			}
			if calls != want {
				t.Fatalf("source fatal/ordinary scheduling calls=%d want=%d", calls, want)
			}
			mineCheck(t, r, func(db *sql.DB) {
				if n := mineScalar(t, db, `SELECT count(*) FROM mining_runs WHERE run_id='run-1' AND state='incomplete' AND attempt IN (1,3)`); n != 2 {
					t.Fatal("source error lost known versus inspected prefix facts")
				}
			})
		})
	}
}

func TestMineCanceledCleanupRetainsPrimaryFailure(t *testing.T) {
	r := mineRequest(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := mineArtifact()
	p := mineRunProvider(a)
	p.acquire = func(context.Context, sourceRun, acquisitionSink) (runOutcome, error) {
		mineCheck(t, r, func(db *sql.DB) {
			if _, err := db.Exec(`CREATE TRIGGER fail_cleanup BEFORE UPDATE ON mining_runs BEGIN SELECT RAISE(ABORT,'fixture cleanup failure'); END`); err != nil {
				t.Fatal(err)
			}
		})
		cancel()
		return runOutcome{State: "complete", Attempts: []int{1}}, ctx.Err()
	}
	_, err := mineWithProvider(ctx, r, p)
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "fixture cleanup failure") {
		t.Fatalf("primary cancellation or cleanup failure lost: %v", err)
	}
}

func TestMineCanceledCleanupCannotResolve(t *testing.T) {
	r := mineRequest(t)
	a := mineArtifact()
	p := mineRunProvider(a)
	p.acquire = func(_ context.Context, run sourceRun, s acquisitionSink) (runOutcome, error) {
		err := s.Gap(acquisitionGap{RunID: run.ID, UnitKey: "jobs", Reason: "jobs_unavailable"})
		return runOutcome{State: "incomplete"}, err
	}
	mineDo(t, r, p)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.acquire = func(context.Context, sourceRun, acquisitionSink) (runOutcome, error) {
		cancel()
		return runOutcome{State: "complete", Attempts: []int{1}}, nil
	}
	if _, err := mineWithProvider(ctx, r, p); err != context.Canceled {
		t.Fatalf("canceled primary identity changed: %v", err)
	}
	mineCheck(t, r, func(db *sql.DB) {
		if n := mineScalar(t, db, `SELECT count(*) FROM acquisition_gaps WHERE resolved_at IS NULL`); n != 1 {
			t.Fatal("canceled cleanup resolved gap")
		}
		if n := mineScalar(t, db, `SELECT count(*) FROM mining_runs WHERE state='complete'`); n != 0 {
			t.Fatal("canceled cleanup marked complete")
		}
	})
}

func TestMineActivityReplayAndLaterAttempt(t *testing.T) {
	r := mineRequest(t)
	a := mineArtifact()
	calls := 0
	p := mineRunProvider(a)
	acquire := p.acquire
	p.acquire = func(c context.Context, run sourceRun, s acquisitionSink) (runOutcome, error) {
		calls++
		return acquire(c, run, s)
	}
	first := mineDo(t, r, p)
	if first.Activity.InspectedRuns != 1 || first.Activity.InspectedAttempts != 1 || first.Activity.PersistedArtifacts != 1 || first.Activity.NewRawCases != 1 || first.Activity.NetFactualResults != 1 {
		t.Fatalf("initial request activity missing: %+v", first.Activity)
	}
	r.Resume = true
	replay := mineDo(t, r, p)
	if calls != 2 || replay.RawCases != 1 || replay.Activity.NewRawCases != 0 || replay.Activity.ReplayCases != 1 || replay.Activity.NetFactualResults != 0 || replay.Activity.PersistedArtifacts != 1 {
		t.Fatalf("resume skipped or confused cumulative/replay counts: %+v", replay)
	}
	p.list = func(context.Context) ([]sourceRun, []acquisitionGap, error) {
		return []sourceRun{{ID: a.Observation.RunID, RunAttempt: persistPtr(3)}}, nil, nil
	}
	p.acquire = func(_ context.Context, run sourceRun, s acquisitionSink) (runOutcome, error) {
		calls++
		err := s.Gap(acquisitionGap{RunID: run.ID, UnitKey: "jobs", Attempt: persistPtr(3), Reason: "jobs_unavailable"})
		return runOutcome{State: "incomplete", Attempts: []int{1, 2}}, err
	}
	later := mineDo(t, r, p)
	if calls != 3 || later.Activity.InspectedRuns != 1 || later.Activity.InspectedAttempts != 2 || later.InspectedAttempts != 3 || later.Activity.IncompleteRuns != 1 || later.Activity.PersistedArtifacts != 0 || later.Activity.GapsByReason["jobs_unavailable"] != 1 || later.ActiveGaps != 1 {
		t.Fatalf("later attempt inspection/known failure facts confused: %+v %+v", later, later.Activity)
	}
}

func TestMineNegativeActivityAndConflictReplay(t *testing.T) {
	r := mineRequest(t)
	a := mineArtifact()
	mineDo(t, r, mineRunProvider(a))
	p := mineRunProvider(a)
	p.acquire = func(_ context.Context, run sourceRun, s acquisitionSink) (runOutcome, error) {
		err := s.Gap(acquisitionGap{RunID: run.ID, UnitKey: a.Observation.UnitKey, Reason: "acquisition_conflict"})
		return runOutcome{State: "incomplete"}, err
	}
	conflict := mineDo(t, r, p)
	if conflict.Activity.NetFactualResults != -1 || conflict.Activity.NewRawCases != 0 || conflict.Activity.GapsByReason["acquisition_conflict"] != 1 || conflict.ActiveGaps != 1 {
		t.Fatalf("negative net/conflict activity hidden: %+v %+v", conflict, conflict.Activity)
	}
	replay := mineDo(t, r, mineRunProvider(a))
	if replay.Activity.NetFactualResults != 0 || replay.Activity.ReplayCases != 1 || replay.Activity.PersistedArtifacts != 1 || replay.Activity.IncompleteRuns != 1 || replay.ActiveGaps != 1 || replay.FactualResults != 0 {
		t.Fatalf("conflicted replay activity/completion false: %+v %+v", replay, replay.Activity)
	}
}

func TestMineWriterFailureStopsLaterCallbacks(t *testing.T) {
	r := mineRequest(t)
	a := mineArtifact()
	bad := a
	bad.Observation.ContentSHA256 = strings.Repeat("0", 64)
	var callbackErr error
	p := mineRunProvider(a)
	p.acquire = func(_ context.Context, _ sourceRun, s acquisitionSink) (runOutcome, error) {
		callbackErr = s.Artifact(bad)
		_ = s.Artifact(a)
		return runOutcome{State: "complete"}, nil
	}
	stats, err := mineWithProvider(context.Background(), r, p)
	if callbackErr == nil || err != callbackErr || stats.RawCases != 0 {
		t.Fatalf("writer failure admitted later callback or lost original error: %v %+v", err, stats)
	}
	if _, err = os.Stat(r.RawDir); !os.IsNotExist(err) {
		t.Fatal("later callback published raw after writer cancellation")
	}
}

func TestMineBackpressureWaitsForDatabase(t *testing.T) {
	r := mineRequest(t)
	a := mineArtifact()
	entered := make(chan struct{})
	permit := make(chan struct{})
	acknowledged := make(chan error, 1)
	done := make(chan error, 1)
	p := mineRunProvider(a)
	p.acquire = func(_ context.Context, _ sourceRun, s acquisitionSink) (runOutcome, error) {
		close(entered)
		<-permit
		err := s.Artifact(a)
		acknowledged <- err
		return runOutcome{State: "complete", Attempts: []int{1}}, err
	}
	go func() { _, err := mineWithProvider(context.Background(), r, p); done <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not enter")
	}
	lock, err := store.Open(r.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	if _, err = lock.DB().Exec(`BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = lock.DB().Exec(`ROLLBACK`) }()
	close(permit)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err = os.Stat(filepath.Join(r.RawDir, a.Observation.ContentSHA256)); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("writer never published raw before blocked database")
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case <-acknowledged:
		t.Fatal("callback acknowledged while database write blocked")
	default:
	}
	if _, err = lock.DB().Exec(`ROLLBACK`); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-acknowledged:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("callback not acknowledged after database released")
	}
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("writer did not join")
	}
}

func TestMineActivityStringDeterministic(t *testing.T) {
	s := Stats{Activity: Activity{GapsByReason: map[string]int{"z_reason": 2, "a_reason": 1}}, ActiveGaps: 3, ResolvedGaps: 4}
	text := s.String()
	if !strings.Contains(text, "active_gaps=3 resolved_gaps=4") || !strings.Contains(text, "activity_gap_a_reason=1 activity_gap_z_reason=2") {
		t.Fatalf("gap/activity text missing or unordered: %s", text)
	}
}

func TestMineDiscoveryCancellationPreservesKnownFacts(t *testing.T) {
	r := mineRequest(t)
	a := mineArtifact()
	mineDo(t, r, mineRunProvider(a))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := mineProvider{list: func(context.Context) ([]sourceRun, []acquisitionGap, error) {
		cancel()
		return []sourceRun{{ID: "discovered", RunAttempt: persistPtr(3)}}, nil, ctx.Err()
	}}
	stats, err := mineWithProvider(ctx, r, p)
	if err != context.Canceled {
		t.Fatalf("discovery cancellation identity lost: %v", err)
	}
	mineCheck(t, r, func(db *sql.DB) {
		if n := mineScalar(t, db, `SELECT count(*) FROM mining_runs WHERE run_id='discovered' AND attempt=3 AND state='incomplete'`); n != 1 {
			t.Fatal("canceled discovery lost known ordinal")
		}
		if n := mineScalar(t, db, `SELECT count(*) FROM mining_runs WHERE run_id='run-1' AND state='complete'`); n != 1 {
			t.Fatal("cleanup changed unrelated completed history")
		}
	})
	if stats.Activity.InspectedRuns != 0 || stats.Activity.InspectedAttempts != 0 {
		t.Fatal("discovered/unstarted run counted as inspected")
	}
}

func TestMineDiscoveryGapFailurePreservesKnownFacts(t *testing.T) {
	r := mineRequest(t)
	if err := os.WriteFile(r.RawDir, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := mineWithProvider(context.Background(), r, mineGapProvider(acquisitionGap{RunID: "failed", UnitKey: "jobs", Attempt: persistPtr(2), Reason: "jobs_unavailable", Raw: []byte("body")}))
	if err == nil {
		t.Fatal("gap raw storage failure acknowledged")
	}
	mineCheck(t, r, func(db *sql.DB) {
		if n := mineScalar(t, db, `SELECT count(*) FROM mining_runs WHERE run_id='failed' AND attempt=2 AND state='incomplete'`); n != 1 {
			t.Fatal("failed discovery gap lost known attempt")
		}
		persistCount(t, db, "acquisition_gaps", 0)
	})
}

func TestMineCanceledCallbacksDoNotPublish(t *testing.T) {
	r := mineRequest(t)
	a := mineArtifact()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := mineRunProvider(a)
	p.acquire = func(_ context.Context, run sourceRun, s acquisitionSink) (runOutcome, error) {
		cancel()
		if err := s.Gap(acquisitionGap{RunID: run.ID, UnitKey: "jobs", Reason: "jobs_unavailable", Raw: []byte("late")}); !errors.Is(err, context.Canceled) {
			t.Errorf("late gap callback error=%v", err)
		}
		if err := s.Artifact(a); !errors.Is(err, context.Canceled) {
			t.Errorf("late artifact callback error=%v", err)
		}
		return runOutcome{State: "complete"}, nil
	}
	stats, err := mineWithProvider(ctx, r, p)
	if err != context.Canceled || stats.RawCases != 0 || stats.Activity.PersistedArtifacts != 0 {
		t.Fatalf("canceled callback persisted: %v %+v", err, stats)
	}
	if _, err = os.Stat(r.RawDir); !os.IsNotExist(err) {
		t.Fatal("canceled callback published raw")
	}
	mineCheck(t, r, func(db *sql.DB) { persistCount(t, db, "acquisition_gaps", 0) })
}

func TestMineBindingTimestampAndGapEnrichment(t *testing.T) {
	r := mineRequest(t)
	stamp := r.Since
	gap := acquisitionGap{RunID: "run", UnitKey: "jobs", Reason: "jobs_unavailable"}
	mineDo(t, r, mineGapProvider(gap))
	var first string
	mineCheck(t, r, func(db *sql.DB) {
		if err := db.QueryRow(`SELECT observed_at FROM mining_sources`).Scan(&first); err != nil {
			t.Fatal(err)
		}
	})
	gap.Attempt = persistPtr(2)
	gap.JobID = persistPtr("job")
	gap.SourceTime = &stamp
	stats := mineDo(t, r, mineGapProvider(gap))
	mineCheck(t, r, func(db *sql.DB) {
		var binding, job, source string
		var attempt int
		if err := db.QueryRow(`SELECT observed_at FROM mining_sources`).Scan(&binding); err != nil {
			t.Fatal(err)
		}
		if binding != first {
			t.Fatal("binding timestamp replaced")
		}
		if err := db.QueryRow(`SELECT job_id,source_time,attempt FROM acquisition_gaps`).Scan(&job, &source, &attempt); err != nil {
			t.Fatal(err)
		}
		if job != "job" || attempt != 2 || source != stamp.Format(observationTimeFormat) {
			t.Fatal("unknown gap facts not enriched")
		}
		persistCount(t, db, "acquisition_gaps", 1)
	})
	if stats.Activity.InspectedAttempts != 0 || stats.Activity.InspectedRuns != 0 || stats.Activity.GapsByReason[gap.Reason] != 1 {
		t.Fatal("gap-only work counted as inspection")
	}
}

func TestMineGapDetailCodesOnly(t *testing.T) {
	r := mineRequest(t)
	mineDo(t, r, mineGapProvider(acquisitionGap{UnitKey: "runs", Reason: "discovery_failed", Detail: "https://example.invalid/private?body=opaque"}))
	mineCheck(t, r, func(db *sql.DB) {
		var detail string
		if err := db.QueryRow(`SELECT detail FROM acquisition_gaps`).Scan(&detail); err != nil {
			t.Fatal(err)
		}
		if detail != "" {
			t.Fatal("non-code source detail persisted")
		}
	})
	for _, reason := range []string{"bad reason", strings.Repeat("x", 81)} {
		if _, err := mineWithProvider(context.Background(), r, mineGapProvider(acquisitionGap{UnitKey: "runs", Reason: reason})); err == nil {
			t.Fatal("invalid gap reason accepted")
		}
	}
}

type mineCancelAfterErrContext struct {
	context.Context
	cancel    context.CancelFunc
	remaining atomic.Int32
	armed     atomic.Bool
}

func (c *mineCancelAfterErrContext) Err() error {
	err := c.Context.Err()
	// Cancel after a truthful snapshot to reproduce arrival just after a check.
	if err == nil && c.armed.Load() && c.remaining.Add(-1) == 0 {
		c.cancel()
	}
	return err
}

func TestMineCancellationAtCheckpointBoundary(t *testing.T) {
	for _, checks := range []int32{3, 4} {
		t.Run(strconv.Itoa(int(checks)), func(t *testing.T) {
			r := mineRequest(t)
			a := mineArtifact()
			p := mineRunProvider(a)
			p.acquire = func(_ context.Context, run sourceRun, s acquisitionSink) (runOutcome, error) {
				err := s.Gap(acquisitionGap{RunID: run.ID, UnitKey: "jobs", Reason: "jobs_unavailable"})
				return runOutcome{State: "incomplete"}, err
			}
			mineDo(t, r, p)
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx := &mineCancelAfterErrContext{Context: parent, cancel: cancel}
			ctx.remaining.Store(checks)
			created := r.Since
			p.list = func(context.Context) ([]sourceRun, []acquisitionGap, error) {
				return []sourceRun{{ID: a.Observation.RunID, RunAttempt: persistPtr(3), SourceHeadSHA: a.Observation.SourceHeadSHA, Branch: a.Observation.Branch, CreatedAt: &created}}, nil, nil
			}
			p.acquire = func(context.Context, sourceRun, acquisitionSink) (runOutcome, error) {
				ctx.armed.Store(true)
				return runOutcome{State: "complete", Attempts: []int{2}}, nil
			}
			stats, err := mineWithProvider(ctx, r, p)
			if parent.Err() != context.Canceled {
				t.Fatal("fixture did not cancel at checkpoint boundary")
			}
			if err != context.Canceled || stats.ActiveGaps != 1 || stats.Activity.IncompleteRuns != 1 {
				t.Errorf("late checkpoint cancellation lost: err=%v active_gaps=%d incomplete=%d", err, stats.ActiveGaps, stats.Activity.IncompleteRuns)
			}
			mineCheck(t, r, func(db *sql.DB) {
				if n := mineScalar(t, db, `SELECT count(*) FROM acquisition_gaps WHERE resolved_at IS NULL`); n != 1 {
					t.Error("canceled checkpoint resolved active gap")
				}
				if n := mineScalar(t, db, `SELECT count(*) FROM mining_runs WHERE state='complete'`); n != 0 {
					t.Error("canceled checkpoint promoted completion")
				}
				if n := mineScalar(t, db, `SELECT count(*) FROM mining_runs WHERE state='incomplete' AND attempt IN (1,2,3)`); n != 3 {
					t.Errorf("canceled checkpoint lost conservative known attempts: %d", n)
				}
				if n := mineScalar(t, db, `SELECT count(*) FROM mining_runs WHERE attempt IN (2,3) AND source_head_sha=? AND branch=? AND created_at=?`, a.Observation.SourceHeadSHA, a.Observation.Branch, created.Format(observationTimeFormat)); n != 2 {
					t.Error("canceled checkpoint lost actual source facts")
				}
			})
		})
	}
}

func TestMineLateCancellationRetainsCleanupFailure(t *testing.T) {
	r := mineRequest(t)
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &mineCancelAfterErrContext{Context: parent, cancel: cancel}
	ctx.remaining.Store(3)
	p := mineRunProvider(mineArtifact())
	p.acquire = func(context.Context, sourceRun, acquisitionSink) (runOutcome, error) {
		mineCheck(t, r, func(db *sql.DB) {
			if _, err := db.Exec(`CREATE TRIGGER fail_late_cleanup BEFORE UPDATE ON mining_runs BEGIN SELECT RAISE(ABORT,'fixture late cleanup failure'); END`); err != nil {
				t.Fatal(err)
			}
		})
		ctx.armed.Store(true)
		return runOutcome{State: "complete", Attempts: []int{1}}, nil
	}
	_, err := mineWithProvider(ctx, r, p)
	if parent.Err() != context.Canceled {
		t.Fatal("fixture did not cancel at checkpoint boundary")
	}
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "fixture late cleanup failure") {
		t.Fatalf("late cancellation or cleanup failure lost: %v", err)
	}
}
