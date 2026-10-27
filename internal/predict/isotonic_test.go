package predict

import (
	"math"
	"reflect"
	"testing"
)

func ones(n int) []float64 {
	w := make([]float64, n)
	for i := range w {
		w[i] = 1
	}
	return w
}

func approx(got, want, tol float64) bool { return math.Abs(got-want) <= tol }

func TestIsotonicFitKnownVectors(t *testing.T) {
	cases := []struct {
		name   string
		xs, ys []float64
		ws     []float64
		wantX  []float64
		wantY  []float64
	}{
		{
			name:  "already monotone is kept",
			xs:    []float64{1, 2, 3},
			ys:    []float64{0, 0.5, 1},
			ws:    ones(3),
			wantX: []float64{1, 2, 3},
			wantY: []float64{0, 0.5, 1},
		},
		{
			name:  "single violation pools to the mean",
			xs:    []float64{1, 2, 3, 4},
			ys:    []float64{0.1, 0.3, 0.2, 0.4},
			ws:    ones(4),
			wantX: []float64{1, 2, 4},
			wantY: []float64{0.1, 0.25, 0.4},
		},
		{
			name:  "weights shift the pooled mean",
			xs:    []float64{1, 2, 3, 4},
			ys:    []float64{0.1, 0.3, 0.2, 0.4},
			ws:    []float64{1, 1, 3, 1},
			wantX: []float64{1, 2, 4},
			wantY: []float64{0.1, 0.225, 0.4},
		},
		{
			name:  "cascading pool absorbs the earlier block",
			xs:    []float64{1, 2, 3, 4},
			ys:    []float64{0.2, 0.6, 0.4, 0.3},
			ws:    ones(4),
			wantX: []float64{1, 2},
			wantY: []float64{0.2, 1.3 / 3},
		},
		{
			name:  "fully decreasing pools to one block",
			xs:    []float64{1, 2, 3, 4},
			ys:    []float64{0.9, 0.5, 0.4, 0.1},
			ws:    ones(4),
			wantX: []float64{1},
			wantY: []float64{0.475},
		},
		{
			name:  "equal neighbours merge into one step",
			xs:    []float64{1, 2},
			ys:    []float64{0.5, 0.5},
			ws:    ones(2),
			wantX: []float64{1},
			wantY: []float64{0.5},
		},
		{
			name:  "ties in x are pooled first",
			xs:    []float64{1, 1, 2, 2},
			ys:    []float64{0, 1, 1, 1},
			ws:    ones(4),
			wantX: []float64{1, 2},
			wantY: []float64{0.5, 1},
		},
		{
			name:  "tied x pool is weighted and input order does not matter",
			xs:    []float64{3, 1, 2, 1},
			ys:    []float64{1, 1, 1, 0},
			ws:    []float64{1, 3, 1, 1},
			wantX: []float64{1, 2},
			wantY: []float64{0.75, 1},
		},
		{
			name:  "single point",
			xs:    []float64{7},
			ys:    []float64{1},
			ws:    []float64{2},
			wantX: []float64{7},
			wantY: []float64{1},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := FitIsotonic(tc.xs, tc.ys, tc.ws)
			if err != nil {
				t.Fatalf("FitIsotonic: %v", err)
			}
			if len(m.X) != len(tc.wantX) || len(m.Y) != len(tc.wantY) {
				t.Fatalf("model = X %v Y %v, want X %v Y %v", m.X, m.Y, tc.wantX, tc.wantY)
			}
			for i := range tc.wantX {
				if !approx(m.X[i], tc.wantX[i], 1e-12) || !approx(m.Y[i], tc.wantY[i], 1e-12) {
					t.Fatalf("model = X %v Y %v, want X %v Y %v", m.X, m.Y, tc.wantX, tc.wantY)
				}
			}
		})
	}
}

