package predict

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fake is an httptest server that records what it receives and answers with a
// scripted handler.
type fake struct {
	srv *httptest.Server

	mu          sync.Mutex
	bodies      [][]byte
	headers     []http.Header
	methods     []string
	paths       []string
	inflight    int
	maxInflight int
}

// handler answers the call-th request (counted from 1) with body.
type handler func(call int, body []byte, w http.ResponseWriter, r *http.Request)

func newFake(t *testing.T, h handler) *fake {
	t.Helper()
	f := &fake{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readAll(t, r)
		f.mu.Lock()
		f.bodies = append(f.bodies, body)
		f.headers = append(f.headers, r.Header.Clone())
		f.methods = append(f.methods, r.Method)
		f.paths = append(f.paths, r.URL.Path)
		call := len(f.bodies)
		f.inflight++
		f.maxInflight = max(f.maxInflight, f.inflight)
		f.mu.Unlock()
		defer func() {
			f.mu.Lock()
			f.inflight--
			f.mu.Unlock()
		}()
		h(call, body, w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func readAll(t *testing.T, r *http.Request) []byte {
	t.Helper()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return []byte(b.String())
}

func (f *fake) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

func (f *fake) received() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.bodies...)
}

// jev returns a client pointed at the fake server.
func (f *fake) jev(t *testing.T, mutate func(*JevOptions)) *Jev {
	t.Helper()
	return newTestJev(t, func(o *JevOptions) {
		o.Config.Endpoint = f.srv.URL + "/api/alpha/decisions"
		if mutate != nil {
			mutate(o)
		}
	})
}

// questionKeys returns the keys of the questions of a request body.
func questionKeys(t *testing.T, body []byte) []string {
	t.Helper()
	return sortedKeys(decodeRequest(t, body).Questions)
}

// testName returns the name of the test behind a question key.
func testName(t *testing.T, body []byte, key string) string {
	t.Helper()
	var d struct{ Name string }
	if err := json.Unmarshal(decodeRequest(t, body).State.Tests[key], &d); err != nil {
		t.Fatalf("descriptor of %s: %v", key, err)
	}
	return d.Name
}

// reply writes a 200 response with the given model, cost and answers.
func reply(w http.ResponseWriter, model string, cost float64, answers map[string]float64) {
	type ans struct {
		Type string  `json:"type"`
		Noul float64 `json:"noul"`
	}
	out := map[string]ans{}
	for k, p := range answers {
		out[k] = ans{"noul", p}
	}
	body, _ := json.Marshal(map[string]any{
		"id": "gen-dec-test", "model": model, "provider": "TypeSafe", "answers": out,
		"usage": map[string]any{"input_tokens": 1000, "output_tokens": 0, "cost": cost},
	})
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

// answerAll answers every question of a request with p(name of its test).
func answerAll(t *testing.T, model string, cost float64, p func(name string) float64) handler {
	return func(_ int, body []byte, w http.ResponseWriter, _ *http.Request) {
		req := decodeRequest(t, body)
		answers := map[string]float64{}
		for key := range req.Questions {
			if key == tierKey {
				continue
			}
			var d struct{ Name string }
			if err := json.Unmarshal(req.State.Tests[key], &d); err != nil {
				t.Errorf("descriptor of %s: %v", key, err)
			}
			answers[key] = p(d.Name)
		}
		reply(w, model, cost, answers)
	}
}

func constant(p float64) func(string) float64 { return func(string) float64 { return p } }

const servedSnapshot = "typesafe/jev-1.13-20260917"

func TestJevTransportAnswersAndHeaders(t *testing.T) {
	f := newFake(t, func(_ int, _ []byte, w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(readFixture(t, "response-ok.json"))
	})
	j := f.jev(t, nil)
	probs, err := j.Predict(t.Context(), goldenChange(), goldenCandidates())
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	if err := ValidateProbs(goldenCandidates(), probs); err != nil {
		t.Fatalf("ValidateProbs: %v", err)
	}
	if probs[0].P != 0.93 || probs[1].P != 0.01 {
		t.Fatalf("probabilities = %v, %v, want 0.93, 0.01", probs[0].P, probs[1].P)
	}
	for i, p := range probs {
		if p.Source != "jev" || p.Shard != 0 || p.Truncated {
			t.Fatalf("prob %d = %+v", i, p)
		}
		if want := 0.000395 / 2; p.CostUSD != want {
			t.Fatalf("prob %d cost = %v, want the even share %v", i, p.CostUSD, want)
		}
	}
	if f.methods[0] != http.MethodPost || f.paths[0] != "/api/alpha/decisions" {
		t.Fatalf("request = %s %s", f.methods[0], f.paths[0])
	}
	if got := f.headers[0].Get("Authorization"); got != "Bearer "+testKey {
		t.Fatalf("Authorization = %q", got)
	}
	if got := f.headers[0].Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	st := j.LastStats()
	if st.Requests != 1 || st.CacheHits != 0 || st.FailedShards != 0 || st.BudgetSkipped != 0 || st.CostUSD != 0.000395 || st.ServedModel != servedSnapshot || st.Tier != "" || len(st.Warnings) != 0 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestJevTransportAnswersInInputOrder(t *testing.T) {
	cands := candidatesN(5)
	shuffled := []int{3, 0, 4, 1, 2}
	input := cands[:0:0]
	for _, i := range shuffled {
		input = append(input, cands[i])
	}
	f := newFake(t, answerAll(t, servedSnapshot, 0.001, func(name string) float64 {
		return float64(name[len(name)-1]-'0') / 10 // Test0003 -> 0.3
	}))
	j := f.jev(t, func(o *JevOptions) { o.Config.QuestionsPerRequest = 2 })
	probs, err := j.Predict(t.Context(), goldenChange(), input)
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	if err := ValidateProbs(input, probs); err != nil {
		t.Fatalf("ValidateProbs: %v", err)
	}
	for i, p := range probs {
		want := float64(input[i].TestName[len(input[i].TestName)-1]-'0') / 10
		if p.P != want {
			t.Fatalf("prob for %s = %v, want %v", p.TestID, p.P, want)
		}
	}
	shardOf := map[string]int{}
	for _, p := range probs {
		shardOf[p.TestID] = p.Shard
	}
	if shardOf["go:m/p:Test0001"] != 0 || shardOf["go:m/p:Test0002"] != 0 || shardOf["go:m/p:Test0003"] != 1 || shardOf["go:m/p:Test0005"] != 2 {
		t.Fatalf("shards = %v, want TestID order two per request", shardOf)
	}
}

func TestJevTransportRetriesTransientStatuses(t *testing.T) {
	for _, status := range []int{429, 500, 502, 503, 524, 529} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			ok := answerAll(t, servedSnapshot, 0.001, constant(0.25))
			f := newFake(t, func(call int, body []byte, w http.ResponseWriter, r *http.Request) {
				if call <= 2 {
					http.Error(w, `{"error":{"code":1,"message":"try again"}}`, status)
					return
				}
				ok(call, body, w, r)
			})
			j := f.jev(t, nil)
			probs, err := j.Predict(t.Context(), goldenChange(), candidatesN(3))
			if err != nil {
				t.Fatalf("Predict: %v", err)
			}
			if f.count() != 3 {
				t.Fatalf("%d requests, want 3 (two failures and the retry that worked)", f.count())
			}
			for _, p := range probs {
				if p.P != 0.25 || p.Source != "jev" {
					t.Fatalf("prob = %+v, want the answer of the third attempt", p)
				}
			}
			bodies := f.received()
			if string(bodies[0]) != string(bodies[1]) || string(bodies[1]) != string(bodies[2]) {
				t.Fatal("a retry must resend identical bytes")
			}
			if st := j.LastStats(); st.Requests != 3 || st.FailedShards != 0 {
				t.Fatalf("stats = %+v", st)
			}
		})
	}
}

