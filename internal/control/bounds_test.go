package control

import (
	"math"
	"testing"
)

func relDiff(got, want float64) float64 {
	if got == want {
		return 0
	}
	return math.Abs(got-want) / math.Max(math.Abs(want), math.SmallestNonzeroFloat64)
}

func TestMinCalibrationNVectors(t *testing.T) {
	// Hand computed: smallest n with (1-alpha)^n <= delta, i.e.
	// n = ceil(ln(delta)/ln(1-alpha)).
	tests := []struct {
		alpha, delta float64
		want         int
	}{
		{0.01, 0.05, 299},   // 0.99^298 = 0.0501 > 0.05, 0.99^299 = 0.0496
		{0.05, 0.05, 59},    // ln 0.05 / ln 0.95 = 58.4
		{0.1, 0.1, 22},      // ln 0.1 / ln 0.9 = 21.85
		{0.02, 0.05, 149},   // 148.28
		{0.001, 0.05, 2995}, // 2994.2
		{0.05, 0.01, 90},    // 89.78
	}
	for _, tc := range tests {
		got := MinCalibrationN(tc.alpha, tc.delta)
		if got != tc.want {
			t.Errorf("MinCalibrationN(%v, %v) = %d, want %d", tc.alpha, tc.delta, got, tc.want)
		}
		// definition: the returned n satisfies the bound and n-1 does not.
		if p := HBPValue(got, 0, tc.alpha); !(p <= tc.delta) {
			t.Errorf("HBPValue(%d, 0, %v) = %v > delta %v", got, tc.alpha, p, tc.delta)
		}
		if p := HBPValue(got-1, 0, tc.alpha); !(p > tc.delta) {
			t.Errorf("HBPValue(%d, 0, %v) = %v <= delta %v, n is not minimal", got-1, tc.alpha, p, tc.delta)
		}
	}
}

func TestMinCalibrationNUnreachableIsNeverZero(t *testing.T) {
	bad := [][2]float64{
		{0, 0.05}, {1, 0.05}, {-0.1, 0.05}, {math.NaN(), 0.05},
		{0.01, 0}, {0.01, -1}, {0.01, math.NaN()}, {0.01, math.Inf(1)}, {0.01, 1}, {0.01, 1.5},
		{1e-300, 0.05}, // needs more than the search bound: unreachable, not small
	}
	for _, tc := range bad {
		if got := MinCalibrationN(tc[0], tc[1]); got != math.MaxInt {
			t.Errorf("MinCalibrationN(%v, %v) = %d, want math.MaxInt (unknown is never zero)", tc[0], tc[1], got)
		}
	}
}

func TestHBPValueIsOneWhenEmpiricalLossReachesAlpha(t *testing.T) {
	tests := []struct {
		n           int
		rhat, alpha float64
	}{
		{1000, 0.05, 0.01}, // the clip guard vector: un-clipped KL would give ~1e-18
		{1000, 0.01, 0.01}, // rhat == alpha
		{10, 1, 0.5},
		{1, 1, 0.999},
		{200000, 0.02, 0.01},
	}
	for _, tc := range tests {
		if got := HBPValue(tc.n, tc.rhat, tc.alpha); got != 1 {
			t.Errorf("HBPValue(%d, %v, %v) = %v, want exactly 1", tc.n, tc.rhat, tc.alpha, got)
		}
	}
}

func TestHBPValueInvalidInputIsNaN(t *testing.T) {
	nan := math.NaN()
	inf := math.Inf(1)
	tests := []struct {
		name        string
		n           int
		rhat, alpha float64
	}{
		{"n zero", 0, 0.1, 0.5},
		{"n negative", -3, 0.1, 0.5},
		{"rhat negative", 10, -0.1, 0.5},
		{"rhat above one", 10, 1.1, 0.5},
		{"rhat NaN", 10, nan, 0.5},
		{"rhat inf", 10, inf, 0.5},
		{"alpha zero", 10, 0.1, 0},
		{"alpha one", 10, 0.1, 1},
		{"alpha negative", 10, 0.1, -0.2},
		{"alpha above one", 10, 0.1, 1.5},
		{"alpha NaN", 10, 0.1, nan},
	}
	for _, tc := range tests {
		if got := HBPValue(tc.n, tc.rhat, tc.alpha); !math.IsNaN(got) {
			t.Errorf("%s: HBPValue(%d, %v, %v) = %v, want NaN", tc.name, tc.n, tc.rhat, tc.alpha, got)
		}
	}
}

func TestHBPValueZeroLossUsesLimitOfKL(t *testing.T) {
	// 0 log 0 = 0, so KL(0||alpha) = -ln(1-alpha) and the exp branch is (1-alpha)^n.
	if got, want := HBPValue(10, 0, 0.5), 1.0/1024; relDiff(got, want) > 1e-15 {
		t.Errorf("HBPValue(10, 0, 0.5) = %v, want %v", got, want)
	}
	if got, want := HBPValue(299, 0, 0.01), math.Pow(0.99, 299); relDiff(got, want) > 1e-12 {
		t.Errorf("HBPValue(299, 0, 0.01) = %v, want %v", got, want)
	}
	// Underflow is a legitimate 0 (reject), never NaN.
	if got := HBPValue(1_000_000, 0, 0.01); got != 0 {
		t.Errorf("HBPValue(1e6, 0, 0.01) = %v, want 0 (underflow)", got)
	}
}

