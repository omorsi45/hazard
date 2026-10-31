package control

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
	"time"
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
		// Hoeffding-Bentkus is conservative: delta*seeds = 50 is the level of
		// an exact test, and this experiment is deterministic (2 false
		// certifications). The ceiling sits at half the level, so a level
		// inflated tenfold (p <= 10*delta, 67 false certifications) fails.
		maxFalse = 25
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

// TestLTTCertifiesNothingWhenTheRiskExceedsAlphaFromTheFirstLambda: the loss
// curve above begins below alpha and grows smoothly, so its walk never meets an
// empirical loss far above alpha, the only situation in which the un-clipped KL
// term of the Hoeffding-Bentkus bound misbehaves (it shrinks as the loss grows
// and would reject H). Here the true risk exceeds alpha at the first grid point
// and everywhere after it: R(lambda) = 0.02 and 0.05 against alpha = 0.01.
// Every H is true, so a certification of any lambda is a false one, and across
// 1000 samples there must be none. The p-value is clipped to 1 whenever the
// empirical loss reaches alpha; without the clip all of the 1000 certify.
func TestLTTCertifiesNothingWhenTheRiskExceedsAlphaFromTheFirstLambda(t *testing.T) {
	const (
		seeds = 1000
		n     = 2000
		alpha = 0.01
		delta = 0.05
	)
	grid, err := ParseGrid("0.005:0.5:0.005")
	if err != nil {
		t.Fatal(err)
	}
	for _, trueRisk := range []float64{0.02, 0.05} {
		certified := 0
		for seed := 0; seed < seeds; seed++ {
			rng := rand.New(rand.NewPCG(uint64(seed), 0x9e3779b97f4a7c15))
			misses := 0
			for i := 0; i < n; i++ {
				if rng.Float64() < trueRisk {
					misses++
				}
			}
			risk := func(float64) float64 { return float64(misses) / n } // the same at every lambda
			if res := LTT(n, risk, grid, alpha, delta); !res.Refused {
				certified++
			}
		}
		if certified != 0 {
			t.Errorf("true risk %v: %d of %d seeds certified a lambda although R(lambda) > alpha everywhere", trueRisk, certified, seeds)
		}
	}
}

// Cross-implementation check (G10). The Go statistics are compared with the
// independent Python module in research/ on one shared vectors file.

type crossHB struct {
	N     int     `json:"n"`
	Rhat  float64 `json:"rhat"`
	Alpha float64 `json:"alpha"`
}

type crossMinN struct {
	Alpha float64 `json:"alpha"`
	Delta float64 `json:"delta"`
}

type crossLTT struct {
	N     int       `json:"n"`
	Alpha float64   `json:"alpha"`
	Delta float64   `json:"delta"`
	Grid  []float64 `json:"grid"`
	Risk  []float64 `json:"risk"`
}

type crossCP struct {
	K          int     `json:"k"`
	N          int     `json:"n"`
	Confidence float64 `json:"confidence"`
}

type crossVectors struct {
	HB   []crossHB   `json:"hb"`
	MinN []crossMinN `json:"min_n"`
	LTT  []crossLTT  `json:"ltt"`
	CP   []crossCP   `json:"cp"`
}

type crossPoint struct {
	Lambda   float64  `json:"lambda"`
	Risk     float64  `json:"risk"`
	P        *float64 `json:"p"`
	Rejected bool     `json:"rejected"`
}

type crossResults struct {
	HB []struct {
		P *float64 `json:"p"`
	} `json:"hb"`
	MinN []struct {
		N *int `json:"n"`
	} `json:"min_n"`
	LTT []struct {
		Lambda  float64      `json:"lambda"`
		Refused bool         `json:"refused"`
		NeedN   *int         `json:"need_n"`
		HaveN   int          `json:"have_n"`
		Points  []crossPoint `json:"points"`
	} `json:"ltt"`
	CP []struct {
		Upper float64 `json:"upper"`
	} `json:"cp"`
}

