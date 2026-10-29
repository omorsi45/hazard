package predict

import (
	"errors"
	"fmt"
	"math"
	"sort"
)

// IsotonicModel is a monotone step function. X holds the breakpoints in
// ascending order and Y the fitted value of the step that starts at each
// breakpoint, non-decreasing and within [0,1] when the fitted ys are labels.
type IsotonicModel struct {
	X []float64 `json:"x"`
	Y []float64 `json:"y"`
}

// isoBlock is a run of pooled points: the smallest x of the run, the summed
// weight and the weighted mean of the pooled ys.
type isoBlock struct {
	x, w, mean float64
}

// FitIsotonic fits a non-decreasing step function to (xs, ys) with weights ws
// by weighted pool-adjacent-violators. Points sharing an x are pooled first,
// then neighbouring blocks are pooled while the left mean is not below the
// right mean, so the result has strictly ascending X and strictly increasing
// Y (equal neighbours merge into one step). The fit does not depend on input
// order. Inputs must be finite, weights positive, the three slices of equal
// length and at least one point given.
func FitIsotonic(xs, ys, ws []float64) (IsotonicModel, error) {
	n := len(xs)
	if n == 0 {
		return IsotonicModel{}, errors.New("isotonic fit needs at least one point")
	}
	if len(ys) != n || len(ws) != n {
		return IsotonicModel{}, fmt.Errorf("isotonic fit input lengths differ: %d xs, %d ys, %d ws", n, len(ys), len(ws))
	}
	for i := 0; i < n; i++ {
		if !isFinite(xs[i]) || !isFinite(ys[i]) || !isFinite(ws[i]) {
			return IsotonicModel{}, fmt.Errorf("isotonic fit point %d is not finite (x=%v y=%v w=%v)", i, xs[i], ys[i], ws[i])
		}
		if ws[i] <= 0 {
			return IsotonicModel{}, fmt.Errorf("isotonic fit point %d has non-positive weight %v", i, ws[i])
		}
	}

	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	// Order by x, then y, then weight so equal-x pooling sums in a fixed order
	// and the result cannot depend on input order.
	sort.Slice(idx, func(a, b int) bool {
		i, j := idx[a], idx[b]
		if xs[i] != xs[j] {
			return xs[i] < xs[j]
		}
		if ys[i] != ys[j] {
			return ys[i] < ys[j]
		}
		return ws[i] < ws[j]
	})

	blocks := make([]isoBlock, 0, n)
	for p := 0; p < n; {
		q := p
		var sw, swy float64
		for q < n && xs[idx[q]] == xs[idx[p]] {
			sw += ws[idx[q]]
			swy += ws[idx[q]] * ys[idx[q]]
			q++
		}
		blocks = append(blocks, isoBlock{x: xs[idx[p]], w: sw, mean: swy / sw})
		for len(blocks) > 1 && blocks[len(blocks)-2].mean >= blocks[len(blocks)-1].mean {
			hi := blocks[len(blocks)-1]
			lo := blocks[len(blocks)-2]
			w := lo.w + hi.w
			blocks = blocks[:len(blocks)-1]
			blocks[len(blocks)-1] = isoBlock{x: lo.x, w: w, mean: (lo.w*lo.mean + hi.w*hi.mean) / w}
		}
		p = q
	}

	m := IsotonicModel{X: make([]float64, len(blocks)), Y: make([]float64, len(blocks))}
	for i, b := range blocks {
		m.X[i], m.Y[i] = b.x, b.mean
	}
	return m, nil
}

// Predict returns the value of the step with the largest breakpoint X <= x,
// clamping below the first breakpoint to Y[0] and above the last to Y[last].
// An unknown input (NaN) or an empty model returns NaN: unknown is never 0.
func (m IsotonicModel) Predict(x float64) float64 {
	if math.IsNaN(x) || len(m.X) == 0 || len(m.X) != len(m.Y) {
		return math.NaN()
	}
	i := sort.Search(len(m.X), func(i int) bool { return m.X[i] > x })
	if i == 0 {
		return m.Y[0]
	}
	return m.Y[i-1]
}

func isFinite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
