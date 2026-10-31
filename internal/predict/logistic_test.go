package predict

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"

	"github.com/omorsi45/hazard/internal/impact"
)

// gauss approximates a standard normal draw from twelve uniforms.
func (l *lcg) gauss() float64 {
	s := -6.0
	for i := 0; i < 12; i++ {
		s += l.next()
	}
	return s
}

// clusters builds two overlapping 2-feature clusters, positives around posC
// and negatives around negC, every row weight 1, one group per row.
func clusters(g *lcg, nPos, nNeg int, posC, negC [2]float64, prefix string) []Example {
	out := make([]Example, 0, nPos+nNeg)
	for i := 0; i < nPos; i++ {
		out = append(out, Example{
			Features: []float64{posC[0] + g.gauss(), posC[1] + g.gauss()},
			Label:    true, Weight: 1, Group: fmt.Sprintf("%s-p%03d", prefix, i),
		})
	}
	for i := 0; i < nNeg; i++ {
		out = append(out, Example{
			Features: []float64{negC[0] + g.gauss(), negC[1] + g.gauss()},
			Label:    false, Weight: 1, Group: fmt.Sprintf("%s-n%03d", prefix, i),
		})
	}
	return out
}

func norm(v []float64) float64 {
	var s float64
	for _, x := range v {
		s += x * x
	}
	return math.Sqrt(s)
}

// separable returns a fit and a scale set where feature 0 raises and feature 1
// lowers the chance of a positive label. The cluster centres sit at +-0.5 so
// that the classes overlap and the fit has a finite optimum.
func separable() (fit, scale []Example) {
	g := lcg(11)
	return clusters(&g, 100, 100, [2]float64{0.5, -0.5}, [2]float64{-0.5, 0.5}, "f"),
		clusters(&g, 50, 50, [2]float64{0.5, -0.5}, [2]float64{-0.5, 0.5}, "s")
}

func TestLogisticDefaultOptions(t *testing.T) {
	want := LogisticOptions{L2: 1, CIWeight: 10, MaxIter: 5000, Tol: 1e-9}
	if got := DefaultLogisticOptions(); got != want {
		t.Errorf("DefaultLogisticOptions() = %+v, want %+v", got, want)
	}
}

