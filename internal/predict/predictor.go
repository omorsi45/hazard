// Package predict holds the probability predictors of L2: the Jev Decisions
// client, its judgment cache and the max ensemble. Every predictor answers
// with exactly one Prob per candidate, and every failure or unknown degrades
// toward 1.0 so that a missing answer selects the test, never drops it.
package predict

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/omorsi45/hazard/internal/change"
	"github.com/omorsi45/hazard/internal/impact"
)

// Prob is the probability that the change breaks one candidate test.
type Prob struct {
	TestID    string
	P         float64
	Source    string // "jev", "cache", "logistic", "static" or a failure source
	LatencyMS int
	CostUSD   float64
	Shard     int  // index of the request that answered, -1 when no request did
	Truncated bool // the descriptor sent for this test was cut to fit a token limit
}

// Predictor scores candidates. Version must match the lock's model_version.
type Predictor interface {
	Name() string
	Version() string
	Predict(ctx context.Context, ch *change.ChangeSet, cands []impact.Candidate) ([]Prob, error)
}

// ErrModelDrift means the served model release differs from the configured
// release, so the threshold fitted against the configured release no longer
// applies.
var ErrModelDrift = errors.New("served model release differs from the configured release")

// ValidateProbs checks that probs answers cands one to one and in order, and
// that every probability is finite and within [0, 1]. A violation is an
// error, never a zero.
func ValidateProbs(cands []impact.Candidate, probs []Prob) error {
	if len(probs) != len(cands) {
		return fmt.Errorf("%d probabilities for %d candidates", len(probs), len(cands))
	}
	for i, p := range probs {
		if p.TestID != cands[i].TestID {
			return fmt.Errorf("probability %d is for %q, want %q", i, p.TestID, cands[i].TestID)
		}
		if math.IsNaN(p.P) || math.IsInf(p.P, 0) {
			return fmt.Errorf("probability for %q is not finite: %v", p.TestID, p.P)
		}
		if p.P < 0 || p.P > 1 {
			return fmt.Errorf("probability for %q is outside [0, 1]: %v", p.TestID, p.P)
		}
	}
	return nil
}
