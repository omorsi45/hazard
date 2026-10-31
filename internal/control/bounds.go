// Package control implements the statistical core of the risk controller:
// the Hoeffding-Bentkus p-value, Learn-then-Test threshold selection, the
// calibration lock with its identity checks, and the drift upper bound.
package control

import "math"

// maxSearchN bounds the search for the minimum calibration size. A
// requirement beyond it is reported as unreachable (math.MaxInt).
const maxSearchN = 1 << 40

// negligibleLog is how far below the largest term a binomial term may fall
// before the remaining (monotonically shrinking) terms are dropped: e^-50 is
// 2e-22 of the largest term, far below float64 resolution of the sum.
const negligibleLog = -50.0

// countSlack is the relative distance from an integer within which n*rhat is
// read as that integer (see missCount). It is several orders of magnitude
// above float64 rounding of (k/n)*n and far below any real fractional count.
const countSlack = 1e-12

// HBPValue returns the Hoeffding-Bentkus p-value for H : R > alpha given n
// calibration commits with empirical loss rhat.
//
// It is 1 whenever rhat >= alpha: the lower-tail test carries no evidence
// against H there, and the un-clipped KL term would wrongly accept large
// upper-tail losses. Otherwise
//
//	p = min(exp(-n KL(rhat||alpha)), e * P(Bin(n, alpha) <= ceil(n*rhat)))
//
// clipped to [0, 1], with 0 ln 0 = 0 and the binomial tail summed in log
// space. Invalid input (n <= 0, rhat outside [0,1], alpha outside (0,1), any
// NaN) returns NaN; callers must treat NaN as non-rejection.
func HBPValue(n int, rhat, alpha float64) float64 {
	if n <= 0 || !(rhat >= 0 && rhat <= 1) || !(alpha > 0 && alpha < 1) {
		return math.NaN()
	}
	if rhat >= alpha {
		return 1
	}
	chernoff, binom := hbTerms(n, rhat, alpha)
	return math.Min(1, math.Max(0, math.Min(chernoff, binom)))
}

// hbTerms returns the two branches of the bound, exp(-n KL(rhat||alpha)) and
// e * P(Bin(n, alpha) <= ceil(n*rhat)). It requires valid input with
// rhat < alpha.
func hbTerms(n int, rhat, alpha float64) (chernoff, binom float64) {
	chernoff = math.Exp(-float64(n) * bernoulliKL(rhat, alpha))
	binom = math.E * math.Exp(logBinomCDF(missCount(n, rhat), n, alpha))
	return chernoff, binom
}

// missCount returns ceil(n*rhat). When rhat is an empirical rate k/n the
// product is the integer k in exact arithmetic but float64 can place it one
// ulp above k, which a bare ceil would turn into k+1. A product within
// countSlack of an integer is therefore taken as that integer.
func missCount(n int, rhat float64) int {
	x := float64(n) * rhat
	r := math.Round(x)
	if math.Abs(x-r) > countSlack*math.Max(1, r) {
		r = math.Ceil(x)
	}
	return int(math.Min(r, float64(n)))
}

// bernoulliKL returns KL(a||b) = a ln(a/b) + (1-a) ln((1-a)/(1-b)) for
// 0 <= a < b < 1, with 0 ln 0 = 0. The log1p forms stay accurate when a is
// close to b or b is tiny.
func bernoulliKL(a, b float64) float64 {
	kl := (1 - a) * math.Log1p((b-a)/(1-b))
	if a > 0 {
		kl += a * math.Log1p((a-b)/b)
	}
	return kl
}

// logBinomCDF returns ln P(Bin(n, p) <= k) for 0 < p < 1. Terms are built
// from log-gamma and summed relative to the largest one, so it is stable for
// n up to 1e6 and beyond.
func logBinomCDF(k, n int, p float64) float64 {
	if k < 0 {
		return math.Inf(-1)
	}
	if k >= n {
		return 0
	}
	lp, lq := math.Log(p), math.Log1p(-p)
	lgN, _ := math.Lgamma(float64(n) + 1)
	logPMF := func(i int) float64 {
		a, _ := math.Lgamma(float64(i) + 1)
		b, _ := math.Lgamma(float64(n-i) + 1)
		return lgN - a - b + float64(i)*lp + float64(n-i)*lq
	}

	// The pmf rises to the mode floor((n+1)p) and falls after it, so the
	// largest term in [0, k] sits at min(k, mode).
	peak := int(math.Floor(float64(n+1) * p))
	if peak > k {
		peak = k
	}
	top := logPMF(peak)

	sum := 1.0 // the peak term itself, relative to itself
	for i := peak - 1; i >= 0; i-- {
		d := logPMF(i) - top
		if d < negligibleLog {
			break
		}
		sum += math.Exp(d)
	}
	for i := peak + 1; i <= k; i++ {
		d := logPMF(i) - top
		if d < negligibleLog {
			break
		}
		sum += math.Exp(d)
	}
	return math.Min(0, top+math.Log(sum))
}

// MinCalibrationN returns the smallest n for which a loss-free calibration
// sample rejects H at level delta, that is the smallest n with
// HBPValue(n, 0, alpha) <= delta. That function is decreasing in n, so the
// search doubles then bisects. An invalid (alpha, delta) or a requirement
// beyond maxSearchN returns math.MaxInt: an unknown requirement is never zero.
func MinCalibrationN(alpha, delta float64) int {
	if !(alpha > 0 && alpha < 1) || !(delta > 0 && delta < 1) {
		return math.MaxInt
	}
	ok := func(n int) bool { return HBPValue(n, 0, alpha) <= delta }

	hi := 1
	for !ok(hi) {
		if hi >= maxSearchN {
			return math.MaxInt
		}
		hi *= 2
	}
	lo := hi / 2 // ok(lo) is false (or lo == 0); the answer is in (lo, hi]
	for hi-lo > 1 {
		mid := lo + (hi-lo)/2
		if ok(mid) {
			hi = mid
		} else {
			lo = mid
		}
	}
	return hi
}