func TestLogisticSigmoid(t *testing.T) {
	cases := []struct{ in, want float64 }{
		{0, 0.5},
		{2, 0.8807970779778823},
		{-2, 0.11920292202211757},
		{800, 1},
		{-800, 0},
	}
	for _, tc := range cases {
		if got := sigmoid(tc.in); !approx(got, tc.want, 1e-15) {
			t.Errorf("sigmoid(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
	if got := sigmoid(math.NaN()); !math.IsNaN(got) {
		t.Errorf("sigmoid(NaN) = %v, want NaN (unknown is never 0)", got)
	}
}

func TestLogisticExampleWeights(t *testing.T) {
	ex := []Example{
		{Features: []float64{1}, Label: true, Weight: 10},
		{Features: []float64{1}, Label: true, Weight: 1},
	}
	for i := 0; i < 6; i++ {
		ex = append(ex, Example{Features: []float64{0}, Label: false, Weight: 1})
	}
	// n=8, 2 positives, 6 negatives: class weights 8/(2*2)=2 and 8/(2*6)=2/3.
	got, err := exampleWeights("fit", ex)
	if err != nil {
		t.Fatalf("exampleWeights: %v", err)
	}
	want := []float64{20, 2, 2.0 / 3, 2.0 / 3, 2.0 / 3, 2.0 / 3, 2.0 / 3, 2.0 / 3}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if !approx(got[i], want[i], 1e-12) {
			t.Fatalf("weights = %v, want %v", got, want)
		}
	}
}

func TestLogisticExampleWeightsRefuseOneClass(t *testing.T) {
	pos := []Example{{Features: []float64{1}, Label: true, Weight: 1}}
	neg := []Example{{Features: []float64{1}, Label: false, Weight: 1}}
	if _, err := exampleWeights("fit", pos); err == nil {
		t.Error("accepted a set with no negatives")
	}
	if _, err := exampleWeights("fit", neg); err == nil {
		t.Error("accepted a set with no positives")
	}
}

func TestLogisticStandardize(t *testing.T) {
	ex := []Example{
		{Features: []float64{1, 5, 0.1}},
		{Features: []float64{3, 5, 0.1}},
		{Features: []float64{5, 5, 0.1}},
	}
	means, scales, x, err := standardize(ex)
	if err != nil {
		t.Fatalf("standardize: %v", err)
	}
	if len(means) != 3 || len(scales) != 3 || len(x) != 3 {
		t.Fatalf("standardize returned %d means, %d scales, %d rows, want 3 each", len(means), len(scales), len(x))
	}
	for i := range x {
		if len(x[i]) != 3 {
			t.Fatalf("row %d has %d columns, want 3", i, len(x[i]))
		}
	}
	std0 := math.Sqrt(8.0 / 3) // population std of 1, 3, 5
	if !approx(means[0], 3, 1e-12) || !approx(scales[0], std0, 1e-12) {
		t.Errorf("column 0: mean %v scale %v, want 3 and %v", means[0], scales[0], std0)
	}
	// A constant column keeps its exact value as the mean and gets scale 1,
	// even when summing it would not reproduce the value bit for bit.
	if means[1] != 5 || scales[1] != 1 || means[2] != 0.1 || scales[2] != 1 {
		t.Errorf("constant columns: means %v scales %v, want [_ 5 0.1] and [_ 1 1]", means, scales)
	}
	for i := range x {
		if x[i][1] != 0 || x[i][2] != 0 {
			t.Errorf("row %d constant columns standardize to %v, want exactly 0", i, x[i])
		}
	}
	if !approx(x[0][0], -2/std0, 1e-12) || !approx(x[2][0], 2/std0, 1e-12) || x[1][0] != 0 {
		t.Errorf("column 0 standardized = %v %v %v", x[0][0], x[1][0], x[2][0])
	}
}

func TestLogisticStandardizeRejectsOverflow(t *testing.T) {
	ex := []Example{
		{Features: []float64{1.7e308}}, {Features: []float64{1.7e308}}, {Features: []float64{-1.7e308}},
	}
	if _, _, _, err := standardize(ex); err == nil {
		t.Error("standardize accepted features whose statistics overflow")
	}
}

func TestLogisticFitConvergesWithSignedWeights(t *testing.T) {
	fit, scale := separable()
	opts := DefaultLogisticOptions()
	m, err := FitLogistic(fit, scale, opts)
	if err != nil {
		t.Fatalf("FitLogistic: %v", err)
	}
	if !m.Converged {
		t.Fatalf("did not converge in %d iterations (MaxIter %d)", m.Iterations, opts.MaxIter)
	}
	if m.Iterations <= 0 || m.Iterations > opts.MaxIter {
		t.Errorf("Iterations = %d, want within (0, %d]", m.Iterations, opts.MaxIter)
	}
	if m.FeatureVersion != impact.FeatureVersion {
		t.Errorf("FeatureVersion = %q, want %q", m.FeatureVersion, impact.FeatureVersion)
	}
	if !(m.Weights[0] > 0) || !(m.Weights[1] < 0) {
		t.Errorf("weights = %v, want positive then negative", m.Weights)
	}
	if len(m.Means) != 2 || len(m.Scales) != 2 || !(m.Scales[0] > 0) || !(m.Scales[1] > 0) {
		t.Errorf("means %v scales %v", m.Means, m.Scales)
	}
	hi, err := m.Prob([]float64{2, -2})
	if err != nil {
		t.Fatalf("Prob: %v", err)
	}
	lo, err := m.Prob([]float64{-2, 2})
	if err != nil {
		t.Fatalf("Prob: %v", err)
	}
	if !(hi > 0.5 && hi < 1) || !(lo > 0 && lo < 0.5) || !(hi > lo) {
		t.Errorf("Prob high=%v low=%v, want 0.5<high<1 and 0<low<0.5", hi, lo)
	}
	if err := m.validate(); err != nil {
		t.Errorf("fitted model fails validate: %v", err)
	}
}

func TestLogisticFitStopsAtMaxIterAndReportsIt(t *testing.T) {
	fit, scale := separable()
	opts := DefaultLogisticOptions()
	opts.MaxIter = 3
	m, err := FitLogistic(fit, scale, opts)
	if err != nil {
		t.Fatalf("FitLogistic: %v", err)
	}
	if m.Converged {
		t.Error("Converged = true after 3 iterations")
	}
	if m.Iterations != 3 {
		t.Errorf("Iterations = %d, want 3", m.Iterations)
	}
}

func TestLogisticL2ShrinksWeightsMonotonically(t *testing.T) {
	fit, scale := separable()
	prev := math.Inf(1)
	for _, l2 := range []float64{0.01, 1, 100} {
		opts := DefaultLogisticOptions()
		opts.L2 = l2
		opts.MaxIter = 50000
		m, err := FitLogistic(fit, scale, opts)
		if err != nil {
			t.Fatalf("L2=%v: %v", l2, err)
		}
		// Iterations below the cap means the coefficient fit met the
		// tolerance.
		if m.Iterations >= opts.MaxIter {
			t.Fatalf("L2=%v coefficient fit hit the %d iteration cap", l2, opts.MaxIter)
		}
		n := norm(m.Weights)
		if !(n < prev) {
			t.Errorf("L2=%v: ||w||=%v, want below %v", l2, n, prev)
		}
		prev = n
	}
}

func TestLogisticClassWeightingRaisesMinorityRecall(t *testing.T) {
	g := lcg(7)
	fit := clusters(&g, 30, 600, [2]float64{1.5, 1.5}, [2]float64{0, 0}, "f")
	scale := clusters(&g, 15, 300, [2]float64{1.5, 1.5}, [2]float64{0, 0}, "s")
	opts := DefaultLogisticOptions()
	m, err := FitLogistic(fit, scale, opts)
	if err != nil {
		t.Fatalf("FitLogistic: %v", err)
	}
	// Raw logit sign decides the class so Platt scaling plays no part.
	recall := func(logit func(f []float64) float64) float64 {
		var hit, pos int
		for _, e := range fit {
			if !e.Label {
				continue
			}
			pos++
			if logit(e.Features) > 0 {
				hit++
			}
		}
		return float64(hit) / float64(pos)
	}
	weighted := recall(func(f []float64) float64 { return m.logit(f) })

	means, scales, x, err := standardize(fit)
	if err != nil {
		t.Fatalf("standardize: %v", err)
	}
	w, b, _, _, err := descend(logisticProblem{x: x, y: labelTargets(exampleLabels(fit)), w: ones(len(fit)), l2: opts.L2}, make([]float64, 2), 0, opts)
	if err != nil {
		t.Fatalf("descend: %v", err)
	}
	if len(w) != 2 || len(means) != 2 || len(scales) != 2 {
		t.Fatalf("descend returned %d weights, standardize %d means and %d scales, want 2 each", len(w), len(means), len(scales))
	}
	unweighted := recall(func(f []float64) float64 {
		z := b
		for j := range f {
			z += w[j] * (f[j] - means[j]) / scales[j]
		}
		return z
	})
	if !(weighted >= 0.6) || !(weighted > unweighted+0.2) {
		t.Errorf("minority recall weighted=%.3f unweighted=%.3f, want weighted >= 0.6 and 0.2 above unweighted", weighted, unweighted)
	}
}

func TestLogisticFitHonoursExampleWeight(t *testing.T) {
	// Up-weighting the positives that disagree with the bulk must pull the
	// fitted weight on feature 0 up compared with weight 1 on every row.
	g := lcg(3)
	base := clusters(&g, 80, 80, [2]float64{0.3, 0}, [2]float64{-0.3, 0}, "f")
	scale := clusters(&g, 40, 40, [2]float64{0.3, 0}, [2]float64{-0.3, 0}, "s")
	heavy := make([]Example, len(base))
	copy(heavy, base)
	for i := range heavy {
		if heavy[i].Label && heavy[i].Features[0] > 0.3 {
			heavy[i].Weight = 10
		}
	}
	opts := DefaultLogisticOptions()
	m1, err := FitLogistic(base, scale, opts)
	if err != nil {
		t.Fatal(err)
	}
	m2, err := FitLogistic(heavy, scale, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(m1.Weights) != 2 || len(m2.Weights) != 2 {
		t.Fatalf("weights = %v and %v, want 2 each", m1.Weights, m2.Weights)
	}
	if !(m2.Weights[0] > m1.Weights[0]) {
		t.Errorf("weight on feature 0 with up-weighted rows = %v, want above %v", m2.Weights[0], m1.Weights[0])
	}
}

// calibratedGrid returns a logit grid where P(y=1|z)=sigmoid(z) holds exactly:
// each z carries one positive row of weight sigmoid(z) and one negative row of
// weight 1-sigmoid(z), so the class counts are equal and class weights are 1.
// The weights are scaled by a million so the weighted class counts are large
// and the Platt target smoothing (which moves targets by about 1/(N+2)) is far
// below the 1e-3 tolerance of the recovery test.
func calibratedGrid(transform func(z float64) float64) (z []float64, y []bool, w []float64) {
	const mass = 1e6
	for k := -8; k <= 8; k++ {
		raw := float64(k) / 2
		p := sigmoid(raw)
		z = append(z, transform(raw), transform(raw))
		y = append(y, true, false)
		w = append(w, mass*p, mass*(1-p))
	}
	return z, y, w
}

func TestLogisticPlattRecoversCalibration(t *testing.T) {
	opts := DefaultLogisticOptions()
	opts.MaxIter = 20000
	cases := []struct {
		name         string
		transform    func(float64) float64
		wantA, wantB float64
	}{
		{"already calibrated", func(z float64) float64 { return z }, 1, 0},
		{"overconfident logits", func(z float64) float64 { return 2 * z }, 0.5, 0},
		{"shifted logits", func(z float64) float64 { return z + 1 }, 1, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			z, y, w := calibratedGrid(tc.transform)
			a, b, _, conv, err := fitPlatt(z, y, w, opts)
			if err != nil {
				t.Fatalf("fitPlatt: %v", err)
			}
			if !conv {
				t.Error("fitPlatt did not converge")
			}
			if !approx(a, tc.wantA, 1e-3) || !approx(b, tc.wantB, 1e-3) {
				t.Errorf("a=%v b=%v, want a=%v b=%v", a, b, tc.wantA, tc.wantB)
			}
		})
	}
}

func TestLogisticPlattRefusals(t *testing.T) {
	opts := DefaultLogisticOptions()
	nan, inf := math.NaN(), math.Inf(1)
	cases := []struct {
		name string
		z    []float64
		y    []bool
		w    []float64
	}{
		{"empty", nil, nil, nil},
		{"labels shorter", []float64{0, 1}, []bool{true}, []float64{1, 1}},
		{"weights shorter", []float64{0, 1}, []bool{true, false}, []float64{1}},
		{"NaN logit", []float64{nan, 1}, []bool{true, false}, []float64{1, 1}},
		{"Inf logit", []float64{inf, 1}, []bool{true, false}, []float64{1, 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, _, _, err := fitPlatt(tc.z, tc.y, tc.w, opts); err == nil {
				t.Fatal("fitPlatt returned no error")
			}
		})
	}
}

// separatedScale is a scale set the fitted model separates perfectly: ten
// positives at (k, -k) and ten negatives at (-k, k). The unpenalized maximum
// likelihood slope of the Platt fit would be infinite; the smoothed Platt
// targets give it a finite optimum.
func separatedScale(k float64) []Example {
	var out []Example
	for i := 0; i < 10; i++ {
		out = append(out,
			Example{Features: []float64{k, -k}, Label: true, Weight: 1, Group: fmt.Sprintf("x-p%02d", i)},
			Example{Features: []float64{-k, k}, Label: false, Weight: 1, Group: fmt.Sprintf("x-n%02d", i)})
	}
	return out
}

func extremeScale() []Example { return separatedScale(3) }

func TestLogisticSeparableScaleSetConverges(t *testing.T) {
	fit, _ := separable()
	m, err := FitLogistic(fit, extremeScale(), DefaultLogisticOptions())
	if err != nil {
		t.Fatalf("FitLogistic with a separable scale set: %v", err)
	}
	if !m.Converged {
		t.Error("Converged = false although the smoothed Platt targets have a finite optimum on a separable scale set")
	}
	if !isFinite(m.PlattA) || !isFinite(m.PlattB) || !(m.PlattA > 0) {
		t.Errorf("platt a=%v b=%v, want finite with a positive slope", m.PlattA, m.PlattB)
	}
}

// TestLogisticPlattSurvivesSaturatedLogits: scale rows far outside the fit
// give raw logits of tens to hundreds, where the sigmoid is flat and a
// Newton solve started at slope 1 has no usable curvature.
func TestLogisticPlattSurvivesSaturatedLogits(t *testing.T) {
	fit, _ := separable()
	for _, k := range []float64{30, 100} {
		t.Run(fmt.Sprintf("features at +-%v", k), func(t *testing.T) {
			scale := separatedScale(k)
			opts := DefaultLogisticOptions()
			m, err := FitLogistic(fit, scale, opts)
			if err != nil {
				t.Fatalf("FitLogistic: %v", err)
			}
			if !m.Converged {
				t.Fatalf("Converged = false (platt a=%v b=%v)", m.PlattA, m.PlattB)
			}
			logits := make([]float64, len(scale))
			y := make([]bool, len(scale))
			for i, e := range scale {
				logits[i], y[i] = m.logit(e.Features), e.Label
			}
			ga, gb := refPlattGradient(logits, y, refWeights(scale), m.PlattA, m.PlattB)
			if math.Abs(ga) >= opts.Tol || math.Abs(gb) >= opts.Tol {
				t.Errorf("platt gradient (%g, %g) at a=%v b=%v, want below %g in both components", ga, gb, m.PlattA, m.PlattB, opts.Tol)
			}
		})
	}
}

func TestLogisticConvergedNeedsBothStages(t *testing.T) {
	fit, scale := separable()
	opts := DefaultLogisticOptions()
	base, err := FitLogistic(fit, scale, opts)
	if err != nil {
		t.Fatalf("FitLogistic: %v", err)
	}
	if !base.Converged {
		t.Fatalf("baseline fit did not converge in %d iterations", base.Iterations)
	}
	// The Platt fit on this scale set converges, but needs more iterations
	// than the coefficient fit.
	wide := separatedScale(10)
	logits := make([]float64, len(wide))
	for i, e := range wide {
		logits[i] = base.logit(e.Features)
	}
	_, _, plattIters, plattConverged, err := fitPlatt(logits, exampleLabels(wide), ones(len(wide)), opts)
	if err != nil || !plattConverged || plattIters <= base.Iterations {
		t.Fatalf("fixture drifted: platt fit took %d iterations (converged %v, err %v), want more than the %d of the coefficient fit", plattIters, plattConverged, err, base.Iterations)
	}
	// A cap equal to the coefficient fit's own iteration count lets it
	// converge exactly at the cap; the Platt fit then runs out of iterations.
	opts.MaxIter = base.Iterations
	capped, err := FitLogistic(fit, wide, opts)
	if err != nil {
		t.Fatalf("FitLogistic: %v", err)
	}
	if capped.Iterations != base.Iterations || !reflect.DeepEqual(capped.Weights, base.Weights) {
		t.Fatalf("coefficient fit differs from the baseline: %d vs %d iterations", capped.Iterations, base.Iterations)
	}
	if capped.Converged {
		t.Error("Converged = true although the Platt fit hit the iteration cap")
	}
}

func TestLogisticFitRefusals(t *testing.T) {
	fit, scale := separable()
	onlyPos := func(in []Example) []Example {
		var out []Example
		for _, e := range in {
			if e.Label {
				out = append(out, e)
			}
		}
		return out
	}
	onlyNeg := func(in []Example) []Example {
		var out []Example
		for _, e := range in {
			if !e.Label {
				out = append(out, e)
			}
		}
		return out
	}
	with := func(in []Example, mut func(e *Example)) []Example {
		out := make([]Example, len(in))
		for i, e := range in {
			e.Features = append([]float64(nil), e.Features...)
			out[i] = e
		}
		mut(&out[0])
		return out
	}
	badOpts := func(mut func(o *LogisticOptions)) LogisticOptions {
		o := DefaultLogisticOptions()
		mut(&o)
		return o
	}
	def := DefaultLogisticOptions()
	cases := []struct {
		name       string
		fit, scale []Example
		opts       LogisticOptions
	}{
		{"empty fit", nil, scale, def},
		{"fit without positives", onlyNeg(fit), scale, def},
		{"fit without negatives", onlyPos(fit), scale, def},
		{"empty scale", fit, nil, def},
		{"scale without positives", fit, onlyNeg(scale), def},
		{"scale without negatives", fit, onlyPos(scale), def},
		{"NaN feature in fit", with(fit, func(e *Example) { e.Features[0] = math.NaN() }), scale, def},
		{"Inf feature in scale", fit, with(scale, func(e *Example) { e.Features[1] = math.Inf(1) }), def},
		{"ragged fit row", with(fit, func(e *Example) { e.Features = e.Features[:1] }), scale, def},
		{"scale width differs", fit, with(scale, func(e *Example) { e.Features = append(e.Features, 0) }), def},
		{"zero feature width", with(fit, func(e *Example) { e.Features = nil }), scale, def},
		{"zero example weight", with(fit, func(e *Example) { e.Weight = 0 }), scale, def},
		{"NaN example weight", fit, with(scale, func(e *Example) { e.Weight = math.NaN() }), def},
		{"negative L2", fit, scale, badOpts(func(o *LogisticOptions) { o.L2 = -1 })},
		{"NaN L2", fit, scale, badOpts(func(o *LogisticOptions) { o.L2 = math.NaN() })},
		{"zero MaxIter", fit, scale, badOpts(func(o *LogisticOptions) { o.MaxIter = 0 })},
		{"zero Tol", fit, scale, badOpts(func(o *LogisticOptions) { o.Tol = 0 })},
		{"non-positive CIWeight", fit, scale, badOpts(func(o *LogisticOptions) { o.CIWeight = 0 })},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := FitLogistic(tc.fit, tc.scale, tc.opts); err == nil {
				t.Fatal("FitLogistic returned no error")
			}
		})
	}
}

