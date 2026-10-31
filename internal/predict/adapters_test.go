package predict

import (
	"context"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/omorsi45/hazard/internal/impact"
)

// adapterCase is one local predictor wrapped around a tiny verified artifact,
// with candidates in TestID order and the probability Score gives each.
type adapterCase struct {
	name, source string
	predictor    Predictor
	cands        []impact.Candidate
	want         []float64
}

func adapterCases(t *testing.T) []adapterCase {
	t.Helper()
	static := NewStatic(sealStatic(t, sampleStatic())) // X 0.1 0.5 2, Y 0.05 0.25 0.75
	logistic := NewLogistic(sealLogistic(t, vectorArtifact(vectorModel())))
	return []adapterCase{
		{
			name: "static", source: "static", predictor: static,
			cands: []impact.Candidate{{TestID: "a", Rank: 0}, {TestID: "b", Rank: 0.5}, {TestID: "c", Rank: 9}},
			want:  []float64{0.05, 0.25, 0.75},
		},
		{
			name: "logistic", source: "logistic", predictor: logistic,
			cands: []impact.Candidate{
				{TestID: "a", Features: impact.Features{}},
				{TestID: "b", Features: impact.Features{Hops: 3, Reachable: true, TestFailRate: 0.25}},
				{TestID: "c", Features: impact.Features{Hops: 1, Reachable: true}},
			},
			want: []float64{sigmoidRef(195.5), sigmoidRef(3.5), sigmoidRef(-2.5)},
		},
	}
}

func TestLocalPredictorsImplementPredictor(t *testing.T) {
	for _, tc := range adapterCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.predictor.Name(); got != tc.name {
				t.Errorf("Name() = %q, want %q", got, tc.name)
			}
		})
	}
	s := NewStatic(sealStatic(t, sampleStatic()))
	if got, want := Predictor(s).Version(), s.Version(); got != want || !strings.HasPrefix(got, "p0/sha256:") {
		t.Errorf("static Version() through the interface = %q, want %q", got, want)
	}
	a := sealLogistic(t, vectorArtifact(vectorModel()))
	if got, want := Predictor(NewLogistic(a)).Version(), "p1/"+a.Digest; got != want {
		t.Errorf("logistic Version() through the interface = %q, want %q", got, want)
	}
}

func TestLocalPredictReturnsOneProbPerCandidateInOrder(t *testing.T) {
	for _, tc := range adapterCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.predictor.Predict(context.Background(), nil, tc.cands)
			if err != nil {
				t.Fatalf("Predict: %v", err)
			}
			if err := ValidateProbs(tc.cands, got); err != nil {
				t.Fatalf("ValidateProbs: %v", err)
			}
			for i, p := range got {
				if p.TestID != tc.cands[i].TestID || !approx(p.P, tc.want[i], 1e-12) {
					t.Errorf("Predict[%d] = %s=%v, want %s=%v", i, p.TestID, p.P, tc.cands[i].TestID, tc.want[i])
				}
				if p.Source != tc.source {
					t.Errorf("Predict[%d].Source = %q, want %q", i, p.Source, tc.source)
				}
				if p.Shard != -1 || p.LatencyMS != 0 || p.CostUSD != 0 || p.Truncated {
					t.Errorf("Predict[%d] = %+v, want no shard, latency, cost or truncation", i, p)
				}
			}
		})
	}
}

func TestLocalPredictOfNoCandidates(t *testing.T) {
	for _, tc := range adapterCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.predictor.Predict(context.Background(), nil, nil)
			if err != nil || len(got) != 0 {
				t.Errorf("Predict(nil) = %v, %v, want no probabilities and no error", got, err)
			}
		})
	}
}

// TestLocalPredictRefusesCandidatesOutOfTestIDOrder: Score answers in TestID
// order, so candidates in another order cannot be answered one to one. The
// adapter reports it instead of pairing a probability with the wrong test.
func TestLocalPredictRefusesCandidatesOutOfTestIDOrder(t *testing.T) {
	for _, tc := range adapterCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			shuffled := []impact.Candidate{tc.cands[2], tc.cands[0], tc.cands[1]}
			got, err := tc.predictor.Predict(context.Background(), nil, shuffled)
			if err == nil {
				t.Fatalf("Predict = %v with no error for candidates out of order", got)
			}
			if got != nil {
				t.Errorf("Predict returned %v alongside an error", got)
			}
			if !strings.Contains(err.Error(), tc.name+" predictor") || !strings.Contains(err.Error(), `want "c"`) {
				t.Errorf("error %q does not name the predictor and the mismatch", err)
			}
		})
	}
}

func TestLocalPredictRefusesAnUnknownScore(t *testing.T) {
	static := NewStatic(sealStatic(t, sampleStatic()))
	logistic := NewLogistic(sealLogistic(t, vectorArtifact(vectorModel())))
	cases := map[string]struct {
		predictor Predictor
		cands     []impact.Candidate
	}{
		"static NaN rank": {static, []impact.Candidate{{TestID: "a", Rank: 1}, {TestID: "b", Rank: math.NaN()}}},
		"logistic NaN feature": {logistic, []impact.Candidate{
			{TestID: "a", Features: impact.Features{}},
			{TestID: "b", Features: impact.Features{TestFailRate: math.NaN()}},
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := tc.predictor.Predict(context.Background(), nil, tc.cands)
			if err == nil || got != nil {
				t.Fatalf("Predict = %v, %v, want a nil answer and an error", got, err)
			}
			if !strings.Contains(err.Error(), `"b"`) && !strings.Contains(err.Error(), " b ") && !strings.Contains(err.Error(), "b:") {
				t.Errorf("error %q does not name the test", err)
			}
		})
	}
}

func TestLocalPredictRefusesAnUnverifiedArtifact(t *testing.T) {
	cases := map[string]Predictor{
		"static":   NewStatic(sampleStatic()),                  // never written, so no digest
		"logistic": NewLogistic(vectorArtifact(vectorModel())), // never written, so no digest
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			cands := []impact.Candidate{{TestID: "a"}}
			got, err := p.Predict(context.Background(), nil, cands)
			if err == nil || got != nil {
				t.Fatalf("Predict = %v, %v, want a nil answer and an error", got, err)
			}
		})
	}
}

func TestLocalPredictHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range adapterCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.predictor.Predict(ctx, nil, tc.cands)
			if err == nil || got != nil {
				t.Fatalf("Predict = %v, %v, want a nil answer and an error", got, err)
			}
			if !reflect.DeepEqual(err, context.Canceled) {
				t.Errorf("error = %v, want context.Canceled", err)
			}
		})
	}
}
