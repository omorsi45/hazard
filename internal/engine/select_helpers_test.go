package engine

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/omorsi45/hazard/internal/config"
	"github.com/omorsi45/hazard/internal/control"
	"github.com/omorsi45/hazard/internal/predict"
)

// lockedYML limits the graph to one hop, so editing core makes core, left and
// right CERTAIN and leaves top (two hops) and solo (unrelated) as candidates.
const lockedYML = "version: 1\nlanguages:\n  python:\n    enabled: false\nimpact:\n  max_hops: 1\n"

// lockLambda is the threshold of the locks the tests write.
const lockLambda = 0.05

var certainIDs = []string{idCore, idLeft, idRght}

// lockedFixture commits the diamond module with the given configuration (and
// extra files), edits core in the working tree and builds the index. The
// configuration must be final: the index records its digest, so changing
// hazard.yml afterwards makes the index stale.
func lockedFixture(t *testing.T, yml string, extra map[string]string) *fixture {
	t.Helper()
	files := map[string]string{"hazard.yml": yml}
	for k, v := range extra {
		files[k] = v
	}
	f := newFixture(t, files)
	f.write(t, map[string]string{"core/core.go": coreEdited})
	f.indexNow(t, false)
	return f
}

// extraFiles are independent packages, each with one test named TestE1, TestE2...
func extraFiles(names ...string) map[string]string {
	out := map[string]string{}
	for _, n := range names {
		out[n+"/"+n+".go"] = "package " + n + "\n\nfunc F() int { return 1 }\n"
		out[n+"/"+n+"_test.go"] = "package " + n + "\n\nimport \"testing\"\n\nfunc Test" + strings.ToUpper(n) + "(t *testing.T) {\n\tif F() != 1 {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n"
	}
	return out
}

func extraID(n string) string {
	return "go:example.com/m/" + n + ":Test" + strings.ToUpper(n)
}

func (f *fixture) config(t *testing.T) config.Config {
	t.Helper()
	cfg, err := config.Load(filepath.Join(f.dir, "hazard.yml"))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func (f *fixture) lockPath() string { return filepath.Join(f.dir, "hazard.lock") }

// writeLock writes the lock the engine would verify for kind: the identity of
// the predictor LoadPredictor builds and the digest of BuildPolicy. mutate
// changes the lock before it is written, so a test can make exactly one field
// disagree with the running system.
func (f *fixture) writeLock(t *testing.T, kind string, mutate func(*control.Lock)) control.Lock {
	t.Helper()
	cfg := f.config(t)
	_, id, err := LoadPredictor(f.dir, cfg, kind)
	if err != nil {
		t.Fatalf("LoadPredictor(%s): %v", kind, err)
	}
	l := control.Lock{
		Version: 1, CreatedAt: "2026-10-29T00:00:00Z", Alpha: 0.01, Delta: 0.05, Unit: "commit", Lambda: lockLambda,
		Predictor: kind, ModelVersion: id.ModelRelease, RubricVersion: id.RubricVersion,
		FeatureVersion: id.FeatureVersion, WeightsDigest: id.WeightsDigest, PolicyDigest: BuildPolicy(cfg, kind).Digest(),
		NCal: 400, EmpiricalMissRate: 0.002, Split: "temporal", SplitAt: "2026-06-01",
	}
	if mutate != nil {
		mutate(&l)
	}
	if err := control.WriteLock(f.lockPath(), l); err != nil {
		t.Fatal(err)
	}
	read, err := control.ReadLock(f.lockPath())
	if err != nil {
		t.Fatal(err)
	}
	return read
}

// logistic writes the logistic artifact at its configured path: a reachable
// test scores sigmoid(weight+bias), an unreachable one sigmoid(bias).
func (f *fixture) logistic(t *testing.T, weight, bias float64) predict.LogisticArtifact {
	t.Helper()
	return writeLogisticArtifact(t, filepath.Join(f.dir, ".hazard", "logistic.json"), weight, bias)
}

// The default artifact scores top (reachable, three hops from core) at
// sigmoid(-2), about 0.119, above lockLambda, and every unreachable test at
// sigmoid(-6), about 0.0025, below it.
const (
	defaultWeight = 4.0
	defaultBias   = -6.0
)

// logisticFixture is a fixture with the default logistic artifact and a lock for it.
func logisticFixture(t *testing.T, yml string, extra map[string]string) (*fixture, control.Lock) {
	t.Helper()
	f := lockedFixture(t, yml, extra)
	f.logistic(t, defaultWeight, defaultBias)
	return f, f.writeLock(t, "logistic", nil)
}

// stubLoader returns a loader that builds the real identity of kind but
// answers with p, so a test controls the probabilities and sees the
// controller's reaction.
func stubLoader(p predict.Predictor) func(string, config.Config, string) (predict.Predictor, control.Identity, error) {
	return func(repoDir string, cfg config.Config, kind string) (predict.Predictor, control.Identity, error) {
		_, id, err := LoadPredictor(repoDir, cfg, kind)
		return p, id, err
	}
}

// failingLoader fails the test if the engine tries to build a predictor.
func failingLoader(t *testing.T) func(string, config.Config, string) (predict.Predictor, control.Identity, error) {
	return func(string, config.Config, string) (predict.Predictor, control.Identity, error) {
		t.Error("the engine built a predictor")
		return nil, control.Identity{}, ErrPredictorUnavailable
	}
}

// fixedP answers every candidate with p.
func fixedP(p float64) *stubPredictor {
	return &stubPredictor{fn: constantP(p, "stub")}
}

func writeAuditsFile(t *testing.T, f *fixture, lines ...string) {
	t.Helper()
	writeAudits(t, f.dir, strings.Join(lines, "\n")+"\n")
}

// jevServer is a TLS server that stands in for the Decisions API. It answers
// every question with the noul P gives for the test (0 when P is nil), except
// a request that asks about the test named failName (any test when it is
// "*"), which gets a 503. It counts the requests it receives.
type jevServer struct {
	*httptest.Server
	requests atomic.Int32
	P        func(testName string) float64 // set before the first request
	mu       sync.Mutex
	seen     [][]byte
}

// bodies are the request bodies received so far.
func (s *jevServer) bodies() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]byte(nil), s.seen...)
}

