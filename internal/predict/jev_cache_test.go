package predict

import (
	"database/sql"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/omorsi45/hazard/internal/change"
	"github.com/omorsi45/hazard/internal/config"
	"github.com/omorsi45/hazard/internal/impact"
	"github.com/omorsi45/hazard/internal/store"
)

func openCacheDB(t *testing.T) *sql.DB {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "hazard.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s.DB()
}

func rowCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM judgments").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// cached returns a client over the database with the standard fake answers.
func cachedJev(t *testing.T, f *fake, db *sql.DB, mutate func(*JevOptions)) *Jev {
	t.Helper()
	return f.jev(t, func(o *JevOptions) {
		o.Cache = NewCache(db)
		if mutate != nil {
			mutate(o)
		}
	})
}

func nameBasedAnswer(name string) float64 {
	return float64(len(name)%7) / 10
}

func TestJevCacheSecondCallMakesNoRequest(t *testing.T) {
	f := newFake(t, answerAll(t, servedSnapshot, 0.0004, nameBasedAnswer))
	db := openCacheDB(t)
	j := cachedJev(t, f, db, nil)
	cands := candidatesN(450)

	first, err := j.Predict(t.Context(), goldenChange(), cands)
	if err != nil {
		t.Fatal(err)
	}
	if f.count() != 3 || j.LastStats().CacheHits != 0 {
		t.Fatalf("first call: %d requests, stats %+v", f.count(), j.LastStats())
	}
	if n := rowCount(t, db); n != 450 {
		t.Fatalf("%d rows cached, want 450", n)
	}

	second, err := j.Predict(t.Context(), goldenChange(), cands)
	if err != nil {
		t.Fatal(err)
	}
	if f.count() != 3 {
		t.Fatalf("the second call sent %d more requests, want none", f.count()-3)
	}
	st := j.LastStats()
	if st.CacheHits != 450 || st.Requests != 0 || st.CostUSD != 0 || st.FailedShards != 0 {
		t.Fatalf("stats = %+v", st)
	}
	if st.ServedModel != servedSnapshot {
		t.Fatalf("ServedModel = %q, want the snapshot recorded with the judgments", st.ServedModel)
	}
	for i, p := range second {
		if p.P != first[i].P || p.Source != "cache" || p.Shard != -1 || p.CostUSD != 0 || p.LatencyMS != 0 {
			t.Fatalf("cached prob %d = %+v, first answer %+v", i, p, first[i])
		}
	}
}

func TestJevCacheRowRecordsTheCall(t *testing.T) {
	f := newFake(t, answerAll(t, servedSnapshot, 0.0004, constant(0.75)))
	db := openCacheDB(t)
	j := cachedJev(t, f, db, func(o *JevOptions) { o.RubricVersion = "r1s" })
	if _, err := j.Predict(t.Context(), goldenChange(), goldenCandidates()); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query("SELECT cache_key, test_id, p_break, predictor, model_version, rubric_version, latency_ms, cost_usd, created_at FROM judgments ORDER BY test_id")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	hexKey := regexp.MustCompile(`^[0-9a-f]{64}$`)
	keys := map[string]bool{}
	n := 0
	for rows.Next() {
		var key, id, predictor, model, rubric, created string
		var p, cost float64
		var latency int
		if err := rows.Scan(&key, &id, &p, &predictor, &model, &rubric, &latency, &cost, &created); err != nil {
			t.Fatal(err)
		}
		if !hexKey.MatchString(key) || keys[key] {
			t.Fatalf("cache key %q is not a distinct sha256", key)
		}
		keys[key] = true
		if p != 0.75 || predictor != "jev" || model != servedSnapshot || rubric != "r1s" || latency != 0 || cost != 0.0002 || created != "2026-10-28T12:00:00Z" {
			t.Fatalf("row %s = p %v predictor %s model %s rubric %s latency %d cost %v created %s", id, p, predictor, model, rubric, latency, cost, created)
		}
		if !strings.HasPrefix(id, "go:github.com/omorsi45/x/internal/") {
			t.Fatalf("test_id = %q", id)
		}
		n++
	}
	if err := rows.Err(); err != nil || n != 2 {
		t.Fatalf("%d rows, %v", n, err)
	}
}

