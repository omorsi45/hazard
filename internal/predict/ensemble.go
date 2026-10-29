package predict

import (
	"context"
	"errors"
	"fmt"

	"github.com/omorsi45/hazard/internal/change"
	"github.com/omorsi45/hazard/internal/impact"
)

// Ensemble scores each candidate with the larger of two predictors. max is the
// one-sided combiner: either signal can promote a test, neither can suppress
// it. A shard the remote predictor could not answer is already 1.0, so it
// stays 1.0 here.
type Ensemble struct{ Local, Remote Predictor }

var _ Predictor = Ensemble{}

// Name returns the predictor name.
func (e Ensemble) Name() string { return "ensemble" }

// Version joins the versions of both members, local first.
func (e Ensemble) Version() string {
	if e.Local == nil || e.Remote == nil {
		return "ensemble"
	}
	return e.Local.Version() + "+" + e.Remote.Version()
}

// Predict runs the local predictor, then the remote one, and returns the
// larger probability per candidate. An error of either member is the error of
// the ensemble, with no partial answer: the local member runs first so that a
// local failure costs no request. An answer of a member that breaks the
// Predictor contract is an error too.
func (e Ensemble) Predict(ctx context.Context, ch *change.ChangeSet, cands []impact.Candidate) ([]Prob, error) {
	if e.Local == nil || e.Remote == nil {
		return nil, errors.New("ensemble: both a local and a remote predictor are required")
	}
	if len(cands) == 0 {
		return []Prob{}, nil
	}
	local, err := e.Local.Predict(ctx, ch, cands)
	if err != nil {
		return nil, fmt.Errorf("ensemble: local predictor %s: %w", e.Local.Name(), err)
	}
	if err := ValidateProbs(cands, local); err != nil {
		return nil, fmt.Errorf("ensemble: local predictor %s: %w", e.Local.Name(), err)
	}
	remote, err := e.Remote.Predict(ctx, ch, cands)
	if err != nil {
		return nil, fmt.Errorf("ensemble: remote predictor %s: %w", e.Remote.Name(), err)
	}
	if err := ValidateProbs(cands, remote); err != nil {
		return nil, fmt.Errorf("ensemble: remote predictor %s: %w", e.Remote.Name(), err)
	}
	out := make([]Prob, len(cands))
	for i := range cands {
		out[i] = maxProb(local[i], remote[i])
	}
	return out, nil
}

// maxProb returns the larger of two answers for one test. The winner keeps its
// source, a tie goes to the remote answer so that a failed shard stays
// visible. Latency and cost add up; the shard and the truncation flag belong
// to the remote request.
func maxProb(local, remote Prob) Prob {
	out := remote
	if local.P > remote.P {
		out = local
	}
	out.LatencyMS = local.LatencyMS + remote.LatencyMS
	out.CostUSD = local.CostUSD + remote.CostUSD
	out.Shard, out.Truncated = remote.Shard, remote.Truncated
	return out
}
