package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omorsi45/hazard/internal/config"
	"github.com/omorsi45/hazard/internal/control"
	"github.com/omorsi45/hazard/internal/engine"
	"github.com/omorsi45/hazard/internal/impact"
	"github.com/omorsi45/hazard/internal/predict"
)

// scoredDecision is a narrowed decision under a lock with lambda 0.05: goA is
// CERTAIN, goC is a candidate the predictor scored 0.2 (selected), goB one it
// scored 0.01 (not selected), and goA2 a candidate whose shard failed.
func scoredDecision() engine.Decision {
	return engine.Decision{
		ID: "sc1", LockDigest: "sha256:lock", Lambda: 0.05,
		AllTests: []string{goA, goA2, goB, goC},
		L1: &impact.Result{
			Certain: []impact.Member{{TestID: goA, Reason: "direct"}},
			Candidates: []impact.Candidate{
				{TestID: goC, Rank: 0.5, Features: impact.Features{Hops: 1, Reachable: true, CoverageKnown: true, CoverageOverlap: 0.5, CofailureRate: 0.25, CofailureSupport: 3}},
				{TestID: goB, Rank: 0.2, Features: impact.Features{Hops: 2, Reachable: true}},
				{TestID: goA2, Rank: 0.1, Features: impact.Features{Hops: 2, Reachable: true}},
			},
			AllTests:      []string{goA, goA2, goB, goC},
			TestHops:      map[string]int{goA: 0, goB: 2, goC: 1, goA2: 2},
			CoverageState: "fresh",
		},
		Scores: []engine.TestScore{
			{TestID: goA2, P: 1, Source: "jev_failed", Shard: -1},
			{TestID: goB, P: 0.01, Source: "jev", Shard: 1},
			{TestID: goC, P: 0.2, Source: "jev", Shard: 2},
		},
		Tests: []engine.Selected{
			{TestID: goA, Source: "certain", Reason: "direct"},
			{TestID: goA2, Source: "added", Reason: "jev_failed"},
			{TestID: goC, Source: "added", Reason: "predicted"},
		},
	}
}

func TestExplainShowsWhatThePredictorSaid(t *testing.T) {
	chdirRepo(t)
	for _, tc := range []struct {
		name, test string
		want       []string
	}{
		{"a scored candidate that was selected", goC, []string{
			"hops: 1", "coverage overlap: 0.5", "cofailure rate: 0.25",
			"predictor probability: 0.2 (jev)", "shard: 2", "lambda: 0.05", "decision: selected (added, predicted)",
		}},
		{"a scored candidate below lambda", goB, []string{
			"coverage overlap: unknown", "cofailure rate: unknown",
			"predictor probability: 0.01 (jev)", "shard: 1", "lambda: 0.05", "decision: not selected (P 0.01 below lambda 0.05)",
		}},
		{"a candidate whose shard failed", goA2, []string{
			"predictor probability: 1 (jev_failed)", "shard: n/a (no request answered)", "decision: selected (added, jev_failed)",
		}},
		{"a CERTAIN test", goA, []string{
			"coverage overlap: n/a", "cofailure rate: n/a",
			"predictor probability: n/a (a CERTAIN test is not scored)", "lambda: 0.05", "decision: selected (certain, direct)",
		}},
	} {
		f := &fakeSelect{d: scoredDecision()}
		code, out, errOut := runExplainCmd(context.Background(), f.fn, "--test", tc.test)
		if code != 0 || errOut != "" {
			t.Errorf("%s: code=%d err=%q", tc.name, code, errOut)
		}
		for _, w := range tc.want {
			if !strings.Contains(out, w) {
				t.Errorf("%s: output omitted %q:\n%s", tc.name, w, out)
			}
		}
	}
}

func TestExplainTellsWhyTheDecisionIsTheFullSuite(t *testing.T) {
	chdirRepo(t)
	d := scoredDecision()
	d.FullSuite, d.Abstained, d.AbstainReason, d.Tests = true, true, "drift", nil
	f := &fakeSelect{d: d}

	_, out, _ := runExplainCmd(context.Background(), f.fn, "--test", goC)

	for _, w := range []string{"predictor probability: 0.2 (jev)", "lambda: 0.05", "decision: full suite: drift"} {
		if !strings.Contains(out, w) {
			t.Errorf("output omitted %q:\n%s", w, out)
		}
	}
}

func TestExplainLambdaIsNotShownForALockThatDidNotVerify(t *testing.T) {
	chdirRepo(t)
	d := scoredDecision()
	d.LockDigest, d.Lambda = "", 0 // the engine sets these only for a verified lock
	d.FullSuite, d.Abstained, d.AbstainReason, d.Tests, d.Scores = true, true, "lock_corrupt", nil, nil
	f := &fakeSelect{d: d}

	_, out, _ := runExplainCmd(context.Background(), f.fn, "--test", goC)

	for _, w := range []string{"lambda: n/a (no verified lock)", "predictor probability: n/a (the predictor was not asked)", "decision: full suite: lock_corrupt"} {
		if !strings.Contains(out, w) {
			t.Errorf("output omitted %q:\n%s", w, out)
		}
	}
}

