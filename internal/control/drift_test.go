package control

import (
	"math"
	"testing"
)

func TestClopperPearsonUpperKnownVectors(t *testing.T) {
	// References from 60 digit decimal bisection on the exact binomial CDF.
	tests := []struct {
		k, n int
		conf float64
		want float64
	}{
		{0, 10, 0.95, 1 - math.Pow(0.05, 1.0/10)}, // closed form 1-(1-conf)^(1/n)
		{0, 100, 0.95, 0.029513049607039935},
		{1, 10, 0.95, 0.39416330243650478},
		{3, 50, 0.9, 0.12875642258660188},
		{5, 100, 0.99, 0.12585173069767864},
	}
	for _, tc := range tests {
		got := ClopperPearsonUpper(tc.k, tc.n, tc.conf)
		if math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("ClopperPearsonUpper(%d, %d, %v) = %.15g, want %.15g", tc.k, tc.n, tc.conf, got, tc.want)
		}
	}
}

func TestClopperPearsonUpperAllMissesIsOne(t *testing.T) {
	for _, conf := range []float64{0.5, 0.9, 0.95, 0.999} {
		if got := ClopperPearsonUpper(10, 10, conf); got != 1 {
			t.Errorf("ClopperPearsonUpper(10, 10, %v) = %v, want 1", conf, got)
		}
	}
}

func TestClopperPearsonUpperSolvesTheDefiningEquation(t *testing.T) {
	for _, n := range []int{1, 5, 30, 400, 5000} {
		for _, k := range []int{0, 1, n / 10, n / 2, n - 1} {
			if k >= n {
				continue // k = n is exactly 1 (covered by AllMissesIsOne)
			}
			for _, conf := range []float64{0.8, 0.95, 0.99} {
				u := ClopperPearsonUpper(k, n, conf)
				if !(u > 0 && u < 1) {
					t.Errorf("ClopperPearsonUpper(%d, %d, %v) = %v, want inside (0,1)", k, n, conf, u)
					continue
				}
				if u < float64(k)/float64(n) {
					t.Errorf("upper bound %v below the point estimate %v (k=%d n=%d)", u, float64(k)/float64(n), k, n)
				}
				if got := math.Exp(logBinomCDF(k, n, u)); math.Abs(got-(1-conf)) > 1e-9 {
					t.Errorf("P(Bin(%d,U)<=%d) = %.12g, want %v", n, k, got, 1-conf)
				}
			}
		}
	}
}

func TestClopperPearsonUpperMonotone(t *testing.T) {
	prev := 0.0
	for k := 0; k <= 40; k++ {
		u := ClopperPearsonUpper(k, 40, 0.95)
		if u <= prev && k > 0 {
			t.Fatalf("not increasing in k: k=%d gives %v after %v", k, u, prev)
		}
		prev = u
	}
	prev = 0
	for _, conf := range []float64{0.5, 0.8, 0.9, 0.95, 0.99, 0.999} {
		u := ClopperPearsonUpper(2, 40, conf)
		if u <= prev {
			t.Fatalf("not increasing in confidence: %v gives %v after %v", conf, u, prev)
		}
		prev = u
	}
}

func TestClopperPearsonUpperInvalidInputIsOne(t *testing.T) {
	// A bound that cannot be computed must trip the drift alarm, never hide it.
	nan := math.NaN()
	tests := []struct {
		name string
		k, n int
		conf float64
	}{
		{"n zero", 0, 0, 0.95},
		{"n negative", 0, -5, 0.95},
		{"k negative", -1, 10, 0.95},
		{"k above n", 11, 10, 0.95},
		{"confidence zero", 1, 10, 0},
		{"confidence one", 1, 10, 1},
		{"confidence NaN", 1, 10, nan},
		{"confidence above one", 1, 10, 1.5},
	}
	for _, tc := range tests {
		if got := ClopperPearsonUpper(tc.k, tc.n, tc.conf); got != 1 {
			t.Errorf("%s: ClopperPearsonUpper(%d, %d, %v) = %v, want 1", tc.name, tc.k, tc.n, tc.conf, got)
		}
	}
}