func TestJevTransportDoesNotRetryClientErrors(t *testing.T) {
	for _, status := range []int{400, 401, 402, 403, 404, 413} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := newFake(t, func(_ int, _ []byte, w http.ResponseWriter, _ *http.Request) {
				http.Error(w, `{"error":{"code":1,"message":"nope"}}`, status)
			})
			j := f.jev(t, nil)
			probs, err := j.Predict(t.Context(), goldenChange(), candidatesN(3))
			if err != nil {
				t.Fatalf("Predict: %v", err)
			}
			if f.count() != 1 {
				t.Fatalf("%d requests, want 1: a %d is not retried", f.count(), status)
			}
			for _, p := range probs {
				if p.P != 1 || p.Source != "jev_failed" || p.Shard != -1 {
					t.Fatalf("prob = %+v, want 1.0 from jev_failed", p)
				}
			}
			st := j.LastStats()
			if st.FailedShards != 1 || st.Requests != 1 || len(st.Warnings) == 0 {
				t.Fatalf("stats = %+v", st)
			}
			if !strings.Contains(strings.Join(st.Warnings, "\n"), fmt.Sprint(status)) {
				t.Fatalf("warnings %v do not name the status", st.Warnings)
			}
		})
	}
}

func TestJevTransportBackoffDoubles(t *testing.T) {
	f := newFake(t, func(call int, body []byte, w http.ResponseWriter, r *http.Request) {
		if call <= 3 {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		answerAll(t, servedSnapshot, 0.001, constant(0.5))(call, body, w, r)
	})
	j := f.jev(t, func(o *JevOptions) {
		o.Config.Retries = 3
		o.Config.RetryBackoffMS = 10
	})
	var slept []time.Duration
	j.sleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}
	if _, err := j.Predict(t.Context(), goldenChange(), candidatesN(1)); err != nil {
		t.Fatalf("Predict: %v", err)
	}
	want := []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond}
	if fmt.Sprint(slept) != fmt.Sprint(want) {
		t.Fatalf("backoffs = %v, want %v", slept, want)
	}
}