func TestExplainJSONCarriesTheSameFields(t *testing.T) {
	chdirRepo(t)
	f := &fakeSelect{d: scoredDecision()}

	code, out, errOut := runExplainCmd(context.Background(), f.fn, "--test", goC, "--json")

	if code != 0 || errOut != "" {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
	var doc struct {
		Hops                 *int     `json:"hops"`
		CoverageOverlap      *float64 `json:"coverage_overlap"`
		CofailureRate        *float64 `json:"cofailure_rate"`
		PredictorProbability string   `json:"predictor_probability"`
		Shard                string   `json:"shard"`
		Lambda               string   `json:"lambda"`
		Decision             string   `json:"decision"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%q", err, out)
	}
	if doc.Hops == nil || *doc.Hops != 1 || doc.CoverageOverlap == nil || *doc.CoverageOverlap != 0.5 || doc.CofailureRate == nil || *doc.CofailureRate != 0.25 ||
		doc.PredictorProbability != "0.2 (jev)" || doc.Shard != "2" || doc.Lambda != "0.05" || doc.Decision != "selected (added, predicted)" {
		t.Errorf("doc = %+v", doc)
	}

	_, out, _ = runExplainCmd(context.Background(), (&fakeSelect{d: scoredDecision()}).fn, "--test", goA, "--json")
	if !strings.Contains(out, `"coverage_overlap":null`) || !strings.Contains(out, `"cofailure_rate":null`) {
		t.Errorf("a test without features must report null, not zero: %s", out)
	}
}

// ---- end to end: a real lock, a real predictor, the real engine ----

// writeE2ELock gives the repository of e2eRepo a logistic artifact that scores
// an unreachable test sigmoid(-6) and a lock for it with lambda 0.05, built
// with the helpers the engine itself uses.
func writeE2ELock(t *testing.T, dir string) control.Lock {
	t.Helper()
	d := len(impact.Features{}.Vector())
	means, scales, weights := make([]float64, d), make([]float64, d), make([]float64, d)
	for i := range scales {
		scales[i] = 1
	}
	weights[1] = 4 // the "reachable" feature
	art := predict.LogisticArtifact{
		Schema: predict.ArtifactSchema, FeatureVersion: impact.FeatureVersion,
		Model: predict.LogisticModel{FeatureVersion: impact.FeatureVersion, Means: means, Scales: scales, Weights: weights, Bias: -6, PlattA: 1, Converged: true},
	}
	path := filepath.Join(dir, ".hazard", "logistic.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := predict.WriteLogistic(path, art); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(filepath.Join(dir, "hazard.yml"))
	if err != nil {
		t.Fatal(err)
	}
	_, id, err := engine.LoadPredictor(dir, cfg, "logistic")
	if err != nil {
		t.Fatal(err)
	}
	lock := control.Lock{
		Version: 1, CreatedAt: "2026-10-29T00:00:00Z", Alpha: 0.01, Delta: 0.05, Unit: "commit", Lambda: 0.05,
		Predictor: "logistic", ModelVersion: id.ModelRelease, RubricVersion: id.RubricVersion, FeatureVersion: id.FeatureVersion,
		WeightsDigest: id.WeightsDigest, PolicyDigest: engine.BuildPolicy(cfg, "logistic", impact.ModeGraph).Digest(),
		L1Mode: control.L1ModeGraph, NCal: 400, EmpiricalMissRate: 0.002, Split: "temporal", SplitAt: "2026-06-01",
	}
	if err := control.WriteLock(filepath.Join(dir, "hazard.lock"), lock); err != nil {
		t.Fatal(err)
	}
	read, err := control.ReadLock(filepath.Join(dir, "hazard.lock"))
	if err != nil {
		t.Fatal(err)
	}
	return read
}

func TestExplainAndSelectEndToEndUnderALock(t *testing.T) {
	dir := e2eRepo(t)
	lock := writeE2ELock(t, dir)
	ctx := context.Background()
	const solo = "go:example.com/m/solo:TestSolo"

	t.Run("select narrows to the CERTAIN tests", func(t *testing.T) {
		code, out, errOut := invoke(ctx, "select")
		want := "selected 2 of 3 tests\n" + e2eCore + "\n" + e2eLeft + "\n"
		if code != 0 || out != want {
			t.Errorf("code=%d out=%q err=%q, want out %q", code, out, errOut, want)
		}
		code, out, _ = invoke(ctx, "select", "--json")
		var doc struct {
			FullSuite  bool    `json:"full_suite"`
			LockDigest string  `json:"lock_digest"`
			Lambda     float64 `json:"lambda"`
		}
		if err := json.Unmarshal([]byte(out), &doc); err != nil || code != 0 || doc.FullSuite || doc.LockDigest != lock.Digest || doc.Lambda != 0.05 {
			t.Errorf("code=%d doc=%+v (%v)", code, doc, err)
		}
	})
	t.Run("explain a candidate that was not selected", func(t *testing.T) {
		code, out, errOut := invoke(ctx, "explain", "--test", solo)
		if code != 0 || errOut != "" {
			t.Fatalf("code=%d err=%q", code, errOut)
		}
		for _, w := range []string{
			"hops: unreachable", "l1: candidate",
			"predictor probability: 0.00247262 (logistic)",
			"shard: n/a (no request answered)", "lambda: 0.05", "decision: not selected (P 0.00247262 below lambda 0.05)",
		} {
			if !strings.Contains(out, w) {
				t.Errorf("output omitted %q:\n%s", w, out)
			}
		}
	})
	t.Run("explain a CERTAIN test", func(t *testing.T) {
		_, out, _ := invoke(ctx, "explain", "--test", e2eLeft)
		for _, w := range []string{"l1: certain (graph)", "predictor probability: n/a (a CERTAIN test is not scored)", "lambda: 0.05", "decision: selected (certain, graph)"} {
			if !strings.Contains(out, w) {
				t.Errorf("output omitted %q:\n%s", w, out)
			}
		}
	})
	t.Run("a recall override is refused", func(t *testing.T) {
		code, out, errOut := invoke(ctx, "select", "--recall", "0.9", "--i-know-what-im-doing")
		if code != 2 || out != "" || !strings.Contains(errOut, "recall override is not supported without recalibration") {
			t.Errorf("code=%d out=%q err=%q", code, out, errOut)
		}
	})
}