func TestLogisticFitIsDeterministicAndPure(t *testing.T) {
	fit, scale := separable()
	snapshot, err := json.Marshal([2][]Example{fit, scale})
	if err != nil {
		t.Fatal(err)
	}
	opts := DefaultLogisticOptions()
	m1, err := FitLogistic(fit, scale, opts)
	if err != nil {
		t.Fatal(err)
	}
	m2, err := FitLogistic(fit, scale, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(m1.Weights) != 2 {
		t.Fatalf("weights = %v, want 2", m1.Weights)
	}
	b1, _ := json.Marshal(m1)
	b2, _ := json.Marshal(m2)
	if !reflect.DeepEqual(m1, m2) || string(b1) != string(b2) {
		t.Errorf("two fits differ:\n%s\n%s", b1, b2)
	}
	after, err := json.Marshal([2][]Example{fit, scale})
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(snapshot) {
		t.Error("FitLogistic modified its input examples")
	}
}

func handModel() LogisticModel {
	return LogisticModel{
		FeatureVersion: impact.FeatureVersion,
		Means:          []float64{1, 2},
		Scales:         []float64{2, 4},
		Weights:        []float64{1, -1},
		Bias:           0.5,
		PlattA:         2,
		PlattB:         -1,
		Iterations:     10,
		Converged:      true,
	}
}

func TestLogisticProbKnownVector(t *testing.T) {
	// standardized (3,2) = (1, 0); z = 0.5 + 1*1 - 1*0 = 1.5; t = 2*1.5 - 1 = 2.
	got, err := handModel().Prob([]float64{3, 2})
	if err != nil {
		t.Fatalf("Prob: %v", err)
	}
	if want := 0.8807970779778823; !approx(got, want, 1e-12) {
		t.Errorf("Prob = %v, want %v", got, want)
	}
}

func TestLogisticProbRefusesUnknown(t *testing.T) {
	cases := []struct {
		name string
		m    LogisticModel
		f    []float64
	}{
		{"NaN feature", handModel(), []float64{math.NaN(), 0}},
		{"Inf feature", handModel(), []float64{0, math.Inf(1)}},
		{"too few features", handModel(), []float64{1}},
		{"too many features", handModel(), []float64{1, 2, 3}},
		{"no features", handModel(), nil},
		{
			// Both standardized values overflow to +Inf and the weights
			// cancel them to NaN: the probability is unknown, never 0.
			name: "NaN probability",
			m: LogisticModel{
				FeatureVersion: impact.FeatureVersion,
				Means:          []float64{0, 0}, Scales: []float64{1e-300, 1e-300},
				Weights: []float64{1, -1}, PlattA: 1,
			},
			f: []float64{1e300, 1e300},
		},
		{
			name: "invalid model scale",
			m: func() LogisticModel {
				m := handModel()
				m.Scales[0] = 0
				return m
			}(),
			f: []float64{1, 2},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := tc.m.Prob(tc.f)
			if err == nil {
				t.Fatalf("Prob = %v with no error", p)
			}
		})
	}
}

