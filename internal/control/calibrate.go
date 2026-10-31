package control

import (
	"slices"
	"time"
)

// lockCreatedFormat is the layout of Lock.CreatedAt: UTC to the second.
const lockCreatedFormat = "2006-01-02T15:04:05Z"

// CalUnit is one calibration commit as the controller saw it.
type CalUnit struct {
	ID      string   // the event ID of the commit, "ci:<sha>"
	Failing []string // F(c): the test IDs of its strong regression labels
	Input   Input    // everything Decide needs; Lambda is set for each evaluation
	Failed  bool     // the commit could not be replayed: its outcome is unknown, which is never a success
}

// CalibrationParams are the inputs of Calibrate besides the units. Identity
// is what the lock binds the threshold to; the caller takes it from the
// running predictor and policy, never by hand, so the controller's Verify
// accepts the lock for the system it was fitted on.
type CalibrationParams struct {
	Alpha, Delta   float64
	Grid           []float64
	Unit           string
	Split, SplitAt string
	L1Mode         string // the mode the units were replayed in: L1ModeGraph or L1ModeCoverage
	Identity       Identity
	Now            time.Time
}

// Calibration is the outcome of Calibrate. Lock is the zero Lock when the
// run was refused; otherwise it is complete except for its Digest, which
// WriteLock sets.
type Calibration struct {
	LTT  LTTResult
	Lock Lock
}

// UnitLoss is L(c, lambda) of seed 10.1 for one calibration commit: 1 when
// some regression of the commit is not in the selection the controller makes
// at lambda, else 0. The selection is what Decide returns for the unit's
// input, so every abstention rule of production applies: an abstention runs
// the full suite, which cannot miss, and has loss 0. A regression that is
// neither certain nor a scored candidate is not selected and is a miss: an
// unknown is never counted as caught. A unit whose replay failed is such an
// unknown: what production would have selected cannot be told, so its loss is
// 1 at every lambda.
func UnitLoss(u CalUnit, lambda float64) float64 {
	if u.Failed {
		return 1
	}
	in := u.Input
	in.Lambda = lambda
	d := Decide(in)
	if d.FullSuite {
		return 0
	}
	selected := make(map[string]bool, len(d.Members))
	for _, m := range d.Members {
		selected[m.TestID] = true
	}
	for _, id := range u.Failing {
		if !selected[id] {
			return 1
		}
	}
	return 0
}

// Calibrate runs Learn-then-Test (LTT) over the calibration units: the risk
// at each lambda of the grid is the mean UnitLoss, and the threshold is the
// largest lambda of the rejected prefix. When LTT refuses (too few units for
// (alpha, delta), or no lambda with a defensible miss rate) the lock stays
// empty. Otherwise the lock carries the threshold, the sample size, the
// empirical miss, the p-value of every evaluated lambda, the sorted event IDs
// of the units and the identity of p. The result does not depend on the order
// of units.
func Calibrate(units []CalUnit, p CalibrationParams) Calibration {
	n := len(units)
	risk := func(lambda float64) float64 {
		misses := 0.0
		for _, u := range units {
			misses += UnitLoss(u, lambda)
		}
		return misses / float64(n)
	}
	res := LTT(n, risk, p.Grid, p.Alpha, p.Delta)
	if res.Refused {
		return Calibration{LTT: res}
	}

	ids := make([]string, n)
	failures := 0
	for i, u := range units {
		ids[i] = u.ID
		if u.Failed {
			failures++
		}
	}
	slices.Sort(ids)
	pvalues := make(map[string]float64, len(res.Points))
	for _, pt := range res.Points {
		pvalues[PValueKey(pt.Lambda)] = pt.PValue
	}
	return Calibration{LTT: res, Lock: Lock{
		Version:           lockVersion,
		CreatedAt:         p.Now.UTC().Format(lockCreatedFormat),
		Alpha:             p.Alpha,
		Delta:             p.Delta,
		Unit:              p.Unit,
		Lambda:            res.Lambda,
		Predictor:         p.Identity.Predictor,
		ModelVersion:      p.Identity.ModelRelease,
		RubricVersion:     p.Identity.RubricVersion,
		FeatureVersion:    p.Identity.FeatureVersion,
		WeightsDigest:     p.Identity.WeightsDigest,
		PolicyDigest:      p.Identity.PolicyDigest,
		L1Mode:            p.L1Mode,
		NCal:              n,
		ReplayFailures:    failures,
		EmpiricalMissRate: res.EmpiricalMiss,
		Split:             p.Split,
		SplitAt:           p.SplitAt,
		Grid:              slices.Clone(p.Grid),
		PValues:           pvalues,
		CalibrationIDs:    ids,
	}}
}
