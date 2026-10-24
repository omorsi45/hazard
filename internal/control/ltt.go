package control

import "math"

// GridPoint is one lambda that LTT evaluated.
type GridPoint struct {
	Lambda   float64 // the threshold
	Risk     float64 // empirical miss rate at Lambda
	PValue   float64 // Hoeffding-Bentkus p-value for H : R(Lambda) > alpha (NaN if Risk was invalid)
	Rejected bool    // PValue <= delta
}

// LTTResult is the outcome of a Learn-then-Test run.
type LTTResult struct {
	// Lambda is the largest lambda of the rejected prefix. When Refused it is
	// 0, which selects every test.
	Lambda  float64
	Refused bool // no lambda can be certified at this (alpha, delta, n)
	// NeedN is MinCalibrationN(alpha, delta) and HaveN is n, for the
	// "need n >= X, have Y" message.
	NeedN, HaveN int
	// Points holds every evaluated lambda in grid order, ending at the first
	// non-rejection. Lambdas after that stop are not evaluated.
	Points []GridPoint
	// EmpiricalMiss is the risk at Lambda. When Refused it is 1: nothing was
	// certified, and an unknown miss rate is never reported as zero.
	EmpiricalMiss float64
}

// LTT runs fixed-sequence Learn-then-Test. grid must be strictly ascending;
// risk(lambda) is the empirical miss rate of the calibration sample at lambda.
// Starting from the smallest (most conservative) lambda it rejects
// H_l : R(l) > alpha while the Hoeffding-Bentkus p-value is <= delta and stops
// at the first non-rejection, so a monotone loss needs no multiplicity
// correction. Lambda is the largest rejected one; if none is rejected the run
// is refused. A p-value that is NaN (invalid risk) counts as non-rejection.
//
// Arguments that cannot be evaluated (n <= 0, nil risk, a grid that is empty,
// not finite or not strictly ascending, invalid alpha or delta) refuse
// without evaluating any lambda.
func LTT(n int, risk func(lambda float64) float64, grid []float64, alpha, delta float64) LTTResult {
	need := MinCalibrationN(alpha, delta)
	res := LTTResult{Refused: true, NeedN: need, HaveN: n, EmpiricalMiss: 1}
	if n <= 0 || risk == nil || need == math.MaxInt || !strictlyAscending(grid) {
		return res
	}

	points := make([]GridPoint, 0, len(grid))
	for _, lambda := range grid {
		r := risk(lambda)
		p := HBPValue(n, r, alpha)
		rejected := p <= delta // false for NaN
		points = append(points, GridPoint{Lambda: lambda, Risk: r, PValue: p, Rejected: rejected})
		if !rejected {
			break
		}
	}
	res.Points = points

	rejectedPrefix := len(points)
	if !points[rejectedPrefix-1].Rejected {
		rejectedPrefix--
	}
	if rejectedPrefix == 0 {
		return res
	}
	last := points[rejectedPrefix-1]
	res.Refused = false
	res.Lambda = last.Lambda
	res.EmpiricalMiss = last.Risk
	return res
}

// strictlyAscending reports whether grid is non-empty, finite and strictly
// increasing.
func strictlyAscending(grid []float64) bool {
	if len(grid) == 0 {
		return false
	}
	for i, l := range grid {
		if math.IsNaN(l) || math.IsInf(l, 0) {
			return false
		}
		if i > 0 && !(l > grid[i-1]) {
			return false
		}
	}
	return true
}