func TestLogisticModelValidate(t *testing.T) {
	nan := math.NaN()
	mut := func(f func(m *LogisticModel)) LogisticModel {
		m := handModel()
		m.Means = append([]float64(nil), m.Means...)
		m.Scales = append([]float64(nil), m.Scales...)
		m.Weights = append([]float64(nil), m.Weights...)
		f(&m)
		return m
	}
	if err := handModel().validate(); err != nil {
		t.Fatalf("valid model rejected: %v", err)
	}
	bad := map[string]LogisticModel{
		"wrong feature version": mut(func(m *LogisticModel) { m.FeatureVersion = "f1" }),
		"no weights":            mut(func(m *LogisticModel) { m.Weights, m.Means, m.Scales = nil, nil, nil }),
		"means length":          mut(func(m *LogisticModel) { m.Means = m.Means[:1] }),
		"scales length":         mut(func(m *LogisticModel) { m.Scales = m.Scales[:1] }),
		"zero scale":            mut(func(m *LogisticModel) { m.Scales[1] = 0 }),
		"negative scale":        mut(func(m *LogisticModel) { m.Scales[0] = -1 }),
		"NaN mean":              mut(func(m *LogisticModel) { m.Means[0] = nan }),
		"NaN weight":            mut(func(m *LogisticModel) { m.Weights[1] = nan }),
		"Inf bias":              mut(func(m *LogisticModel) { m.Bias = math.Inf(1) }),
		"NaN platt a":           mut(func(m *LogisticModel) { m.PlattA = nan }),
		"Inf platt b":           mut(func(m *LogisticModel) { m.PlattB = math.Inf(-1) }),
		"negative iterations":   mut(func(m *LogisticModel) { m.Iterations = -1 }),
	}
	for name, m := range bad {
		t.Run(name, func(t *testing.T) {
			if err := m.validate(); err == nil {
				t.Error("validate accepted an invalid model")
			}
		})
	}
}

