package control

import "math"

// maxBisections caps the Clopper-Pearson search; 200 halvings of [0, 1] are
// far past float64 resolution, so the loop normally ends on convergence.
const maxBisections = 200

// ClopperPearsonUpper returns the exact one-sided Clopper-Pearson upper
// confidence bound for a binomial proportion after k events in n trials: the
// p solving P(Bin(n, p) <= k) = 1 - confidence. k = n gives 1 and k = 0 gives
// 1 - (1-confidence)^(1/n). The bound is found by bisection on the binomial
// CDF and the conservative (upper) end of the final bracket is returned.
//
// Input that cannot be evaluated (n <= 0, k outside [0, n], confidence
// outside (0, 1)) returns 1: an upper bound that cannot be computed must trip
// the drift alarm, not hide it.
func ClopperPearsonUpper(k, n int, confidence float64) float64 {
	if n <= 0 || k < 0 || k > n || !(confidence > 0 && confidence < 1) {
		return 1
	}
	if k == n {
		return 1
	}
	logTail := math.Log1p(-confidence) // ln(1 - confidence)
	if k == 0 {
		return -math.Expm1(logTail / float64(n))
	}
	lo, hi := 0.0, 1.0
	for i := 0; i < maxBisections; i++ {
		mid := lo + (hi-lo)/2
		if mid <= lo || mid >= hi {
			break
		}
		if logBinomCDF(k, n, mid) > logTail {
			lo = mid // the CDF is still too large: the bound is above mid
		} else {
			hi = mid
		}
	}
	return hi
}
