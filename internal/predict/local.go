package predict

import (
	"context"
	"fmt"
	"math"
	"sort"

	"github.com/omorsi45/hazard/internal/change"
	"github.com/omorsi45/hazard/internal/impact"
)

// LocalProb is one local prediction: the probability of Score, without the
// request accounting of Prob.
type LocalProb struct {
	TestID string
	P      float64
}

// Static is the P0 predictor: the isotonic model of an artifact applied to
// the L1 rank of each candidate.
type Static struct {
	a   StaticArtifact
	err error
}

// NewStatic wraps a. The artifact is verified once, including its digest, so
// pass one returned by ReadStatic; Score reports the failure of an artifact
// that does not verify.
func NewStatic(a StaticArtifact) *Static { return &Static{a: a, err: a.verify()} }

// Version is "p0/" followed by the artifact digest.
func (s *Static) Version() string { return "p0/" + s.a.Digest }

// Score returns one probability per candidate, sorted by TestID. A rank that
// is not finite, or a probability that is not, is an error: an unknown
// probability is never 0. The input is not modified.
func (s *Static) Score(c []impact.Candidate) ([]LocalProb, error) {
	if s.err != nil {
		return nil, fmt.Errorf("static predictor: %w", s.err)
	}
	out := make([]LocalProb, len(c))
	for i, cand := range c {
		p := s.a.Model.Predict(cand.Rank)
		if !isFinite(cand.Rank) || !isFinite(p) {
			return nil, fmt.Errorf("static predictor: candidate %s has no finite probability for rank %v", cand.TestID, cand.Rank)
		}
		out[i] = LocalProb{TestID: cand.TestID, P: p}
	}
	sortProbs(out)
	return out, nil
}

// Logistic is the P1 predictor: the calibrated logistic model of an artifact
// applied to the feature vector of each candidate.
type Logistic struct {
	a   LogisticArtifact
	err error
}

// NewLogistic wraps a. The artifact is verified once, including its digest,
// so pass one returned by ReadLogistic; Score reports the failure of an
// artifact that does not verify.
func NewLogistic(a LogisticArtifact) *Logistic { return &Logistic{a: a, err: a.verify()} }

// Version is "p1/" followed by the artifact digest.
func (l *Logistic) Version() string { return "p1/" + l.a.Digest }

// WeightsDigest is the artifact digest, which covers the standardization, the
// weights, the Platt parameters and the training record. The lock binds it.
func (l *Logistic) WeightsDigest() string { return l.a.Digest }

// Score returns one probability per candidate, sorted by TestID. A feature
// vector of the wrong length or with a non-finite value, or a score that is
// not finite, is an error: an unknown probability is never 0. The input is
// not modified.
func (l *Logistic) Score(c []impact.Candidate) ([]LocalProb, error) {
	if l.err != nil {
		return nil, fmt.Errorf("logistic predictor: %w", l.err)
	}
	out := make([]LocalProb, len(c))
	for i, cand := range c {
		p, err := l.a.Model.Prob(cand.Features.Vector())
		if err != nil {
			return nil, fmt.Errorf("logistic predictor: candidate %s: %w", cand.TestID, err)
		}
		out[i] = LocalProb{TestID: cand.TestID, P: p}
	}
	sortProbs(out)
	return out, nil
}

func sortProbs(ps []LocalProb) {
	sort.SliceStable(ps, func(i, j int) bool { return ps[i].TestID < ps[j].TestID })
}

var (
	_ Predictor = (*Static)(nil)
	_ Predictor = (*Logistic)(nil)
)

// Name is "static".
func (s *Static) Name() string { return "static" }

// Predict returns one Prob per candidate, Source "static", through Score.
// Candidates must be in TestID order, as the Predictor contract has it: Score
// answers in that order, and an answer that does not line up with the
// candidates is an error, never a probability paired with the wrong test.
func (s *Static) Predict(ctx context.Context, _ *change.ChangeSet, cands []impact.Candidate) ([]Prob, error) {
	return predictLocal(ctx, "static", s.Score, cands)
}

// Name is "logistic".
func (l *Logistic) Name() string { return "logistic" }

// Predict returns one Prob per candidate, Source "logistic", through Score,
// under the same ordering contract as (*Static).Predict.
func (l *Logistic) Predict(ctx context.Context, _ *change.ChangeSet, cands []impact.Candidate) ([]Prob, error) {
	return predictLocal(ctx, "logistic", l.Score, cands)
}

// predictLocal turns the answer of a local scorer into validated Probs. No
// request was made, so Shard is -1 and latency and cost are zero.
func predictLocal(ctx context.Context, source string, score func([]impact.Candidate) ([]LocalProb, error), cands []impact.Candidate) ([]Prob, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	scored, err := score(cands)
	if err != nil {
		return nil, err
	}
	out := make([]Prob, len(scored))
	for i, lp := range scored {
		out[i] = Prob{TestID: lp.TestID, P: lp.P, Source: source, Shard: -1}
	}
	if err := ValidateProbs(cands, out); err != nil {
		return nil, fmt.Errorf("%s predictor: %w", source, err)
	}
	return out, nil
}

// splitEpsilon absorbs floating point error in n*fraction, which can land just
// below an integer (0.57*100 is 56.99999999999999).
const splitEpsilon = 1e-9

// SplitGroups divides the distinct groups into a fit set and a scale set,
// deterministically: the groups are sorted and the first floor(n*fitFraction)
// go to fit, the rest to scale. Pass zero-padded dates first in each key for
// a chronological split. A fraction below 0 or NaN fits nothing and one above
// 1 fits everything. Both maps are always non-nil.
func SplitGroups(groups []string, fitFraction float64) (fit, scale map[string]bool) {
	distinct := make(map[string]bool, len(groups))
	for _, g := range groups {
		distinct[g] = true
	}
	sorted := make([]string, 0, len(distinct))
	for g := range distinct {
		sorted = append(sorted, g)
	}
	sort.Strings(sorted)

	frac := fitFraction
	if math.IsNaN(frac) || frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	nFit := min(int(math.Floor(float64(len(sorted))*frac+splitEpsilon)), len(sorted))

	fit, scale = make(map[string]bool, nFit), make(map[string]bool, len(sorted)-nFit)
	for i, g := range sorted {
		if i < nFit {
			fit[g] = true
		} else {
			scale[g] = true
		}
	}
	return fit, scale
}
