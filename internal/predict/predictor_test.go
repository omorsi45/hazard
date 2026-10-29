package predict

import (
	"math"
	"strings"
	"testing"

	"github.com/omorsi45/hazard/internal/impact"
)

func candidates(ids ...string) []impact.Candidate {
	out := make([]impact.Candidate, len(ids))
	for i, id := range ids {
		out[i] = impact.Candidate{TestID: id, TestName: "Test" + id, Package: "m/p"}
	}
	return out
}

func probsOf(ids []string, ps ...float64) []Prob {
	out := make([]Prob, len(ids))
	for i, id := range ids {
		out[i] = Prob{TestID: id, P: ps[i]}
	}
	return out
}

func TestProbValidate(t *testing.T) {
	ids := []string{"a", "b", "c"}
	cases := []struct {
		name    string
		cands   []impact.Candidate
		probs   []Prob
		wantErr string // substring, empty means valid
	}{
		{"valid", candidates(ids...), probsOf(ids, 0, 0.5, 1), ""},
		{"empty", nil, nil, ""},
		{"too few", candidates(ids...), probsOf(ids[:2], 0, 0.5), "2 probabilities for 3 candidates"},
		{"too many", candidates(ids[:2]...), probsOf(ids, 0, 0.5, 1), "3 probabilities for 2 candidates"},
		{"wrong id", candidates(ids...), []Prob{{TestID: "a"}, {TestID: "x"}, {TestID: "c"}}, `probability 1 is for "x", want "b"`},
		{"reordered", candidates(ids...), []Prob{{TestID: "b"}, {TestID: "a"}, {TestID: "c"}}, `probability 0 is for "b", want "a"`},
		{"nan", candidates(ids...), probsOf(ids, 0, math.NaN(), 1), "not finite"},
		{"plus inf", candidates(ids...), probsOf(ids, math.Inf(1), 0, 1), "not finite"},
		{"minus inf", candidates(ids...), probsOf(ids, 0, 0, math.Inf(-1)), "not finite"},
		{"negative", candidates(ids...), probsOf(ids, 0, -0.01, 1), "outside [0, 1]"},
		{"above one", candidates(ids...), probsOf(ids, 0, 0.5, 1.0000001), "outside [0, 1]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateProbs(tc.cands, tc.probs)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateProbs = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateProbs = nil, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ValidateProbs = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}
