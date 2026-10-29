package predict

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strings"
	"testing"

	"github.com/omorsi45/hazard/internal/change"
	"github.com/omorsi45/hazard/internal/impact"
)

// stub is a Predictor that answers from a function and counts its calls.
type stub struct {
	name, version string
	answer        func(cands []impact.Candidate) ([]Prob, error)
	calls         int
	sawContext    context.Context
}

func (s *stub) Name() string    { return s.name }
func (s *stub) Version() string { return s.version }
func (s *stub) Predict(ctx context.Context, _ *change.ChangeSet, cands []impact.Candidate) ([]Prob, error) {
	s.calls++
	s.sawContext = ctx
	return s.answer(cands)
}

// fixed answers each candidate, in order, with the given probabilities.
func fixed(source string, ps ...float64) func([]impact.Candidate) ([]Prob, error) {
	return func(cands []impact.Candidate) ([]Prob, error) {
		out := make([]Prob, len(cands))
		for i, c := range cands {
			out[i] = Prob{TestID: c.TestID, P: ps[i], Source: source, Shard: -1}
		}
		return out, nil
	}
}

func failing(err error) func([]impact.Candidate) ([]Prob, error) {
	return func([]impact.Candidate) ([]Prob, error) { return nil, err }
}

func TestEnsembleTakesTheMaximumPerCandidate(t *testing.T) {
	local := &stub{name: "logistic", version: "p1/abc", answer: func(cands []impact.Candidate) ([]Prob, error) {
		probs, _ := fixed("logistic", 0.1, 0.9, 0.5, 0)(cands)
		for i := range probs {
			probs[i].LatencyMS, probs[i].CostUSD = 2, 0
		}
		return probs, nil
	}}
	remote := &stub{name: "jev", version: "typesafe/jev-1.13", answer: func(cands []impact.Candidate) ([]Prob, error) {
		probs, _ := fixed("jev", 0.4, 0.2, 0.5, 0)(cands)
		for i := range probs {
			probs[i].LatencyMS, probs[i].CostUSD, probs[i].Shard, probs[i].Truncated = 30, 0.001, 2, i == 3
		}
		return probs, nil
	}}
	e := Ensemble{Local: local, Remote: remote}
	cands := candidatesN(4)
	probs, err := e.Predict(t.Context(), goldenChange(), cands)
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	if err := ValidateProbs(cands, probs); err != nil {
		t.Fatal(err)
	}
	wantP := []float64{0.4, 0.9, 0.5, 0}
	wantSource := []string{"jev", "logistic", "jev", "jev"} // a tie goes to the remote answer
	for i, p := range probs {
		if p.P != wantP[i] || p.Source != wantSource[i] {
			t.Fatalf("prob %d = %+v, want P %v from %s", i, p, wantP[i], wantSource[i])
		}
		if p.LatencyMS != 32 || p.CostUSD != 0.001 || p.Shard != 2 || p.Truncated != (i == 3) {
			t.Fatalf("prob %d = %+v: latency and cost add up, shard and truncation come from the remote", i, p)
		}
	}
}

func TestEnsembleNeitherSignalCanSuppressTheOther(t *testing.T) {
	cands := candidatesN(2)
	for _, tc := range []struct {
		name          string
		local, remote []float64
	}{
		{"remote high", []float64{0, 0}, []float64{1, 0.7}},
		{"local high", []float64{1, 0.7}, []float64{0, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := Ensemble{
				Local:  &stub{answer: fixed("logistic", tc.local...)},
				Remote: &stub{answer: fixed("jev", tc.remote...)},
			}
			probs, err := e.Predict(t.Context(), goldenChange(), cands)
			if err != nil {
				t.Fatal(err)
			}
			if probs[0].P != 1 || probs[1].P != 0.7 {
				t.Fatalf("probs = %+v, want 1 and 0.7", probs)
			}
		})
	}
}

func TestEnsembleFailedRemoteShardSelectsEverything(t *testing.T) {
	f := newFake(t, func(_ int, _ []byte, w http.ResponseWriter, _ *http.Request) { http.Error(w, "down", 503) })
	e := Ensemble{
		Local:  &stub{name: "logistic", answer: fixed("logistic", 0, 0.2, 0.05)},
		Remote: f.jev(t, nil),
	}
	probs, err := e.Predict(t.Context(), goldenChange(), candidatesN(3))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range probs {
		if p.P != 1 || p.Source != "jev_failed" {
			t.Fatalf("prob = %+v, want 1.0 from the failed remote", p)
		}
	}
}

func TestEnsembleWithTheRealClient(t *testing.T) {
	f := newFake(t, answerAll(t, servedSnapshot, 0.0004, func(name string) float64 {
		if name == "Test0002" {
			return 0.8
		}
		return 0.01
	}))
	e := Ensemble{
		Local:  &stub{name: "logistic", answer: fixed("logistic", 0.3, 0.1, 0.02)},
		Remote: f.jev(t, nil),
	}
	probs, err := e.Predict(t.Context(), goldenChange(), candidatesN(3))
	if err != nil {
		t.Fatal(err)
	}
	want := []float64{0.3, 0.8, 0.02}
	for i, p := range probs {
		if math.Abs(p.P-want[i]) > 1e-12 {
			t.Fatalf("prob %d = %v, want %v", i, p.P, want[i])
		}
	}
}