// refLoss is the log loss log(1+exp(z)) - t*z of logit z against a target t
// in [0, 1], without overflow for large z.
func refLoss(z, t float64) float64 {
	if z > 30 {
		return z + math.Log1p(math.Exp(-z)) - t*z
	}
	return math.Log1p(math.Exp(z)) - t*z
}

func refSigmoid(z float64) float64 { return 1 / (1 + math.Exp(-z)) }

// refWeights are the per-row weights of a set: Example.Weight times the class
// weight n/(2*positives) or n/(2*negatives). Written out here so the checks
// below do not depend on the code under test.
func refWeights(ex []Example) []float64 {
	var pos, neg float64
	for _, e := range ex {
		if e.Label {
			pos++
		} else {
			neg++
		}
	}
	n := float64(len(ex))
	out := make([]float64, len(ex))
	for i, e := range ex {
		class := n / (2 * neg)
		if e.Label {
			class = n / (2 * pos)
		}
		out[i] = class * e.Weight
	}
	return out
}

func refTargets(ex []Example) []float64 {
	out := make([]float64, len(ex))
	for i, e := range ex {
		if e.Label {
			out[i] = 1
		}
	}
	return out
}

// refStandardize maps the raw features of ex through the model's recorded
// means and scales.
func refStandardize(m LogisticModel, ex []Example) [][]float64 {
	out := make([][]float64, len(ex))
	for i, e := range ex {
		row := make([]float64, len(e.Features))
		for j, v := range e.Features {
			row[j] = (v - m.Means[j]) / m.Scales[j]
		}
		out[i] = row
	}
	return out
}