func newJevServer(t *testing.T, failName string) *jevServer {
	t.Helper()
	s := &jevServer{}
	s.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.seen = append(s.seen, body)
		s.mu.Unlock()
		var req struct {
			State struct {
				Tests map[string]struct {
					Name string `json:"name"`
				} `json:"tests"`
			} `json:"state"`
			Questions map[string]json.RawMessage `json:"questions"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		for _, test := range req.State.Tests {
			if failName == "*" || test.Name == failName {
				http.Error(w, "busy", http.StatusServiceUnavailable)
				return
			}
		}
		answers := map[string]any{}
		for key, test := range req.State.Tests {
			p := 0.0
			if s.P != nil {
				p = s.P(test.Name)
			}
			answers[key] = map[string]any{"type": "noul", "noul": p}
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"model": "typesafe/jev-1.13-20260601", "answers": answers, "usage": map[string]any{"cost": 0.0004},
		}); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

// jevYML is the configuration of a fixture whose Jev endpoint is srv, with one
// question per request so that every candidate has a shard of its own.
func jevYML(srv *jevServer, extraYAML string) string {
	return lockedYML + "predictor:\n  jev:\n    endpoint: " + srv.URL + "\n    questions_per_request: 1\n    retries: 1\n    retry_backoff_ms: 1\n    timeout_ms: 5000\n" + extraYAML
}

// clientLoader builds the real predictors with client as the Jev HTTP client.
func clientLoader(client *http.Client) func(string, config.Config, string) (predict.Predictor, control.Identity, error) {
	return func(repoDir string, cfg config.Config, kind string) (predict.Predictor, control.Identity, error) {
		return loadPredictor(repoDir, cfg, kind, client)
	}
}

// generousBudgetMS keeps a slow or busy host from spending the default five
// second budget before the predictor runs; the tests of the budget set their own.
const generousBudgetMS = 120000

func selectWith(t *testing.T, f *fixture, mutate func(*Options)) Decision {
	t.Helper()
	opts := Options{RepoDir: f.dir, BudgetMS: generousBudgetMS}
	if mutate != nil {
		mutate(&opts)
	}
	return selectOK(t, opts)
}

func hasWarning(d Decision, substr string) bool {
	for _, w := range d.Warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

func byID(sel []Selected, id string) (Selected, bool) {
	for _, s := range sel {
		if s.TestID == id {
			return s, true
		}
	}
	return Selected{}, false
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
