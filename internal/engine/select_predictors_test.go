package engine

import (
	"math"
	"path/filepath"
	"testing"

	"github.com/omorsi45/hazard/internal/control"
)

func TestSelectNarrowsWithAStaticLock(t *testing.T) {
	t.Parallel()
	f := lockedFixture(t, lockedYML, nil)
	writeStaticArtifact(t, filepath.Join(f.dir, ".hazard", "static.json"), 0.2, 0.2)
	lock := f.writeLock(t, "static", func(l *control.Lock) { l.Lambda = 0.5 })

	d := selectWith(t, f, nil)

	requireNarrowed(t, d, "a static lock")
	if !sameStrings(ids(d.Tests), certainIDs) {
		t.Errorf("Tests = %v, want only the CERTAIN tests: the static predictor scores every candidate 0.2, below lambda 0.5", ids(d.Tests))
	}
	if d.LockDigest != lock.Digest || d.Lambda != 0.5 {
		t.Errorf("LockDigest %q Lambda %v", d.LockDigest, d.Lambda)
	}
	if len(d.Scores) != 2 || d.Scores[0].Source != "static" || d.Scores[0].P != 0.2 {
		t.Errorf("Scores = %+v, want both candidates scored 0.2 by the static predictor", d.Scores)
	}
}

// TestSelectEnsembleTakesTheLargerProbability: either member can promote a
// test and neither can suppress it.
func TestSelectEnsembleTakesTheLargerProbability(t *testing.T) {
	srv := newJevServer(t, "")
	srv.P = func(name string) float64 {
		if name == "TestSolo" {
			return 0.9
		}
		return 0
	}
	f := lockedFixture(t, jevYML(srv, ""), extraFiles("e1", "e2", "e3"))
	f.logistic(t, defaultWeight, defaultBias)
	t.Setenv(f.config(t).Predictor.Jev.APIKeyEnv, "sk-test-engine-key")
	f.writeLock(t, "ensemble", nil)

	d := selectWith(t, f, func(o *Options) { o.load = clientLoader(srv.Client()) })

	requireNarrowed(t, d, "the ensemble")
	// top: the logistic model says sigmoid(-2) and Jev says 0, so the model wins.
	// solo: the logistic model says sigmoid(-6) and Jev says 0.9, so Jev wins.
	if want := []string{idCore, idLeft, idRght, idSolo, idTop}; !sameStrings(ids(d.Tests), want) {
		t.Fatalf("Tests = %v, want %v", ids(d.Tests), want)
	}
	if srv.requests.Load() != 5 {
		t.Errorf("%d requests, want one per candidate", srv.requests.Load())
	}
	bySource := map[string]TestScore{}
	for _, sc := range d.Scores {
		bySource[sc.TestID] = sc
	}
	if top := bySource[idTop]; top.Source != "logistic" || math.Abs(top.P-sigmoid(-2)) > 1e-9 {
		t.Errorf("top scored %+v, want the logistic probability", top)
	}
	if solo := bySource[idSolo]; solo.Source != "jev" || solo.P != 0.9 || solo.Shard < 0 {
		t.Errorf("solo scored %+v, want Jev's 0.9 from a shard", solo)
	}
	if e1 := bySource[extraID("e1")]; e1.Source != "logistic" || math.Abs(e1.P-sigmoid(-6)) > 1e-9 {
		t.Errorf("e1 scored %+v, want the logistic probability", e1)
	}
	if d.CostUSD <= 0 {
		t.Errorf("CostUSD = %v, want the cost of the answered shards", d.CostUSD)
	}
	if d.Lambda != lockLambda || d.LockDigest == "" {
		t.Errorf("Lambda %v LockDigest %q", d.Lambda, d.LockDigest)
	}
}

// TestSelectMostFailedShardsAbstain: a predictor that failed for more than a
// quarter of the candidates is not trusted, and the full suite runs.
func TestSelectMostFailedShardsAbstain(t *testing.T) {
	srv := newJevServer(t, "*")
	f := lockedFixture(t, jevYML(srv, ""), extraFiles("e1", "e2", "e3"))
	t.Setenv(f.config(t).Predictor.Jev.APIKeyEnv, "sk-test-engine-key")
	f.writeLock(t, "jev", nil)

	d := selectWith(t, f, func(o *Options) { o.load = clientLoader(srv.Client()) })

	requireFullSuite(t, d, "predictor_failure")
	if len(d.Hypothetical) != 8 {
		t.Errorf("Hypothetical = %v, want every test: the CERTAIN ones and all five unscored candidates", ids(d.Hypothetical))
	}
	if len(d.Scores) != 5 {
		t.Fatalf("Scores = %+v", d.Scores)
	}
	for _, sc := range d.Scores {
		if sc.Source != "jev_failed" || sc.P != 1 {
			t.Errorf("score %+v, want a failed shard at P = 1", sc)
		}
	}
}