// refProblem evaluates the penalized objective of the logistic fit on
// standardized rows x: sum(wt*loss)/sum(wt) + l2/(2n)*||w||^2, the bias free.
type refProblem struct {
	x          [][]float64
	target, wt []float64
	l2         float64
}

func newRefProblem(m LogisticModel, ex []Example, l2 float64) refProblem {
	return refProblem{x: refStandardize(m, ex), target: refTargets(ex), wt: refWeights(ex), l2: l2}
}

func (p refProblem) objective(w []float64, b float64) float64 {
	var num, den, sq float64
	for i, row := range p.x {
		z := b
		for j, v := range row {
			z += w[j] * v
		}
		num += p.wt[i] * refLoss(z, p.target[i])
		den += p.wt[i]
	}
	for _, v := range w {
		sq += v * v
	}
	return num/den + p.l2/float64(len(p.x))*sq/2
}

// gradient returns the derivative of objective with respect to each weight
// and to the bias.
func (p refProblem) gradient(w []float64, b float64) (gw []float64, gb float64) {
	gw = make([]float64, len(w))
	var den float64
	for i, row := range p.x {
		z := b
		for j, v := range row {
			z += w[j] * v
		}
		r := p.wt[i] * (refSigmoid(z) - p.target[i])
		gb += r
		for j, v := range row {
			gw[j] += r * v
		}
		den += p.wt[i]
	}
	gb /= den
	for j := range gw {
		gw[j] = gw[j]/den + p.l2/float64(len(p.x))*w[j]
	}
	return gw, gb
}

func (p refProblem) worstGradient(w []float64, b float64) float64 {
	gw, gb := p.gradient(w, b)
	worst := math.Abs(gb)
	for _, g := range gw {
		worst = math.Max(worst, math.Abs(g))
	}
	return worst
}

// refPlattGradient is the gradient of the weighted mean log loss of
// sigmoid(a*z+b) against the Platt targets (N+ + 1)/(N+ + 2) for positives and
// 1/(N- + 2) for negatives, N+ and N- the weighted class counts.
func refPlattGradient(z []float64, y []bool, w []float64, a, b float64) (ga, gb float64) {
	var pos, neg, total float64
	for i := range z {
		if y[i] {
			pos += w[i]
		} else {
			neg += w[i]
		}
		total += w[i]
	}
	hi, lo := (pos+1)/(pos+2), 1/(neg+2)
	for i := range z {
		target := lo
		if y[i] {
			target = hi
		}
		r := w[i] * (refSigmoid(a*z[i]+b) - target)
		ga += r * z[i]
		gb += r
	}
	return ga / total, gb / total
}

// rareShape returns n rows of 25 raw features on very different scales and
// offsets whose label depends on the first four and is rare, as in a real
// training run (a fraction of a percent of positives).
func rareShape(n int, seed uint64) []Example {
	r := rand.New(rand.NewPCG(seed, seed+1))
	out := make([]Example, n)
	for i := range out {
		f := make([]float64, 25)
		z := make([]float64, 25)
		for j := range f {
			z[j] = r.NormFloat64()
			f[j] = float64(3*j) + float64(1+j%5)*z[j]
		}
		logit := -9 + 1.1*z[0] + 0.9*z[1] - 0.7*z[2] + 0.5*z[3]
		out[i] = Example{Features: f, Label: r.Float64() < refSigmoid(logit), Weight: 1, Group: fmt.Sprintf("r%d-%06d", seed, i)}
	}
	return out
}

func positives(ex []Example) int {
	var n int
	for _, e := range ex {
		if e.Label {
			n++
		}
	}
	return n
}

// rareProblem returns the fit and scale sets of the real-data shape: at least
// 20000 fit rows, fewer than 0.1% of them positive, a scale set with a few.
func rareProblem(t *testing.T) (fit, scale []Example) {
	t.Helper()
	fit, scale = rareShape(50000, 1), rareShape(8000, 3)
	if pos := positives(fit); pos < 5 || pos*1000 >= len(fit) {
		t.Fatalf("fixture drifted: %d positives of %d fit rows, want at least 5 and under 0.1%%", pos, len(fit))
	}
	if pos := positives(scale); pos < 1 {
		t.Fatalf("fixture drifted: %d positives of %d scale rows, want at least 1", pos, len(scale))
	}
	return fit, scale
}

func TestLogisticFitConvergesOnRareLabels(t *testing.T) {
	fit, scale := rareProblem(t)
	opts := DefaultLogisticOptions()
	m, err := FitLogistic(fit, scale, opts)
	if err != nil {
		t.Fatalf("FitLogistic: %v", err)
	}
	if !m.Converged {
		t.Fatalf("did not converge in %d iterations (MaxIter %d)", m.Iterations, opts.MaxIter)
	}
	p := newRefProblem(m, fit, opts.L2)
	if worst := p.worstGradient(m.Weights, m.Bias); !(worst < opts.Tol) {
		t.Errorf("largest gradient component %g at the returned weights, want below %g", worst, opts.Tol)
	}
}

func TestLogisticFitReportsAnUnfinishedFit(t *testing.T) {
	fit, scale := rareProblem(t)
	opts := DefaultLogisticOptions()
	opts.MaxIter = 1
	m, err := FitLogistic(fit, scale, opts)
	if err != nil {
		t.Fatalf("FitLogistic: %v", err)
	}
	if m.Converged {
		t.Error("Converged = true after one iteration of a 25 feature fit")
	}
	if m.Iterations != 1 {
		t.Errorf("Iterations = %d, want 1", m.Iterations)
	}
}