func TestJevTransportGivesUpAfterTheConfiguredRetries(t *testing.T) {
	f := newFake(t, func(_ int, _ []byte, w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "busy", http.StatusServiceUnavailable)
	})
	j := f.jev(t, func(o *JevOptions) { o.Config.Retries = 2 })
	probs, err := j.Predict(t.Context(), goldenChange(), candidatesN(2))
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	if f.count() != 3 {
		t.Fatalf("%d requests, want 3 (one try and two retries)", f.count())
	}
	if probs[0].P != 1 || probs[1].P != 1 || probs[0].Source != "jev_failed" {
		t.Fatalf("probs = %+v", probs)
	}
	j2 := f.jev(t, func(o *JevOptions) { o.Config.Retries = 0 })
	before := f.count()
	if _, err := j2.Predict(t.Context(), goldenChange(), candidatesN(2)); err != nil {
		t.Fatal(err)
	}
	if f.count()-before != 1 {
		t.Fatalf("retries=0 sent %d requests, want 1", f.count()-before)
	}
}

func TestJevTransportRetriesNetworkErrors(t *testing.T) {
	ok := answerAll(t, servedSnapshot, 0.001, constant(0.4))
	f := newFake(t, func(call int, body []byte, w http.ResponseWriter, r *http.Request) {
		if call <= 2 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		ok(call, body, w, r)
	})
	j := f.jev(t, nil)
	probs, err := j.Predict(t.Context(), goldenChange(), candidatesN(2))
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	if f.count() != 3 || probs[0].P != 0.4 {
		t.Fatalf("%d requests, probs %+v, want a retry after each dropped connection", f.count(), probs)
	}
}

func TestJevTransportTimeoutIsRetried(t *testing.T) {
	ok := answerAll(t, servedSnapshot, 0.001, constant(0.6))
	f := newFake(t, func(call int, body []byte, w http.ResponseWriter, r *http.Request) {
		if call == 1 {
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
			return
		}
		ok(call, body, w, r)
	})
	j := f.jev(t, func(o *JevOptions) { o.Config.TimeoutMS = 400 })
	probs, err := j.Predict(t.Context(), goldenChange(), candidatesN(2))
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	if f.count() != 2 || probs[0].P != 0.6 || probs[0].Source != "jev" {
		t.Fatalf("%d requests, probs %+v, want the retry after the timeout to answer", f.count(), probs)
	}
}