func TestHBPValueBranches(t *testing.T) {
	// n=20, alpha=0.5, rhat=0.25 -> k=5. Exact rational reference:
	// P(Bin(20,.5) <= 5) = 5425/262144, e * that = 0.05625411575084808...
	// exp(-20 KL(.25||.5)) = 0.07307706433667735...  The binomial branch is the min.
	ch, bi := hbTerms(20, 0.25, 0.5)
	if relDiff(ch, 0.0730770643366773511) > 1e-12 {
		t.Errorf("chernoff term = %v, want 0.07307706433667735", ch)
	}
	if relDiff(bi, 0.0562541157508480850) > 1e-12 {
		t.Errorf("binomial term = %v, want 0.05625411575084808", bi)
	}
	if got := HBPValue(20, 0.25, 0.5); relDiff(got, 0.0562541157508480850) > 1e-12 {
		t.Errorf("HBPValue(20, .25, .5) = %v, want the binomial branch 0.05625411575084808", got)
	}
	// rhat = 0: the exp branch wins because of the factor e.
	ch, bi = hbTerms(100, 0, 0.05)
	if !(ch < bi) {
		t.Errorf("rhat=0: chernoff %v should be below binomial %v", ch, bi)
	}
}

func TestHBPValueMatchesExactReferenceAtLargeN(t *testing.T) {
	// References from 80 digit decimal arithmetic with an lgamma-free recurrence.
	tests := []struct {
		n           int
		rhat, alpha float64
		want        float64
		tol         float64 // relative
	}{
		{1000, 0.005, 0.01, 1.7978583254514914e-01, 1e-9},
		{50, 0.04, 0.2, 3.4941201094308096e-03, 1e-9},
		{200000, 0.0099, 0.01, 9.0161762080846289e-01, 1e-9},
		{200000, 0.0001, 0.001, 1.6601303247137484e-59, 1e-6},
		{1_000_000, 0.0095, 0.01, 5.6754304148871343e-07, 1e-6},
	}
	for _, tc := range tests {
		got := HBPValue(tc.n, tc.rhat, tc.alpha)
		if math.IsNaN(got) || relDiff(got, tc.want) > tc.tol {
			t.Errorf("HBPValue(%d, %v, %v) = %.17g, want %.17g (rel tol %g)", tc.n, tc.rhat, tc.alpha, got, tc.want, tc.tol)
		}
	}
}

func TestHBPValueCountDerivedRhatUsesTheExactCount(t *testing.T) {
	// rhat = k/n makes n*rhat an integer in exact arithmetic, but in float64
	// the product can land one ulp above k (200000 * (1980/200000) is
	// 1980.0000000000002). The tail must still be summed to exactly k.
	for _, n := range []int{49, 100, 1000, 3000, 200000} {
		for _, alpha := range []float64{0.01, 0.05, 0.3} {
			stride := 1 + n/50000
			for k := 0; k < n && float64(k)/float64(n) < alpha; k += stride {
				rhat := float64(k) / float64(n)
				_, binom := hbTerms(n, rhat, alpha)
				want := math.E * math.Exp(logBinomCDF(k, n, alpha))
				if binom != want {
					t.Fatalf("hbTerms(%d, %d/%d, %v) binomial term = %.17g, want the k=%d tail %.17g", n, k, n, alpha, binom, k, want)
				}
			}
		}
	}
}

func TestHBPValueNonIntegerCountRoundsUp(t *testing.T) {
	// n*rhat = 12.5 -> the tail is summed to 13, never 12.
	n, rhat, alpha := 1000, 0.0125, 0.05
	_, binom := hbTerms(n, rhat, alpha)
	if want := math.E * math.Exp(logBinomCDF(13, n, alpha)); binom != want {
		t.Errorf("binomial term = %.17g, want the ceil=13 tail %.17g", binom, want)
	}
}

func TestHBPValueMonotoneAndBounded(t *testing.T) {
	for _, n := range []int{1, 7, 100, 2000, 50000} {
		for _, alpha := range []float64{0.001, 0.01, 0.2, 0.9} {
			prev := -1.0
			for i := 0; i <= 40; i++ {
				rhat := alpha * float64(i) / 40
				p := HBPValue(n, rhat, alpha)
				if math.IsNaN(p) || p < 0 || p > 1 {
					t.Fatalf("HBPValue(%d, %v, %v) = %v outside [0,1]", n, rhat, alpha, p)
				}
				if p < prev {
					t.Fatalf("HBPValue(%d, %v, %v) = %v below previous %v: not monotone in rhat", n, rhat, alpha, p, prev)
				}
				prev = p
			}
		}
	}
}