func TestJevCacheChangedDescriptorMissesOnlyThatTest(t *testing.T) {
	edits := map[string]func(*impact.Candidate){
		"name":    func(c *impact.Candidate) { c.TestName += "Again" },
		"package": func(c *impact.Candidate) { c.Package = "m/other" },
		"hops":    func(c *impact.Candidate) { c.Features.Hops = 3 },
		"coverage": func(c *impact.Candidate) {
			c.Features = impact.Features{Reachable: true, Hops: 1, CoverageKnown: true, CoverageOverlap: 1, LinesAdded: 34, LinesRemoved: 12}
		},
	}
	for name, edit := range edits {
		t.Run(name, func(t *testing.T) {
			f := newFake(t, answerAll(t, servedSnapshot, 0.0004, constant(0.3)))
			db := openCacheDB(t)
			j := cachedJev(t, f, db, nil)
			cands := candidatesN(5)
			if _, err := j.Predict(t.Context(), goldenChange(), cands); err != nil {
				t.Fatal(err)
			}
			before := f.count()

			changed := append([]impact.Candidate(nil), cands...)
			edit(&changed[2])
			probs, err := j.Predict(t.Context(), goldenChange(), changed)
			if err != nil {
				t.Fatal(err)
			}
			if f.count()-before != 1 {
				t.Fatalf("%d requests after editing one descriptor, want 1", f.count()-before)
			}
			last := f.received()[f.count()-1]
			if keys := questionKeys(t, last); len(keys) != 1 || keys[0] != "t_001" {
				t.Fatalf("the request asks %v, want only the changed test", keys)
			}
			for i, p := range probs {
				want := "cache"
				if i == 2 {
					want = "jev"
				}
				if p.Source != want {
					t.Fatalf("prob %d source = %q, want %q", i, p.Source, want)
				}
			}
			if st := j.LastStats(); st.CacheHits != 4 || st.Requests != 1 {
				t.Fatalf("stats = %+v", st)
			}
		})
	}
}

func TestJevCacheRebasedChangeWithTheSameBlockHits(t *testing.T) {
	f := newFake(t, answerAll(t, servedSnapshot, 0.0004, constant(0.3)))
	db := openCacheDB(t)
	j := cachedJev(t, f, db, nil)
	if _, err := j.Predict(t.Context(), goldenChange(), goldenCandidates()); err != nil {
		t.Fatal(err)
	}
	before := f.count()

	rebased := goldenChange()
	rebased.BaseSHA, rebased.HeadSHA = "ffffffff00001111", "0000aaaa22223333"
	rebased.Dirty = true
	rebased.TriggersFull = []string{"go.mod:go.mod"}
	rebased.Files[0].Hunks[0].OldStart, rebased.Files[0].Hunks[0].NewStart = 90, 92
	rebased.Files[0].OldPath = "internal/auth/old.go"
	probs, err := j.Predict(t.Context(), rebased, goldenCandidates())
	if err != nil {
		t.Fatal(err)
	}
	if f.count() != before {
		t.Fatalf("a rebased change sent %d requests, want none", f.count()-before)
	}
	for _, p := range probs {
		if p.Source != "cache" {
			t.Fatalf("prob = %+v, want a cache hit", p)
		}
	}
}

func TestJevCacheChangedBlockMissesEverything(t *testing.T) {
	edits := map[string]func(*change.ChangeSet){
		"lines added": func(c *change.ChangeSet) { c.LinesAdded++ },
		"file added": func(c *change.ChangeSet) {
			c.Files = append(c.Files, change.ChangedFile{Path: "z.go", Status: "A", Language: "go"})
		},
		"symbol":       func(c *change.ChangeSet) { c.Files[0].Hunks[0].Symbols = []string{"Other"} },
		"hunk summary": func(c *change.ChangeSet) { c.Files[1].Hunks[0].Text = "-a: 1\n+a: 1 # note\n" },
	}
	for name, edit := range edits {
		t.Run(name, func(t *testing.T) {
			f := newFake(t, answerAll(t, servedSnapshot, 0.0004, constant(0.3)))
			db := openCacheDB(t)
			j := cachedJev(t, f, db, nil)
			if _, err := j.Predict(t.Context(), goldenChange(), goldenCandidates()); err != nil {
				t.Fatal(err)
			}
			before := f.count()
			other := goldenChange()
			edit(other)
			probs, err := j.Predict(t.Context(), other, goldenCandidates())
			if err != nil {
				t.Fatal(err)
			}
			if f.count()-before != 1 || probs[0].Source != "jev" || probs[1].Source != "jev" {
				t.Fatalf("%d requests, probs %+v: a different change block must be asked again", f.count()-before, probs)
			}
		})
	}
}