// crossTolerance is the agreement required between the two implementations.
const crossTolerance = 1e-9

func crossHBVectors() []crossHB {
	var out []crossHB
	ns := []int{1, 2, 3, 5, 10, 25, 50, 100, 299, 300, 500, 1000, 2000, 5000, 20000, 100000, 200000}
	alphas := []float64{0.001, 0.01, 0.05, 0.2}
	// fractions of alpha: zero loss, interior, near alpha, at alpha, above alpha
	fracs := []float64{0, 0.1, 0.5, 0.9, 0.99, 0.999999, 1, 1.5}
	for _, n := range ns {
		for _, a := range alphas {
			for _, f := range fracs {
				out = append(out, crossHB{N: n, Rhat: math.Min(a*f, 1), Alpha: a})
			}
			for _, f := range []float64{1 - 1e-9, 1 - 1e-12} { // just below alpha
				out = append(out, crossHB{N: n, Rhat: a * f, Alpha: a})
			}
			if n >= 100 { // empirical rates k/n
				for _, f := range []float64{0.3, 0.8, 0.97} {
					k := math.Floor(float64(n) * a * f)
					out = append(out, crossHB{N: n, Rhat: k / float64(n), Alpha: a})
				}
			}
		}
	}
	return out
}

func crossLTTVectors(t *testing.T) []crossLTT {
	t.Helper()
	gridA, err := ParseGrid("0.005:0.5:0.005")
	if err != nil {
		t.Fatal(err)
	}
	gridC, err := ParseGrid("0.01:0.9:0.01")
	if err != nil {
		t.Fatal(err)
	}
	rates := func(grid []float64, n int, misses func(lambda float64) float64) []float64 {
		out := make([]float64, len(grid))
		for i, l := range grid {
			out[i] = misses(l) / float64(n)
		}
		return out
	}
	zeros := func(grid []float64) []float64 { return make([]float64, len(grid)) }
	firstFails := zeros(gridA)
	firstFails[0] = 0.02

	return []crossLTT{
		// certifies in the middle of the grid: misses grow as floor(40*lambda)
		{N: 1000, Alpha: 0.01, Delta: 0.05, Grid: gridA, Risk: rates(gridA, 1000, func(l float64) float64 { return math.Floor(l * 40) })},
		// below the minimum n: refuses even with zero loss
		{N: 150, Alpha: 0.01, Delta: 0.05, Grid: gridA, Risk: zeros(gridA)},
		// a different (alpha, delta) and a coarser, longer grid
		{N: 5000, Alpha: 0.02, Delta: 0.1, Grid: gridC, Risk: rates(gridC, 5000, func(l float64) float64 { return math.Floor(l * 500) })},
		// exactly the minimum n with zero loss: certifies the whole grid
		{N: 299, Alpha: 0.01, Delta: 0.05, Grid: gridA, Risk: zeros(gridA)},
		// enough samples but the most conservative lambda already misses too much
		{N: 2000, Alpha: 0.01, Delta: 0.05, Grid: gridA, Risk: firstFails},
	}
}

func crossMinNVectors() []crossMinN {
	return []crossMinN{
		{0.01, 0.05}, {0.05, 0.05}, {0.1, 0.1}, {0.02, 0.05}, {0.001, 0.05},
		{0.05, 0.01}, {0.005, 0.1}, {0.2, 0.001}, {0.3, 0.05}, {0.0001, 0.05},
	}
}

func crossCPVectors() []crossCP {
	return []crossCP{
		{0, 10, 0.95}, {1, 10, 0.95}, {3, 50, 0.9}, {5, 100, 0.99},
		{0, 100, 0.95}, {20, 1000, 0.95}, {2, 30, 0.8}, {7, 7000, 0.999},
	}
}