func TestLogisticFitIsTheMinimumOfThePenalizedObjective(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 8))
	draw := func(n int, prefix string) []Example {
		out := make([]Example, n)
		for i := range out {
			f := []float64{r.NormFloat64(), 5 + 4*r.NormFloat64(), r.NormFloat64()}
			logit := -1.2 + 1.0*f[0] - 0.5*(f[1]-5)/4 + 0.2*f[2]
			w := 1.0
			if i%5 == 0 {
				w = 3
			}
			out[i] = Example{Features: f, Label: r.Float64() < refSigmoid(logit), Weight: w, Group: fmt.Sprintf("%s%03d", prefix, i)}
		}
		return out
	}
	fit, scale := draw(200, "f"), draw(100, "s")
	for _, l2 := range []float64{0.5, 1, 20} {
		t.Run(fmt.Sprintf("L2=%v", l2), func(t *testing.T) {
			opts := DefaultLogisticOptions()
			opts.L2 = l2
			m, err := FitLogistic(fit, scale, opts)
			if err != nil {
				t.Fatalf("FitLogistic: %v", err)
			}
			if !m.Converged {
				t.Fatalf("did not converge in %d iterations", m.Iterations)
			}
			p := newRefProblem(m, fit, l2)
			gw, gb := p.gradient(m.Weights, m.Bias)
			for j, g := range gw {
				if math.Abs(g) >= 1e-9 {
					t.Errorf("gradient for weight %d = %g, want below 1e-9", j, g)
				}
			}
			if math.Abs(gb) >= 1e-9 {
				t.Errorf("gradient for the bias = %g, want below 1e-9", gb)
			}
			base := p.objective(m.Weights, m.Bias)
			const step = 1e-4
			for j := range m.Weights {
				for _, sign := range []float64{1, -1} {
					w := append([]float64(nil), m.Weights...)
					w[j] += sign * step
					if got := p.objective(w, m.Bias); !(got > base) {
						t.Errorf("weight %d moved by %+g: objective %.17g, want above %.17g", j, sign*step, got, base)
					}
				}
			}
			for _, sign := range []float64{1, -1} {
				if got := p.objective(m.Weights, m.Bias+sign*step); !(got > base) {
					t.Errorf("bias moved by %+g: objective %.17g, want above %.17g", sign*step, got, base)
				}
			}
		})
	}
}

func TestLogisticPlattFitsTheSmoothedTargets(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6))
	overlap := func(nPos, nNeg int) (z []float64, y []bool) {
		for i := 0; i < nPos; i++ {
			z, y = append(z, 0.7+r.NormFloat64()), append(y, true)
		}
		for i := 0; i < nNeg; i++ {
			z, y = append(z, -0.7+r.NormFloat64()), append(y, false)
		}
		return z, y
	}
	balancedZ, balancedY := overlap(40, 40)
	skewZ, skewY := overlap(3, 7)
	// One positive whose logit lies above every negative: the unpenalized
	// maximum likelihood slope is infinite. Class weights as FitLogistic
	// gives them: 10/(2*1) for the positive, 10/(2*9) for each negative.
	loneZ := []float64{4, -4, -3.5, -3, -2.5, -2, -1.5, -1, -0.5, -0.2}
	loneY := []bool{true, false, false, false, false, false, false, false, false, false}
	loneW := []float64{5}
	for i := 0; i < 9; i++ {
		loneW = append(loneW, 10.0/18)
	}
	cases := []struct {
		name string
		z    []float64
		y    []bool
		w    []float64
	}{
		{"balanced scale set", balancedZ, balancedY, ones(len(balancedZ))},
		{"three positives of ten", skewZ, skewY, ones(len(skewZ))},
		{"one positive, perfectly separated", loneZ, loneY, loneW},
	}
	opts := DefaultLogisticOptions()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, b, _, conv, err := fitPlatt(tc.z, tc.y, tc.w, opts)
			if err != nil {
				t.Fatalf("fitPlatt: %v", err)
			}
			if !conv {
				t.Fatalf("fitPlatt did not converge (a=%v b=%v)", a, b)
			}
			if !isFinite(a) || !isFinite(b) {
				t.Fatalf("a=%v b=%v, want finite", a, b)
			}
			ga, gb := refPlattGradient(tc.z, tc.y, tc.w, a, b)
			if math.Abs(ga) >= opts.Tol || math.Abs(gb) >= opts.Tol {
				t.Errorf("gradient (%g, %g) at a=%v b=%v, want below %g in both components", ga, gb, a, b, opts.Tol)
			}
		})
	}
}

func TestLogisticFitWithOneScalePositiveIsFinite(t *testing.T) {
	fit, _ := separable()
	scale := []Example{{Features: []float64{3, -3}, Label: true, Weight: 1, Group: "s-p"}}
	for i := 0; i < 40; i++ {
		scale = append(scale, Example{Features: []float64{-3, 3}, Label: false, Weight: 1, Group: fmt.Sprintf("s-n%02d", i)})
	}
	opts := DefaultLogisticOptions()
	m, err := FitLogistic(fit, scale, opts)
	if err != nil {
		t.Fatalf("FitLogistic: %v", err)
	}
	if !m.Converged {
		t.Fatal("Converged = false for a scale set with one positive")
	}
	if !isFinite(m.PlattA) || !isFinite(m.PlattB) || !(m.PlattA > 0) {
		t.Fatalf("platt a=%v b=%v, want finite with a positive slope", m.PlattA, m.PlattB)
	}
	logits := make([]float64, len(scale))
	y := make([]bool, len(scale))
	for i, e := range scale {
		logits[i], y[i] = m.logit(e.Features), e.Label
	}
	ga, gb := refPlattGradient(logits, y, refWeights(scale), m.PlattA, m.PlattB)
	if math.Abs(ga) >= opts.Tol || math.Abs(gb) >= opts.Tol {
		t.Errorf("platt gradient (%g, %g), want below %g in both components", ga, gb, opts.Tol)
	}
}