func TestJevTransportTimeoutExhaustedSelectsAll(t *testing.T) {
	f := newFake(t, func(_ int, _ []byte, _ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	j := f.jev(t, func(o *JevOptions) {
		o.Config.TimeoutMS = 50
		o.Config.Retries = 1
	})
	probs, err := j.Predict(t.Context(), goldenChange(), candidatesN(2))
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	if probs[0].P != 1 || probs[0].Source != "jev_failed" || j.LastStats().FailedShards != 1 {
		t.Fatalf("probs %+v stats %+v, want a failed shard", probs, j.LastStats())
	}
}

func TestJevTransportConcurrencyLimit(t *testing.T) {
	ok := answerAll(t, servedSnapshot, 0.001, constant(0.1))
	f := newFake(t, func(call int, body []byte, w http.ResponseWriter, r *http.Request) {
		time.Sleep(40 * time.Millisecond)
		ok(call, body, w, r)
	})
	j := f.jev(t, func(o *JevOptions) {
		o.Config.QuestionsPerRequest = 2
		o.Config.Concurrency = 3
	})
	if _, err := j.Predict(t.Context(), goldenChange(), candidatesN(16)); err != nil {
		t.Fatalf("Predict: %v", err)
	}
	if f.count() != 8 {
		t.Fatalf("%d requests, want 8", f.count())
	}
	if f.maxInflight > 3 {
		t.Fatalf("%d requests in flight at once, the limit is 3", f.maxInflight)
	}
	if f.maxInflight < 2 {
		t.Fatalf("at most %d request in flight: shards do not run concurrently", f.maxInflight)
	}
}

// TestJevShardFailureSelectsEveryCandidateOfTheShard is G06: a shard that
// fails after all retries gets P=1.0 for each of its candidates and the other
// shards keep their answers.
func TestJevShardFailureSelectsEveryCandidateOfTheShard(t *testing.T) {
	ok := answerAll(t, servedSnapshot, 0.001, constant(0.1))
	f := newFake(t, func(call int, body []byte, w http.ResponseWriter, r *http.Request) {
		if testName(t, body, "t_001") == "Test0201" { // the second shard
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		ok(call, body, w, r)
	})
	j := f.jev(t, nil)
	cands := candidatesN(450)
	probs, err := j.Predict(t.Context(), goldenChange(), cands)
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	if err := ValidateProbs(cands, probs); err != nil {
		t.Fatal(err)
	}
	for i, p := range probs {
		inFailed := i >= 200 && i < 400
		switch {
		case inFailed && (p.P != 1 || p.Source != "jev_failed"):
			t.Fatalf("candidate %d of the failed shard = %+v, want P=1.0 jev_failed", i, p)
		case !inFailed && (p.P != 0.1 || p.Source != "jev"):
			t.Fatalf("candidate %d of a healthy shard = %+v, want its answer", i, p)
		}
	}
	st := j.LastStats()
	if st.FailedShards != 1 || st.Requests != 5 {
		t.Fatalf("stats = %+v, want 1 failed shard and 5 requests (2 healthy + 3 attempts)", st)
	}
}

func TestJevTransportMissingOrMalformedAnswers(t *testing.T) {
	cases := []struct {
		name    string
		answers string // the "answers" object for t_001 and t_002
	}{
		{"absent", `{}`},
		{"out of range", `{"t_001":{"type":"noul","noul":1.5},"t_002":{"type":"noul","noul":-0.1}}`},
		{"string number", `{"t_001":{"type":"noul","noul":"0.9"},"t_002":{"type":"noul","noul":"x"}}`},
		{"null", `{"t_001":{"type":"noul","noul":null},"t_002":null}`},
		{"no noul field", `{"t_001":{"type":"noul"},"t_002":{"type":"noul","confidence":0.9}}`},
		{"wrong type", `{"t_001":{"type":"choice","noul":0.1},"t_002":{"type":"score","noul":0.1}}`},
		{"not an object", `{"t_001":[0.1],"t_002":0.1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, func(_ int, _ []byte, w http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprintf(w, `{"model":%q,"answers":%s,"usage":{"cost":0.0002}}`, servedSnapshot, tc.answers)
			})
			j := f.jev(t, nil)
			probs, err := j.Predict(t.Context(), goldenChange(), candidatesN(2))
			if err != nil {
				t.Fatalf("Predict: %v", err)
			}
			for _, p := range probs {
				if p.P != 1 || p.Source != "jev_invalid" || p.Shard != 0 {
					t.Fatalf("prob = %+v, want 1.0 from jev_invalid", p)
				}
			}
			if st := j.LastStats(); len(st.Warnings) == 0 || st.FailedShards != 0 {
				t.Fatalf("stats = %+v, want a warning and no failed shard", st)
			}
		})
	}
}

func TestJevTransportPartialFixtureKeepsTheValidAnswer(t *testing.T) {
	f := newFake(t, func(_ int, _ []byte, w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(readFixture(t, "response-partial.json"))
	})
	probs, err := f.jev(t, nil).Predict(t.Context(), goldenChange(), goldenCandidates())
	if err != nil {
		t.Fatal(err)
	}
	if probs[0].P != 1 || probs[0].Source != "jev_invalid" || probs[1].P != 1 || probs[1].Source != "jev_invalid" {
		t.Fatalf("probs = %+v, want both 1.0: one out of range, one missing", probs)
	}
	f2 := newFake(t, func(_ int, _ []byte, w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"model":%q,"answers":{"t_002":{"type":"noul","noul":0.3}},"usage":{"cost":0.0002}}`, servedSnapshot)
	})
	probs, err = f2.jev(t, nil).Predict(t.Context(), goldenChange(), goldenCandidates())
	if err != nil {
		t.Fatal(err)
	}
	if probs[0].P != 1 || probs[0].Source != "jev_invalid" || probs[1].P != 0.3 || probs[1].Source != "jev" {
		t.Fatalf("probs = %+v, want t_001 missing and t_002 answered", probs)
	}
}

func TestJevTransportUnusableBodiesFailTheShard(t *testing.T) {
	for name, body := range map[string]string{
		"not json": "<html>gateway</html>",
		"empty":    "",
		"array":    "[1,2]",
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t, func(_ int, _ []byte, w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) })
			j := f.jev(t, nil)
			probs, err := j.Predict(t.Context(), goldenChange(), candidatesN(2))
			if err != nil {
				t.Fatalf("Predict: %v", err)
			}
			if f.count() != 1 {
				t.Fatalf("%d requests, an unparseable 200 is not retried", f.count())
			}
			if probs[0].P != 1 || probs[0].Source != "jev_failed" || j.LastStats().FailedShards != 1 {
				t.Fatalf("probs %+v stats %+v", probs, j.LastStats())
			}
		})
	}
}