// findPython returns a working interpreter: HAZARD_TEST_PYTHON if set, else
// python3, else python. With none, the test skips unless HAZARD_REQUIRE_PYTEST
// is "1", in which case it fails.
func findPython(t *testing.T) string {
	t.Helper()
	candidates := []string{"python3", "python"}
	if p := os.Getenv("HAZARD_TEST_PYTHON"); p != "" {
		candidates = []string{p}
	}
	for _, c := range candidates {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		err := exec.CommandContext(ctx, c, "-c", "import sys").Run()
		cancel()
		if err == nil {
			return c
		}
	}
	if os.Getenv("HAZARD_REQUIRE_PYTEST") == "1" {
		t.Fatalf("no usable Python interpreter among %v and HAZARD_REQUIRE_PYTEST=1", candidates)
	}
	t.Skipf("no usable Python interpreter among %v", candidates)
	return ""
}

func TestCrossImplementationAgainstPython(t *testing.T) {
	py := findPython(t)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	researchDir := filepath.Join(wd, "..", "..", "research")
	if st, err := os.Stat(filepath.Join(researchDir, "hazard_research", "ltt_check.py")); err != nil || st.IsDir() {
		t.Fatalf("reference implementation missing under %s: %v", researchDir, err)
	}

	vectors := buildCrossVectors(t)
	checkCrossCoverage(t, vectors.HB)

	data, err := json.Marshal(vectors)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "vectors.json")
	if err := os.WriteFile(file, data, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, py, "-m", "hazard_research.ltt_check", "--vectors", file)
	cmd.Dir = researchDir
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1", "PYTHONIOENCODING=utf-8")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("python reference failed: %v\n%s", err, stderr.String())
	}
	var res crossResults
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		t.Fatalf("python output is not the expected JSON: %v\n%.500s", err, stdout.String())
	}
	if len(res.HB) != len(vectors.HB) || len(res.MinN) != len(vectors.MinN) || len(res.LTT) != len(vectors.LTT) || len(res.CP) != len(vectors.CP) {
		t.Fatalf("python returned %d/%d/%d/%d results for %d/%d/%d/%d vectors",
			len(res.HB), len(res.MinN), len(res.LTT), len(res.CP), len(vectors.HB), len(vectors.MinN), len(vectors.LTT), len(vectors.CP))
	}

	maxDiff := 0.0
	bad := 0
	fail := func(format string, args ...any) {
		bad++
		if bad <= 12 {
			t.Errorf(format, args...)
		}
	}
	note := func(d float64) { maxDiff = math.Max(maxDiff, d) }

	for i, v := range vectors.HB {
		got := HBPValue(v.N, v.Rhat, v.Alpha)
		want := res.HB[i].P
		if want == nil || math.IsNaN(got) {
			fail("hb[%d] %+v: go %v, python %v (NaN/null)", i, v, got, want)
			continue
		}
		d := math.Abs(got - *want)
		note(d)
		if d > crossTolerance {
			fail("hb[%d] %+v: go %.17g python %.17g differ by %.3g", i, v, got, *want, d)
		}
	}

	for i, v := range vectors.MinN {
		got := MinCalibrationN(v.Alpha, v.Delta)
		if want := res.MinN[i].N; want == nil || *want != got {
			fail("min_n[%d] %+v: go %d python %v", i, v, got, want)
		}
	}

	certified, refused := 0, 0
	for i, v := range vectors.LTT {
		idx := make(map[float64]int, len(v.Grid))
		for j, l := range v.Grid {
			idx[l] = j
		}
		got := LTT(v.N, func(l float64) float64 { return v.Risk[idx[l]] }, v.Grid, v.Alpha, v.Delta)
		want := res.LTT[i]
		if got.Refused {
			refused++
		} else {
			certified++
		}
		if got.Refused != want.Refused || got.Lambda != want.Lambda || got.HaveN != want.HaveN || want.NeedN == nil || got.NeedN != *want.NeedN {
			fail("ltt[%d]: go {lambda %v refused %v need %d have %d} python {lambda %v refused %v need %v have %d}",
				i, got.Lambda, got.Refused, got.NeedN, got.HaveN, want.Lambda, want.Refused, want.NeedN, want.HaveN)
			continue
		}
		if len(got.Points) != len(want.Points) {
			fail("ltt[%d]: go evaluated %d lambdas, python %d", i, len(got.Points), len(want.Points))
			continue
		}
		for j, gp := range got.Points {
			wp := want.Points[j]
			if wp.P == nil || gp.Lambda != wp.Lambda || gp.Risk != wp.Risk || gp.Rejected != wp.Rejected {
				fail("ltt[%d] point %d: go %+v python %+v", i, j, gp, wp)
				continue
			}
			d := math.Abs(gp.PValue - *wp.P)
			note(d)
			if d > crossTolerance {
				fail("ltt[%d] point %d (lambda %v): go p %.17g python p %.17g differ by %.3g", i, j, gp.Lambda, gp.PValue, *wp.P, d)
			}
		}
	}
	if certified < 3 || refused < 2 {
		t.Errorf("LTT vectors cover %d certified and %d refused grids, want at least 3 and 2", certified, refused)
	}

	for i, v := range vectors.CP {
		got := ClopperPearsonUpper(v.K, v.N, v.Confidence)
		d := math.Abs(got - res.CP[i].Upper)
		note(d)
		if d > crossTolerance {
			fail("cp[%d] %+v: go %.17g python %.17g differ by %.3g", i, v, got, res.CP[i].Upper, d)
		}
	}

	t.Logf("compared %d hb, %d min_n, %d ltt grids, %d cp vectors; max |go-python| = %.3g (tolerance %g)",
		len(vectors.HB), len(vectors.MinN), len(vectors.LTT), len(vectors.CP), maxDiff, crossTolerance)
	if bad > 12 {
		t.Errorf("... and %d more disagreements", bad-12)
	}
}

