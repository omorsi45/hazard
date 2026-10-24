package control

import (
	"math"
	"math/rand/v2"
	"sort"
	"testing"
)

// tableRisk returns a risk function backed by a lookup table that records
// which lambdas were evaluated.
func tableRisk(grid, risks []float64, calls *[]float64) func(float64) float64 {
	return func(l float64) float64 {
		*calls = append(*calls, l)
		for i, g := range grid {
			if g == l {
				return risks[i]
			}
		}
		panic("risk evaluated at a lambda outside the grid")
	}
}

func TestLTTFixedSequenceWalkStopsAtFirstNonRejection(t *testing.T) {
	// n=1000, alpha=0.01, delta=0.05. References (80 digit decimal):
	//   rhat 0     -> p 4.3171247410658251e-05  reject
	//   rhat 0.001 -> p 1.1847771378538459e-03  reject
	//   rhat 0.003 -> p 2.7380314431107977e-02  reject
	//   rhat 0.004 -> p 7.7977719841202126e-02  keep: stop
	// The fifth risk (0.0005) would be rejected on its own, but fixed-sequence
	// testing never looks past the first non-rejection.
	grid := []float64{0.01, 0.02, 0.03, 0.04, 0.05}
	risks := []float64{0, 0.001, 0.003, 0.004, 0.0005}
	var calls []float64
	res := LTT(1000, tableRisk(grid, risks, &calls), grid, 0.01, 0.05)

	if res.Refused {
		t.Fatalf("unexpected refusal: %+v", res)
	}
	if res.Lambda != 0.03 {
		t.Errorf("Lambda = %v, want 0.03 (largest lambda of the rejected prefix)", res.Lambda)
	}
	if res.EmpiricalMiss != 0.003 {
		t.Errorf("EmpiricalMiss = %v, want risk(Lambda) = 0.003", res.EmpiricalMiss)
	}
	wantP := []float64{4.3171247410658251e-05, 1.1847771378538459e-03, 2.7380314431107977e-02, 7.7977719841202126e-02}
	wantRej := []bool{true, true, true, false}
	if len(res.Points) != 4 {
		t.Fatalf("len(Points) = %d, want 4 (lambdas after the stop are not evaluated): %+v", len(res.Points), res.Points)
	}
	for i, p := range res.Points {
		if p.Lambda != grid[i] || p.Risk != risks[i] || p.Rejected != wantRej[i] || relDiff(p.PValue, wantP[i]) > 1e-9 {
			t.Errorf("Points[%d] = %+v, want lambda %v risk %v p %v rejected %v", i, p, grid[i], risks[i], wantP[i], wantRej[i])
		}
	}
	if len(calls) != 4 {
		t.Errorf("risk evaluated %d times (%v), want 4: nothing after the stop may be evaluated", len(calls), calls)
	}
	if res.HaveN != 1000 {
		t.Errorf("HaveN = %d, want 1000", res.HaveN)
	}
}

func TestLTTAllRejectedPicksLargestLambda(t *testing.T) {
	grid := []float64{0.01, 0.02, 0.03}
	risks := []float64{0, 0, 0}
	var calls []float64
	res := LTT(1000, tableRisk(grid, risks, &calls), grid, 0.01, 0.05)
	if res.Refused || res.Lambda != 0.03 || res.EmpiricalMiss != 0 || len(res.Points) != 3 {
		t.Errorf("got %+v, want lambda 0.03, miss 0, 3 points, not refused", res)
	}
}

func TestLTTRefusesBelowMinimumNEvenWithZeroRisk(t *testing.T) {
	grid, err := ParseGrid("0.005:0.5:0.005")
	if err != nil {
		t.Fatal(err)
	}
	zero := func(float64) float64 { return 0 }
	for _, n := range []int{1, 2, 50, 200, 298} {
		res := LTT(n, zero, grid, 0.01, 0.05)
		if !res.Refused {
			t.Errorf("n=%d: not refused: %+v", n, res)
		}
		if res.NeedN != 299 || res.HaveN != n {
			t.Errorf("n=%d: NeedN/HaveN = %d/%d, want 299/%d", n, res.NeedN, res.HaveN, n)
		}
		if res.Lambda != 0 {
			t.Errorf("n=%d: Lambda = %v on refusal, want 0 (selects everything)", n, res.Lambda)
		}
		if res.EmpiricalMiss != 1 {
			t.Errorf("n=%d: EmpiricalMiss = %v on refusal, want 1 (unknown is never zero)", n, res.EmpiricalMiss)
		}
	}
	// 299 is the first n that can certify with zero loss.
	res := LTT(299, zero, grid, 0.01, 0.05)
	if res.Refused {
		t.Errorf("n=299 with zero risk must certify: %+v", res)
	}
	if res.Lambda != 0.5 {
		t.Errorf("n=299 zero risk: Lambda = %v, want the top of the grid 0.5", res.Lambda)
	}
}

func TestLTTRefusesWhenTheFirstLambdaIsNotRejected(t *testing.T) {
	// Enough samples, but the most conservative lambda still misses 2% > alpha.
	grid := []float64{0.01, 0.02}
	risks := []float64{0.02, 0.0}
	var calls []float64
	res := LTT(5000, tableRisk(grid, risks, &calls), grid, 0.01, 0.05)
	if !res.Refused || res.NeedN != 299 || res.HaveN != 5000 {
		t.Fatalf("got %+v, want refused with NeedN 299, HaveN 5000", res)
	}
	if len(res.Points) != 1 || res.Points[0].Rejected || res.Points[0].PValue != 1 {
		t.Errorf("Points = %+v, want the one evaluated point, p=1, not rejected", res.Points)
	}
}