// TestJevModelDriftIsAnError is G02 at the client level.
func TestJevModelDriftIsAnError(t *testing.T) {
	f := newFake(t, func(_ int, _ []byte, w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(readFixture(t, "response-drift.json"))
	})
	j := f.jev(t, nil)
	probs, err := j.Predict(t.Context(), goldenChange(), goldenCandidates())
	if !errors.Is(err, ErrModelDrift) {
		t.Fatalf("Predict error = %v, want ErrModelDrift", err)
	}
	if probs != nil {
		t.Fatalf("probs = %+v, want none with a drift error", probs)
	}
	for _, want := range []string{"typesafe/jev-1.13", "typesafe/jev-1.14-20261001"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %q", err, want)
		}
	}
	if got := j.LastStats().ServedModel; got != "typesafe/jev-1.14-20261001" {
		t.Fatalf("ServedModel = %q, want the drifted snapshot recorded", got)
	}
}

func TestJevModelSnapshotOfTheSameReleaseIsAccepted(t *testing.T) {
	ok := answerAll(t, "typesafe/jev-1.13-20261001", 0.001, constant(0.2))
	f := newFake(t, ok)
	j := f.jev(t, nil)
	probs, err := j.Predict(t.Context(), goldenChange(), candidatesN(2))
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	if probs[0].P != 0.2 || probs[0].Source != "jev" {
		t.Fatalf("probs = %+v", probs)
	}
	if got := j.LastStats().ServedModel; got != "typesafe/jev-1.13-20261001" {
		t.Fatalf("ServedModel = %q, want the new snapshot recorded", got)
	}
}

func TestJevModelReleaseComparison(t *testing.T) {
	cases := []struct {
		served string
		ok     bool
	}{
		{"typesafe/jev-1.13", true},
		{"typesafe/jev-1.13-20260917", true},
		{"typesafe/jev-1.13-20261001", true},
		{"typesafe/jev-1.14-20261001", false},
		{"typesafe/jev-1.130-20261001", false},
		{"typesafe/jev-1.13-beta-20261001", false},
		{"typesafe/jev-1.13-", false},
		{"typesafe/jev-1.13-2026", false},
		{"typesafe/jev-1.1", false},
		{"~typesafe/jev-latest", false},
		{"other/jev-1.13-20260917", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.served, func(t *testing.T) {
			f := newFake(t, answerAll(t, tc.served, 0.001, constant(0.2)))
			_, err := f.jev(t, nil).Predict(t.Context(), goldenChange(), candidatesN(1))
			if tc.ok && err != nil {
				t.Fatalf("served %q rejected: %v", tc.served, err)
			}
			if !tc.ok && !errors.Is(err, ErrModelDrift) {
				t.Fatalf("served %q gave %v, want ErrModelDrift", tc.served, err)
			}
		})
	}
}