func TestIsotonicFitRejectsInvalidInput(t *testing.T) {
	nan, inf := math.NaN(), math.Inf(1)
	cases := []struct {
		name       string
		xs, ys, ws []float64
	}{
		{"no points", nil, nil, nil},
		{"ys shorter", []float64{1, 2}, []float64{1}, []float64{1, 1}},
		{"ws shorter", []float64{1, 2}, []float64{1, 0}, []float64{1}},
		{"NaN x", []float64{nan}, []float64{1}, []float64{1}},
		{"Inf x", []float64{inf}, []float64{1}, []float64{1}},
		{"NaN y", []float64{1}, []float64{nan}, []float64{1}},
		{"Inf y", []float64{1}, []float64{inf}, []float64{1}},
		{"NaN weight", []float64{1}, []float64{1}, []float64{nan}},
		{"Inf weight", []float64{1}, []float64{1}, []float64{inf}},
		{"zero weight", []float64{1}, []float64{1}, []float64{0}},
		{"negative weight", []float64{1}, []float64{1}, []float64{-1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := FitIsotonic(tc.xs, tc.ys, tc.ws); err == nil {
				t.Fatal("FitIsotonic accepted invalid input")
			}
		})
	}
}

func TestIsotonicPredictSteps(t *testing.T) {
	m := IsotonicModel{X: []float64{1, 2, 4}, Y: []float64{0.1, 0.25, 0.4}}
	cases := []struct {
		x, want float64
	}{
		{-100, 0.1}, // clamp below to Y[0]
		{0.999, 0.1},
		{1, 0.1},
		{1.5, 0.1},
		{2, 0.25}, // largest X <= x
		{3.999, 0.25},
		{4, 0.4},
		{1e9, 0.4}, // clamp above to Y[last]
		{math.Inf(-1), 0.1},
		{math.Inf(1), 0.4},
	}
	for _, tc := range cases {
		if got := m.Predict(tc.x); got != tc.want {
			t.Errorf("Predict(%v) = %v, want %v", tc.x, got, tc.want)
		}
	}
}

func TestIsotonicPredictUnknownIsNaN(t *testing.T) {
	m := IsotonicModel{X: []float64{1, 2}, Y: []float64{0.1, 0.2}}
	if got := m.Predict(math.NaN()); !math.IsNaN(got) {
		t.Errorf("Predict(NaN) = %v, want NaN (unknown is never 0)", got)
	}
	if got := (IsotonicModel{}).Predict(1); !math.IsNaN(got) {
		t.Errorf("empty model Predict = %v, want NaN (unknown is never 0)", got)
	}
}

// lcg is a tiny deterministic generator so property tests need no seed state.
type lcg uint64

func (l *lcg) next() float64 {
	*l = *l*6364136223846793005 + 1442695040888963407
	return float64(uint64(*l)>>11) / float64(1<<53)
}

func TestIsotonicFitProperties(t *testing.T) {
	g := lcg(42)
	const n = 400
	xs, ys, ws := make([]float64, n), make([]float64, n), make([]float64, n)
	for i := range xs {
		xs[i] = math.Floor(g.next() * 50) // many ties
		p := xs[i] / 50
		if g.next() < p {
			ys[i] = 1
		}
		ws[i] = 0.5 + g.next()*3
	}
	m, err := FitIsotonic(xs, ys, ws)
	if err != nil {
		t.Fatalf("FitIsotonic: %v", err)
	}
	for i := range m.X {
		if i > 0 && !(m.X[i] > m.X[i-1]) {
			t.Fatalf("X not strictly ascending at %d: %v", i, m.X)
		}
		if i > 0 && !(m.Y[i] > m.Y[i-1]) {
			t.Fatalf("Y not strictly increasing at %d: %v", i, m.Y)
		}
		if m.Y[i] < 0 || m.Y[i] > 1 {
			t.Fatalf("Y[%d] = %v outside [0,1] for label inputs", i, m.Y[i])
		}
	}
	// PAV preserves the weighted total of the fitted values.
	var wantSum, gotSum float64
	for i := range xs {
		wantSum += ws[i] * ys[i]
		gotSum += ws[i] * m.Predict(xs[i])
	}
	if !approx(gotSum, wantSum, 1e-9) {
		t.Errorf("weighted sum of fitted values = %v, want %v", gotSum, wantSum)
	}
	// The fit does not depend on input order.
	rx, ry, rw := reverse(xs), reverse(ys), reverse(ws)
	m2, err := FitIsotonic(rx, ry, rw)
	if err != nil {
		t.Fatalf("FitIsotonic reversed: %v", err)
	}
	if !reflect.DeepEqual(m, m2) {
		t.Errorf("fit depends on input order:\n%v\n%v", m, m2)
	}
}

func reverse(in []float64) []float64 {
	out := make([]float64, len(in))
	for i, v := range in {
		out[len(in)-1-i] = v
	}
	return out
}