func TestLTTNonMonotoneRiskStillStopsAtFirstFailure(t *testing.T) {
	grid := []float64{0.01, 0.02, 0.03, 0.04}
	risks := []float64{0, 0.02, 0, 0}
	var calls []float64
	res := LTT(1000, tableRisk(grid, risks, &calls), grid, 0.01, 0.05)
	if res.Refused || res.Lambda != 0.01 || len(res.Points) != 2 {
		t.Errorf("got %+v, want lambda 0.01 from a 2 point walk", res)
	}
}

func TestLTTInvalidArgumentsRefuse(t *testing.T) {
	zero := func(float64) float64 { return 0 }
	ok := []float64{0.01, 0.02}
	nan := math.NaN()
	tests := []struct {
		name         string
		n            int
		risk         func(float64) float64
		grid         []float64
		alpha, delta float64
	}{
		{"empty grid", 1000, zero, nil, 0.01, 0.05},
		{"descending grid", 1000, zero, []float64{0.02, 0.01}, 0.01, 0.05},
		{"duplicate lambda", 1000, zero, []float64{0.01, 0.01}, 0.01, 0.05},
		{"NaN lambda", 1000, zero, []float64{0.01, nan}, 0.01, 0.05},
		{"infinite lambda", 1000, zero, []float64{0.01, math.Inf(1)}, 0.01, 0.05},
		{"nil risk", 1000, nil, ok, 0.01, 0.05},
		{"zero n", 0, zero, ok, 0.01, 0.05},
		{"negative n", -4, zero, ok, 0.01, 0.05},
		{"alpha zero", 1000, zero, ok, 0, 0.05},
		{"alpha one", 1000, zero, ok, 1, 0.05},
		{"delta zero", 1000, zero, ok, 0.01, 0},
		{"delta NaN", 1000, zero, ok, 0.01, nan},
	}
	for _, tc := range tests {
		res := LTT(tc.n, tc.risk, tc.grid, tc.alpha, tc.delta)
		if !res.Refused || len(res.Points) != 0 || res.Lambda != 0 {
			t.Errorf("%s: got %+v, want a refusal with no points", tc.name, res)
		}
		if res.HaveN != tc.n {
			t.Errorf("%s: HaveN = %d, want %d", tc.name, res.HaveN, tc.n)
		}
	}
	if res := LTT(1000, zero, ok, 0, 0.05); res.NeedN != math.MaxInt {
		t.Errorf("invalid alpha: NeedN = %d, want math.MaxInt", res.NeedN)
	}
}

func TestLTTInvalidRiskValueIsNonRejection(t *testing.T) {
	grid := []float64{0.01, 0.02, 0.03}
	for _, bad := range []float64{math.NaN(), -0.1, 1.5, math.Inf(1)} {
		risks := []float64{0, bad, 0}
		var calls []float64
		res := LTT(1000, tableRisk(grid, risks, &calls), grid, 0.01, 0.05)
		if res.Refused || res.Lambda != 0.01 {
			t.Errorf("risk %v: got %+v, want the walk to stop before the invalid value, lambda 0.01", bad, res)
			continue
		}
		if len(res.Points) != 2 || res.Points[1].Rejected || !math.IsNaN(res.Points[1].PValue) {
			t.Errorf("risk %v: Points = %+v, want the invalid point recorded as NaN, not rejected", bad, res.Points)
		}
	}
}

func TestLTTDoesNotModifyItsGrid(t *testing.T) {
	grid := []float64{0.01, 0.02, 0.03}
	before := append([]float64(nil), grid...)
	var calls []float64
	LTT(1000, tableRisk(grid, []float64{0, 0, 0}, &calls), grid, 0.01, 0.05)
	for i := range grid {
		if grid[i] != before[i] {
			t.Fatalf("grid modified: %v", grid)
		}
	}
}

// TestLTTFalseCertificationRate is the LTT correctness check (G09): each
// commit draws u ~ U(0,1) and misses at lambda when u < lambda/4, so the true
// risk is R(lambda) = lambda/4 and R(lambda) > alpha exactly when lambda >
// 0.04. Across 1000 calibration samples the fraction certifying a lambda
// whose true risk exceeds alpha must stay near or below delta.
func TestLTTFalseCertificationRate(t *testing.T) {
	const (
		seeds = 1000
		n     = 2000
		alpha = 0.01
		delta = 0.05
		// delta*seeds = 50; three binomial standard deviations (about 6.9
		// each) put the ceiling at about 70.
		maxFalse = 70
	)
	grid, err := ParseGrid("0.005:0.5:0.005")
	if err != nil {
		t.Fatal(err)
	}
	falseCert, certified := 0, 0
	for seed := 0; seed < seeds; seed++ {
		rng := rand.New(rand.NewPCG(uint64(seed), 0x9e3779b97f4a7c15))
		u := make([]float64, n)
		for i := range u {
			u[i] = rng.Float64()
		}
		sort.Float64s(u)
		risk := func(lambda float64) float64 {
			// number of commits with u < lambda/4
			return float64(sort.SearchFloat64s(u, lambda/4)) / n
		}
		res := LTT(n, risk, grid, alpha, delta)
		if res.Refused {
			continue
		}
		certified++
		if res.Lambda/4 > alpha+1e-12 {
			falseCert++
		}
	}
	t.Logf("false certifications: %d of %d seeds (certified %d); ceiling %d", falseCert, seeds, certified, maxFalse)
	if falseCert > maxFalse {
		t.Errorf("false certification count %d exceeds %d", falseCert, maxFalse)
	}
	// The check must not pass vacuously by refusing everything.
	if certified < 900 {
		t.Errorf("only %d of %d seeds certified any lambda: the test would pass by refusing", certified, seeds)
	}
}