func TestEnsembleErrorsPropagate(t *testing.T) {
	boom := errors.New("local model is gone")
	t.Run("remote drift", func(t *testing.T) {
		remote := &stub{answer: failing(ErrModelDrift)}
		e := Ensemble{Local: &stub{answer: fixed("logistic", 0.1)}, Remote: remote}
		probs, err := e.Predict(t.Context(), goldenChange(), candidatesN(1))
		if !errors.Is(err, ErrModelDrift) || probs != nil {
			t.Fatalf("Predict = %v, %v, want ErrModelDrift and no probabilities", probs, err)
		}
	})
	t.Run("local error skips the remote", func(t *testing.T) {
		remote := &stub{answer: fixed("jev", 0.1)}
		e := Ensemble{Local: &stub{answer: failing(boom)}, Remote: remote}
		probs, err := e.Predict(t.Context(), goldenChange(), candidatesN(1))
		if !errors.Is(err, boom) || probs != nil {
			t.Fatalf("Predict = %v, %v, want the local error", probs, err)
		}
		if remote.calls != 0 {
			t.Fatalf("the remote predictor was called %d times after the local one failed", remote.calls)
		}
	})
	t.Run("remote error keeps the local answer out", func(t *testing.T) {
		remote := &stub{answer: failing(boom)}
		e := Ensemble{Local: &stub{answer: fixed("logistic", 0.9)}, Remote: remote}
		probs, err := e.Predict(t.Context(), goldenChange(), candidatesN(1))
		if !errors.Is(err, boom) || probs != nil {
			t.Fatalf("Predict = %v, %v, want the remote error and no partial answer", probs, err)
		}
	})
	t.Run("real client drift", func(t *testing.T) {
		f := newFake(t, answerAll(t, "typesafe/jev-1.14-20261001", 0.0004, constant(0.1)))
		e := Ensemble{Local: &stub{answer: fixed("logistic", 0.1)}, Remote: f.jev(t, nil)}
		if _, err := e.Predict(t.Context(), goldenChange(), candidatesN(1)); !errors.Is(err, ErrModelDrift) {
			t.Fatalf("Predict error = %v, want ErrModelDrift", err)
		}
	})
}

func TestEnsembleRejectsAnswersThatBreakTheContract(t *testing.T) {
	cands := candidatesN(2)
	cases := map[string]struct {
		local, remote func([]impact.Candidate) ([]Prob, error)
		wantErr       string
	}{
		"short local": {
			local:   func(c []impact.Candidate) ([]Prob, error) { p, _ := fixed("logistic", 0, 0)(c); return p[:1], nil },
			remote:  fixed("jev", 0, 0),
			wantErr: "local",
		},
		"nan remote": {
			local:   fixed("logistic", 0, 0),
			remote:  fixed("jev", 0, math.NaN()),
			wantErr: "remote",
		},
		"out of range local": {
			local:   fixed("logistic", 1.2, 0),
			remote:  fixed("jev", 0, 0),
			wantErr: "local",
		},
		"reordered remote": {
			local: fixed("logistic", 0, 0),
			remote: func(c []impact.Candidate) ([]Prob, error) {
				p, _ := fixed("jev", 0, 0)(c)
				p[0], p[1] = p[1], p[0]
				return p, nil
			},
			wantErr: "remote",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := Ensemble{Local: &stub{answer: tc.local}, Remote: &stub{answer: tc.remote}}
			probs, err := e.Predict(t.Context(), goldenChange(), cands)
			if err == nil || probs != nil {
				t.Fatalf("Predict = %v, %v, want an error and no probabilities", probs, err)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not name the %s predictor", err, tc.wantErr)
			}
		})
	}
}

func TestEnsembleIdentityAndWiring(t *testing.T) {
	local := &stub{name: "logistic", version: "p1/abc", answer: fixed("logistic", 0.1)}
	remote := &stub{name: "jev", version: "typesafe/jev-1.13", answer: fixed("jev", 0.2)}
	e := Ensemble{Local: local, Remote: remote}
	if e.Name() != "ensemble" || e.Version() != "p1/abc+typesafe/jev-1.13" {
		t.Fatalf("name %q version %q", e.Name(), e.Version())
	}
	probs, err := e.Predict(t.Context(), goldenChange(), nil)
	if err != nil || probs == nil || len(probs) != 0 {
		t.Fatalf("Predict with no candidates = %v, %v", probs, err)
	}
	type key struct{}
	ctx := context.WithValue(t.Context(), key{}, "v")
	if _, err := e.Predict(ctx, goldenChange(), candidatesN(1)); err != nil {
		t.Fatal(err)
	}
	if local.sawContext.Value(key{}) != "v" || remote.sawContext.Value(key{}) != "v" {
		t.Fatal("the context did not reach both predictors")
	}
	for name, bad := range map[string]Ensemble{"no local": {Remote: remote}, "no remote": {Local: local}, "neither": {}} {
		if _, err := bad.Predict(t.Context(), goldenChange(), candidatesN(1)); err == nil {
			t.Fatalf("%s: Predict accepted a missing member", name)
		}
		_ = bad.Version()
		_ = bad.Name()
	}
}