func TestJevCacheKeyCoversReleaseRubricAndWhatIsSent(t *testing.T) {
	f := newFake(t, func(call int, body []byte, w http.ResponseWriter, r *http.Request) {
		model := decodeRequest(t, body).Model
		answerAll(t, model+"-20261001", 0.0004, constant(0.3))(call, body, w, r)
	})
	db := openCacheDB(t)
	base := cachedJev(t, f, db, nil)
	if _, err := base.Predict(t.Context(), goldenChange(), goldenCandidates()); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*JevOptions){
		"release": func(o *JevOptions) { o.Config.Model = "typesafe/jev-1.12" },
		"rubric":  func(o *JevOptions) { o.RubricVersion = "r1s" },
		"privacy": func(o *JevOptions) {
			o.Privacy = config.Privacy{Redact: "none", SendSymbolNames: true, SendPaths: true, AllowExternalPredictor: true}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			before := f.count()
			j := cachedJev(t, f, db, mutate)
			probs, err := j.Predict(t.Context(), goldenChange(), goldenCandidates())
			if err != nil {
				t.Fatal(err)
			}
			if f.count()-before != 1 || probs[0].Source != "jev" {
				t.Fatalf("%d requests, prob %+v: a different %s must not hit", f.count()-before, probs[0], name)
			}
		})
	}
	// The original configuration still hits what it stored.
	before := f.count()
	probs, err := base.Predict(t.Context(), goldenChange(), goldenCandidates())
	if err != nil || f.count() != before || probs[0].Source != "cache" {
		t.Fatalf("original configuration: %d new requests, %+v, %v", f.count()-before, probs, err)
	}
}

func TestJevCacheAsksOnlyTheMissesAndNumbersThemAgain(t *testing.T) {
	f := newFake(t, answerAll(t, servedSnapshot, 0.0004, constant(0.3)))
	db := openCacheDB(t)
	j := cachedJev(t, f, db, nil)
	if _, err := j.Predict(t.Context(), goldenChange(), candidatesN(250)); err != nil {
		t.Fatal(err)
	}
	before := f.count()
	probs, err := j.Predict(t.Context(), goldenChange(), candidatesN(255))
	if err != nil {
		t.Fatal(err)
	}
	if f.count()-before != 1 {
		t.Fatalf("%d requests for 5 new tests, want 1", f.count()-before)
	}
	last := f.received()[f.count()-1]
	if keys := questionKeys(t, last); len(keys) != 5 || keys[0] != "t_001" || keys[4] != "t_005" {
		t.Fatalf("keys = %v, want t_001 to t_005", keys)
	}
	for i, p := range probs {
		want := "cache"
		if i >= 250 {
			want = "jev"
		}
		if p.Source != want {
			t.Fatalf("prob %d source = %q, want %q", i, p.Source, want)
		}
	}
}

func TestJevCacheKeepsTheTruncatedFlag(t *testing.T) {
	cands := candidatesN(2)
	cands[1].Purpose = strings.Repeat("long purpose text ", 2000)
	f := newFake(t, answerAll(t, servedSnapshot, 0.0004, constant(0.3)))
	db := openCacheDB(t)
	j := cachedJev(t, f, db, func(o *JevOptions) { o.Privacy.Redact = "none" })
	if _, err := j.Predict(t.Context(), goldenChange(), cands); err != nil {
		t.Fatal(err)
	}
	probs, err := j.Predict(t.Context(), goldenChange(), cands)
	if err != nil {
		t.Fatal(err)
	}
	if probs[0].Source != "cache" || probs[1].Source != "cache" || probs[0].Truncated || !probs[1].Truncated {
		t.Fatalf("probs = %+v, want cache hits with the truncation of the second", probs)
	}
}

