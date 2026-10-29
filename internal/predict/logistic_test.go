package predict

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
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
// that both descents meet the default tolerance well inside the default
// MaxIter (centres at +-1 need about 6000 and 13000 iterations).
func separable() (fit, scale []Example) {
	g := lcg(11)
	return clusters(&g, 100, 100, [2]float64{0.5, -0.5}, [2]float64{-0.5, 0.5}, "f"),
		clusters(&g, 50, 50, [2]float64{0.5, -0.5}, [2]float64{-0.5, 0.5}, "s")
}

func TestLogisticDefaultOptions(t *testing.T) {
	want := LogisticOptions{L2: 1, CIWeight: 10, MaxIter: 5000, Tol: 1e-9, LearningRate: 0.1}
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
		// Iterations below the cap means the coefficient descent met the
		// tolerance; Converged also needs the Platt descent, which is slow
		// when strong shrinkage leaves the raw logits almost constant.
		if m.Iterations >= opts.MaxIter {
			t.Fatalf("L2=%v coefficient descent hit the %d iteration cap", l2, opts.MaxIter)
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
	w, b, _, _, err := descend(logisticProblem{x: x, y: exampleLabels(fit), w: ones(len(fit)), l2: opts.L2}, make([]float64, 2), 0, opts)
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
func calibratedGrid(transform func(z float64) float64) (z []float64, y []bool, w []float64) {
	for k := -8; k <= 8; k++ {
		raw := float64(k) / 2
		p := sigmoid(raw)
		z = append(z, transform(raw), transform(raw))
		y = append(y, true, false)
		w = append(w, p, 1-p)
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

// extremeScale is a scale set the fitted model separates perfectly, so the
// Platt slope has no finite optimum and its descent cannot meet the tolerance.
func extremeScale() []Example {
	var out []Example
	for i := 0; i < 10; i++ {
		out = append(out,
			Example{Features: []float64{3, -3}, Label: true, Weight: 1, Group: fmt.Sprintf("x-p%02d", i)},
			Example{Features: []float64{-3, 3}, Label: false, Weight: 1, Group: fmt.Sprintf("x-n%02d", i)})
	}
	return out
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
	sep, err := FitLogistic(fit, extremeScale(), opts)
	if err != nil {
		t.Fatalf("FitLogistic with a separable scale set: %v", err)
	}
	if sep.Iterations != base.Iterations || !reflect.DeepEqual(sep.Weights, base.Weights) {
		t.Fatalf("coefficient stage differs from the baseline: %d vs %d iterations", sep.Iterations, base.Iterations)
	}
	if sep.Converged {
		t.Error("Converged = true although the Platt fit on a perfectly separable scale set cannot converge")
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
		{"zero LearningRate", fit, scale, badOpts(func(o *LogisticOptions) { o.LearningRate = 0 })},
		{"Inf LearningRate", fit, scale, badOpts(func(o *LogisticOptions) { o.LearningRate = math.Inf(1) })},
		{"diverging LearningRate", fit, scale, badOpts(func(o *LogisticOptions) { o.LearningRate = 1e308 })},
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
