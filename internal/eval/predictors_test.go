package eval

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/omorsi45/hazard/internal/change"
	"github.com/omorsi45/hazard/internal/config"
	"github.com/omorsi45/hazard/internal/control"
	"github.com/omorsi45/hazard/internal/engine"
	"github.com/omorsi45/hazard/internal/impact"
	"github.com/omorsi45/hazard/internal/predict"
)

type fakePredictor struct{ name string }

func (f fakePredictor) Name() string    { return f.name }
func (f fakePredictor) Version() string { return "fake" }
func (f fakePredictor) Predict(context.Context, *change.ChangeSet, []impact.Candidate) ([]predict.Prob, error) {
	return nil, nil
}

// withLoader swaps the predictor loader and the environment lookup for a test.
func withLoader(t *testing.T, load func(string, config.Config, string) (predict.Predictor, control.Identity, error), env map[string]string) {
	t.Helper()
	oldLoad, oldEnv := loadPredictor, lookupEnv
	loadPredictor = load
	lookupEnv = func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	t.Cleanup(func() { loadPredictor, lookupEnv = oldLoad, oldEnv })
}

func TestLoadPredictorsMapsKindsToRowsAndKeepsTheUsableOnes(t *testing.T) {
	withLoader(t, func(_ string, _ config.Config, kind string) (predict.Predictor, control.Identity, error) {
		return fakePredictor{kind}, control.Identity{Predictor: kind, ModelRelease: "p0", WeightsDigest: "sha256:abc"}, nil
	}, nil)
	got := loadPredictors("repo", config.Defaults(), []string{"static", "logistic"})
	if len(got.States) != 2 || got.States[0] != (PredictorState{Kind: "static", ID: RowP0}) || got.States[1] != (PredictorState{Kind: "logistic", ID: RowP1}) {
		t.Fatalf("states = %+v", got.States)
	}
	if len(got.Usable) != 2 || got.Usable["static"] == nil {
		t.Errorf("usable = %v, want both predictors", got.Usable)
	}
	if got.Info[0].ModelRelease != "p0" || got.Info[0].WeightsDigest != "sha256:abc" || got.Info[0].ID != RowP0 {
		t.Errorf("info = %+v, want the identity of the artifact the row was scored with", got.Info[0])
	}
}

// A missing or refused artifact is a row that says so: never zeros.
func TestLoadPredictorsUnavailableArtifactReadsUnavailableWithTheReason(t *testing.T) {
	withLoader(t, func(string, config.Config, string) (predict.Predictor, control.Identity, error) {
		return nil, control.Identity{}, fmt.Errorf("%w: %w", engine.ErrPredictorUnavailable, errors.New("read .hazard/static.json:\nno such file"))
	}, nil)
	got := loadPredictors("repo", config.Defaults(), []string{"static"})
	st := got.States[0]
	if !strings.HasPrefix(st.Status, "unavailable: ") || !strings.Contains(st.Status, "no such file") || strings.ContainsAny(st.Status, "\r\n") {
		t.Errorf("status = %q, want one line, unavailable and the reason", st.Status)
	}
	if len(got.Usable) != 0 {
		t.Errorf("usable = %v, want none", got.Usable)
	}
}

func TestLoadPredictorsRemotePredictorsNeedTheKey(t *testing.T) {
	loaded := 0
	withLoader(t, func(string, config.Config, string) (predict.Predictor, control.Identity, error) {
		loaded++
		return fakePredictor{"remote"}, control.Identity{}, nil
	}, map[string]string{})
	got := loadPredictors("repo", config.Defaults(), []string{"jev", "ensemble"})
	for _, st := range got.States {
		if st.Status != "needs OPENROUTER_API_KEY" {
			t.Errorf("%s status = %q, want %q", st.Kind, st.Status, "needs OPENROUTER_API_KEY")
		}
	}
	if loaded != 0 {
		t.Errorf("the loader ran %d times without a key; a remote predictor must not even be built", loaded)
	}

	cfg := config.Defaults()
	cfg.Predictor.Jev.APIKeyEnv = "MY_KEY"
	withLoader(t, func(string, config.Config, string) (predict.Predictor, control.Identity, error) {
		return fakePredictor{"remote"}, control.Identity{}, nil
	}, map[string]string{"MY_KEY": "  "})
	if st := loadPredictors("repo", cfg, []string{"jev"}).States[0]; st.Status != "needs MY_KEY" {
		t.Errorf("a blank key counts as unset and names the configured variable: status = %q", st.Status)
	}
	withLoader(t, func(string, config.Config, string) (predict.Predictor, control.Identity, error) {
		return fakePredictor{"remote"}, control.Identity{ModelRelease: "jev-1"}, nil
	}, map[string]string{"MY_KEY": "k"})
	got = loadPredictors("repo", cfg, []string{"jev"})
	if got.States[0].Status != "" || got.Usable["jev"] == nil || got.States[0].ID != RowP2 {
		t.Errorf("with the key set: %+v usable=%v, want P2 usable", got.States[0], got.Usable)
	}
}