func TestJevCacheDoesNotStoreWhatIsNotAnAnswer(t *testing.T) {
	t.Run("failed shard", func(t *testing.T) {
		f := newFake(t, func(_ int, _ []byte, w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "x", http.StatusServiceUnavailable)
		})
		db := openCacheDB(t)
		if _, err := cachedJev(t, f, db, nil).Predict(t.Context(), goldenChange(), candidatesN(3)); err != nil {
			t.Fatal(err)
		}
		if n := rowCount(t, db); n != 0 {
			t.Fatalf("%d rows stored for a failed shard", n)
		}
	})
	t.Run("invalid answer", func(t *testing.T) {
		f := newFake(t, func(_ int, _ []byte, w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"model":"` + servedSnapshot + `","answers":{"t_002":{"type":"noul","noul":0.3},"t_001":{"type":"noul","noul":7}},"usage":{"cost":0.0001}}`))
		})
		db := openCacheDB(t)
		if _, err := cachedJev(t, f, db, nil).Predict(t.Context(), goldenChange(), goldenCandidates()); err != nil {
			t.Fatal(err)
		}
		if n := rowCount(t, db); n != 1 {
			t.Fatalf("%d rows stored, want only the valid answer", n)
		}
	})
	t.Run("budget", func(t *testing.T) {
		f := newFake(t, answerAll(t, servedSnapshot, 0.0004, constant(0.3)))
		db := openCacheDB(t)
		j := cachedJev(t, f, db, func(o *JevOptions) { o.Config.BudgetUSDPerRun = 1e-12 })
		if _, err := j.Predict(t.Context(), goldenChange(), candidatesN(3)); err != nil {
			t.Fatal(err)
		}
		if n := rowCount(t, db); n != 0 {
			t.Fatalf("%d rows stored for a skipped shard", n)
		}
	})
	t.Run("drift", func(t *testing.T) {
		f := newFake(t, func(call int, body []byte, w http.ResponseWriter, r *http.Request) {
			model := servedSnapshot
			if testName(t, body, "t_001") == "Test0003" {
				model = "typesafe/jev-1.14-20261001"
			}
			answerAll(t, model, 0.0001, constant(0.3))(call, body, w, r)
		})
		db := openCacheDB(t)
		j := cachedJev(t, f, db, func(o *JevOptions) { o.Config.QuestionsPerRequest = 2 })
		if _, err := j.Predict(t.Context(), goldenChange(), candidatesN(4)); err == nil {
			t.Fatal("no drift error")
		}
		if n := rowCount(t, db); n != 0 {
			t.Fatalf("%d rows stored by a call that drifted", n)
		}
	})
}

func TestJevCacheUnavailableIsAWarningNotAnError(t *testing.T) {
	f := newFake(t, answerAll(t, servedSnapshot, 0.0004, constant(0.3)))
	db := openCacheDB(t)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	j := cachedJev(t, f, db, nil)
	probs, err := j.Predict(t.Context(), goldenChange(), candidatesN(3))
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	if f.count() != 1 || probs[0].Source != "jev" || probs[0].P != 0.3 {
		t.Fatalf("%d requests, probs %+v: the answers must come from the network", f.count(), probs)
	}
	st := j.LastStats()
	if len(st.Warnings) < 1 || !strings.Contains(strings.Join(st.Warnings, "\n"), "cache") {
		t.Fatalf("warnings = %v, want the cache failure reported", st.Warnings)
	}
	if len(st.Warnings) > 2 {
		t.Fatalf("%d warnings: a broken cache is reported once for reading and once for writing", len(st.Warnings))
	}

	empty := f.jev(t, func(o *JevOptions) { o.Cache = NewCache(nil) })
	if _, err := empty.Predict(t.Context(), goldenChange(), candidatesN(3)); err != nil {
		t.Fatalf("Predict with a cache without a database: %v", err)
	}
}

func TestJevCachePayloadStillShowsEverything(t *testing.T) {
	f := newFake(t, answerAll(t, servedSnapshot, 0.0004, constant(0.3)))
	db := openCacheDB(t)
	j := cachedJev(t, f, db, nil)
	empty, err := j.Payload(goldenChange(), goldenCandidates())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.Predict(t.Context(), goldenChange(), goldenCandidates()); err != nil {
		t.Fatal(err)
	}
	after, err := j.Payload(goldenChange(), goldenCandidates())
	if err != nil || len(after) != 1 || string(after[0]) != string(empty[0]) {
		t.Fatalf("Payload changed once the cache was filled: %d shards, %v", len(after), err)
	}
}