func buildCrossVectors(t *testing.T) crossVectors {
	t.Helper()
	return crossVectors{HB: crossHBVectors(), MinN: crossMinNVectors(), LTT: crossLTTVectors(t), CP: crossCPVectors()}
}

// checkCrossCoverage makes sure the vectors exercise both branches of the
// minimum and a wide range of n, so agreement is not trivial.
func checkCrossCoverage(t *testing.T, hb []crossHB) {
	t.Helper()
	if len(hb) < 60 {
		t.Fatalf("only %d hb vectors, want at least 60", len(hb))
	}
	expWins, binomWins, interior, zeroLoss, nearAlpha := 0, 0, 0, 0, 0
	minN, maxN := math.MaxInt, 0
	for _, v := range hb {
		minN, maxN = min(minN, v.N), max(maxN, v.N)
		if v.Rhat == 0 {
			zeroLoss++
		}
		if v.Rhat >= v.Alpha {
			continue
		}
		if v.Rhat > 0.999*v.Alpha {
			nearAlpha++
		}
		chernoff, binom := hbTerms(v.N, v.Rhat, v.Alpha)
		p := HBPValue(v.N, v.Rhat, v.Alpha)
		if p > 1e-6 && p < 1-1e-6 {
			interior++
			if chernoff < binom {
				expWins++
			} else {
				binomWins++
			}
		}
	}
	if expWins < 10 || binomWins < 10 {
		t.Errorf("interior vectors where the exp branch is the minimum: %d, the binomial branch: %d; want at least 10 of each", expWins, binomWins)
	}
	if interior < 50 || zeroLoss == 0 || nearAlpha == 0 || minN != 1 || maxN < 200000 {
		t.Errorf("coverage too thin: interior %d, zero-loss %d, near-alpha %d, n in [%d, %d]", interior, zeroLoss, nearAlpha, minN, maxN)
	}
}