// TestLogisticDescendReportsConvergenceHonestly: converged means the largest
// gradient component is below Tol at the returned point, and a run stopped by
// MaxIter before that says so.
func TestLogisticDescendReportsConvergenceHonestly(t *testing.T) {
	fit, _ := separable()
	_, _, x, err := standardize(fit)
	if err != nil {
		t.Fatalf("standardize: %v", err)
	}
	opts := DefaultLogisticOptions()
	p := logisticProblem{x: x, y: labelTargets(exampleLabels(fit)), w: ones(len(fit)), l2: opts.L2}
	ref := refProblem{x: x, target: refTargets(fit), wt: ones(len(fit)), l2: opts.L2}

	w, b, iters, converged, err := descend(p, make([]float64, 2), 0, opts)
	if err != nil {
		t.Fatalf("descend: %v", err)
	}
	if !converged || iters <= 0 {
		t.Fatalf("converged %v after %d iterations, want true after at least one", converged, iters)
	}
	if worst := ref.worstGradient(w, b); !(worst < opts.Tol) {
		t.Errorf("converged = true with the largest gradient component %g, want below %g", worst, opts.Tol)
	}

	// Started at the optimum there is nothing left to do.
	if _, _, again, converged, err := descend(p, w, b, opts); err != nil || !converged || again != 0 {
		t.Errorf("descend from the optimum: iterations %d converged %v err %v, want 0 true nil", again, converged, err)
	}

	opts.MaxIter = 1
	w1, b1, iters, converged, err := descend(p, make([]float64, 2), 0, opts)
	if err != nil {
		t.Fatalf("descend with MaxIter 1: %v", err)
	}
	if converged || iters != 1 {
		t.Errorf("MaxIter 1: converged %v after %d iterations, want false after 1", converged, iters)
	}
	if worst := ref.worstGradient(w1, b1); !(worst >= opts.Tol) {
		t.Errorf("unfinished run reports the largest gradient component %g, want at least %g", worst, opts.Tol)
	}
}

// TestLogisticDescendFindsTheSameOptimumFromAnyStart: the objective is strictly
// convex, so a start deep in the flat tails of the sigmoid, where a full
// Newton step is huge and overshoots, must still reach the optimum found from
// zero. Only the line search makes that so.
func TestLogisticDescendFindsTheSameOptimumFromAnyStart(t *testing.T) {
	fit, _ := separable()
	_, _, x, err := standardize(fit)
	if err != nil {
		t.Fatalf("standardize: %v", err)
	}
	opts := DefaultLogisticOptions()
	p := logisticProblem{x: x, y: labelTargets(exampleLabels(fit)), w: ones(len(fit)), l2: opts.L2}
	want, wantBias, _, _, err := descend(p, make([]float64, 2), 0, opts)
	if err != nil {
		t.Fatalf("descend from zero: %v", err)
	}
	starts := []struct {
		w    [2]float64
		bias float64
	}{
		{[2]float64{3, -3}, 1},
		{[2]float64{8, -8}, 3},
		{[2]float64{-15, -15}, 20},
		{[2]float64{100, 100}, -50},
	}
	for _, s := range starts {
		t.Run(fmt.Sprintf("start %v %v", s.w, s.bias), func(t *testing.T) {
			w, b, _, converged, err := descend(p, s.w[:], s.bias, opts)
			if err != nil {
				t.Fatalf("descend: %v", err)
			}
			if !converged {
				t.Fatal("did not converge")
			}
			if !approx(w[0], want[0], 1e-6) || !approx(w[1], want[1], 1e-6) || !approx(b, wantBias, 1e-6) {
				t.Errorf("optimum w=%v b=%v, want w=%v b=%v", w, b, want, wantBias)
			}
		})
	}
}

func TestLogisticBacktrackRefusesAHopelessStep(t *testing.T) {
	fit, _ := separable()
	_, _, x, err := standardize(fit)
	if err != nil {
		t.Fatalf("standardize: %v", err)
	}
	p := logisticProblem{x: x, y: labelTargets(exampleLabels(fit)), w: ones(len(fit)), l2: 1}
	total := float64(len(fit))
	cur := p.evaluate(make([]float64, 3), total)
	// Even halved sixty times, a step of 1e30 lands far out on the flat tail
	// of the sigmoid where the loss is far above the starting loss.
	if next, ok := p.backtrack(cur, []float64{1e30, 1e30, 1e30}, total); ok {
		t.Errorf("backtrack accepted a hopeless step (loss %v from %v)", next.loss, cur.loss)
	}
	step, err := p.newtonStep(cur, total)
	if err != nil {
		t.Fatalf("newtonStep: %v", err)
	}
	next, ok := p.backtrack(cur, step, total)
	if !ok || !(next.loss < cur.loss) {
		t.Errorf("backtrack on the Newton step: ok %v, loss %v from %v, want a lower loss", ok, next.loss, cur.loss)
	}
}

func TestLogisticFitRefusesASingularSystem(t *testing.T) {
	fit, scale := separable()
	widen := func(in []Example) []Example {
		out := make([]Example, len(in))
		for i, e := range in {
			e.Features = append(append([]float64(nil), e.Features...), 7)
			out[i] = e
		}
		return out
	}
	opts := DefaultLogisticOptions()
	opts.L2 = 0
	_, err := FitLogistic(widen(fit), widen(scale), opts)
	if err == nil || !strings.Contains(err.Error(), "positive definite") {
		t.Fatalf("FitLogistic with an unpenalized constant column: err = %v, want a positive definite error", err)
	}
	// The default penalty keeps the same system solvable.
	if _, err := FitLogistic(widen(fit), widen(scale), DefaultLogisticOptions()); err != nil {
		t.Errorf("FitLogistic with the default penalty: %v", err)
	}
}