func TestJevModelDriftInALaterShardIsStillAnError(t *testing.T) {
	f := newFake(t, func(call int, body []byte, w http.ResponseWriter, r *http.Request) {
		model := servedSnapshot
		if testName(t, body, "t_001") == "Test0003" {
			model = "typesafe/jev-1.14-20261001"
		}
		answerAll(t, model, 0.001, constant(0.2))(call, body, w, r)
	})
	j := f.jev(t, func(o *JevOptions) { o.Config.QuestionsPerRequest = 2 })
	probs, err := j.Predict(t.Context(), goldenChange(), candidatesN(6))
	if !errors.Is(err, ErrModelDrift) || probs != nil {
		t.Fatalf("Predict = %v, %v, want ErrModelDrift and no probabilities", probs, err)
	}
}

func TestJevContextCancelReturnsAnError(t *testing.T) {
	started := make(chan struct{}, 1)
	f := newFake(t, func(_ int, _ []byte, _ http.ResponseWriter, r *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	j := f.jev(t, func(o *JevOptions) { o.Config.TimeoutMS = 4000 })
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		<-started
		cancel()
	}()
	probs, err := j.Predict(ctx, goldenChange(), candidatesN(2))
	if !errors.Is(err, context.Canceled) || probs != nil {
		t.Fatalf("Predict = %v, %v, want context.Canceled", probs, err)
	}
	if f.count() != 1 {
		t.Fatalf("%d requests, a cancelled call must not retry", f.count())
	}
}

func TestJevAPIKeyNeverLeaks(t *testing.T) {
	f := newFake(t, func(_ int, _ []byte, w http.ResponseWriter, r *http.Request) {
		// A misbehaving server that echoes the credential back.
		http.Error(w, "bad key "+r.Header.Get("Authorization"), http.StatusUnauthorized)
	})
	j := f.jev(t, nil)
	probs, err := j.Predict(t.Context(), goldenChange(), candidatesN(2))
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	text := fmt.Sprint(probs, j.LastStats(), err)
	if strings.Contains(text, testKey) {
		t.Fatalf("the key leaks into the results: %s", text)
	}
	for _, body := range f.received() {
		if strings.Contains(string(body), testKey) {
			t.Fatal("the key is in the request body")
		}
	}
	if err := j.LastStats().Warnings; len(err) == 0 {
		t.Fatal("a rejected key must leave a warning")
	}
}

func TestJevRedirectsAreNotFollowed(t *testing.T) {
	other := newFake(t, func(_ int, _ []byte, w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	f := newFake(t, func(_ int, _ []byte, w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.srv.URL+"/stolen", http.StatusTemporaryRedirect)
	})
	j := f.jev(t, nil)
	probs, err := j.Predict(t.Context(), goldenChange(), candidatesN(1))
	if err != nil {
		t.Fatal(err)
	}
	if other.count() != 0 {
		t.Fatalf("the redirect target received %d requests carrying the key", other.count())
	}
	if probs[0].P != 1 || probs[0].Source != "jev_failed" {
		t.Fatalf("probs = %+v", probs)
	}
}

func TestJevPredictFlagsTruncatedDescriptors(t *testing.T) {
	cands := candidatesN(3)
	cands[1].Purpose = strings.Repeat("long purpose text ", 2000)
	f := newFake(t, answerAll(t, servedSnapshot, 0.001, constant(0.2)))
	j := f.jev(t, func(o *JevOptions) { o.Privacy.Redact = "none" })
	probs, err := j.Predict(t.Context(), goldenChange(), cands)
	if err != nil {
		t.Fatal(err)
	}
	if probs[0].Truncated || !probs[1].Truncated || probs[2].Truncated {
		t.Fatalf("truncated flags = %v %v %v, want only the long descriptor", probs[0].Truncated, probs[1].Truncated, probs[2].Truncated)
	}
}

func TestJevPredictWithoutCandidatesSendsNothing(t *testing.T) {
	f := newFake(t, answerAll(t, servedSnapshot, 0.001, constant(0.2)))
	j := f.jev(t, nil)
	probs, err := j.Predict(t.Context(), goldenChange(), nil)
	if err != nil || probs == nil || len(probs) != 0 || f.count() != 0 {
		t.Fatalf("Predict = %v, %v after %d requests, want an empty answer and no request", probs, err, f.count())
	}
	if _, err := j.Predict(t.Context(), nil, candidatesN(1)); err == nil {
		t.Fatal("a nil change set must be an error")
	}
}

func TestJevPredictChangeBlockTooLargeSelectsAll(t *testing.T) {
	f := newFake(t, answerAll(t, servedSnapshot, 0.001, constant(0.2)))
	j := f.jev(t, func(o *JevOptions) { o.Config.MaxStateTokens = 100 })
	probs, err := j.Predict(t.Context(), goldenChange(), candidatesN(3))
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	if f.count() != 0 {
		t.Fatalf("%d requests for a change block that cannot fit", f.count())
	}
	for _, p := range probs {
		if p.P != 1 || p.Source != "jev_failed" {
			t.Fatalf("prob = %+v", p)
		}
	}
	st := j.LastStats()
	if st.FailedShards != 1 || !strings.Contains(strings.Join(st.Warnings, "\n"), "max_state_tokens") {
		t.Fatalf("stats = %+v", st)
	}
}

func TestJevTransportLatencyIsMeasuredWithTheClock(t *testing.T) {
	f := newFake(t, answerAll(t, servedSnapshot, 0.001, constant(0.2)))
	var ticks int
	j := f.jev(t, func(o *JevOptions) {
		o.Now = func() time.Time {
			ticks++
			return fixedNow().Add(time.Duration(ticks) * 7 * time.Millisecond)
		}
	})
	probs, err := j.Predict(t.Context(), goldenChange(), candidatesN(2))
	if err != nil {
		t.Fatal(err)
	}
	if probs[0].LatencyMS != 7 {
		t.Fatalf("latency = %d ms, want 7", probs[0].LatencyMS)
	}
}

func TestJevTierAnswerIsRecorded(t *testing.T) {
	f := newFake(t, func(_ int, _ []byte, w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(readFixture(t, "response-tier.json"))
	})
	j := f.jev(t, func(o *JevOptions) { o.AskTier = true })
	probs, err := j.Predict(t.Context(), goldenChange(), goldenCandidates())
	if err != nil {
		t.Fatal(err)
	}
	if probs[0].P != 0.93 || probs[1].P != 0.01 {
		t.Fatalf("probs = %+v", probs)
	}
	if st := j.LastStats(); st.Tier != "module" || len(st.Warnings) != 0 {
		t.Fatalf("stats = %+v, want tier module", st)
	}
	// 2 tests answered, tier excluded from the cost share: 0.0004 / 2.
	if probs[0].CostUSD != 0.0002 {
		t.Fatalf("cost share = %v, want 0.0002", probs[0].CostUSD)
	}
	for name, answer := range map[string]string{
		"unknown choice": `{"type":"choice","choice":"everything"}`,
		"absent":         ``,
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t, func(_ int, _ []byte, w http.ResponseWriter, _ *http.Request) {
				tier := ""
				if answer != "" {
					tier = `"tier":` + answer + `,`
				}
				_, _ = fmt.Fprintf(w, `{"model":%q,"answers":{%s"t_001":{"type":"noul","noul":0.5},"t_002":{"type":"noul","noul":0.5}},"usage":{"cost":0.0002}}`, servedSnapshot, tier)
			})
			j := f.jev(t, func(o *JevOptions) { o.AskTier = true })
			probs, err := j.Predict(t.Context(), goldenChange(), goldenCandidates())
			if err != nil || probs[0].P != 0.5 {
				t.Fatalf("Predict = %+v, %v", probs, err)
			}
			if st := j.LastStats(); st.Tier != "" || len(st.Warnings) == 0 {
				t.Fatalf("stats = %+v, want no tier and a warning", st)
			}
		})
	}
}

func TestJevStatsAreACopy(t *testing.T) {
	f := newFake(t, func(_ int, _ []byte, w http.ResponseWriter, _ *http.Request) { http.Error(w, "x", 400) })
	j := f.jev(t, nil)
	if _, err := j.Predict(t.Context(), goldenChange(), candidatesN(1)); err != nil {
		t.Fatal(err)
	}
	a := j.LastStats()
	if len(a.Warnings) == 0 {
		t.Fatal("no warning to mutate")
	}
	a.Warnings[0] = "changed"
	if b := j.LastStats(); b.Warnings[0] == "changed" {
		t.Fatal("LastStats shares its warnings with the client")
	}
}
