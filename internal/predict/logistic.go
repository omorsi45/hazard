package predict

import (
	"errors"
	"fmt"
	"math"

	"github.com/omorsi45/hazard/internal/impact"
)

// Example is one labelled training row. Group is the source event (a commit
// or a mutant base family); every row of one group lands on one side of a
// fit/scale split.
type Example struct {
	Rank     float64
	Features []float64
	Label    bool
	Weight   float64
	Group    string
}

// LogisticOptions are the fit hyperparameters recorded in the artifact.
// CIWeight is the up-weight callers give CI-label examples through
// Example.Weight; FitLogistic only validates and records it.
type LogisticOptions struct {
	L2       float64 `json:"l2"`
	CIWeight float64 `json:"ci_weight"`
	MaxIter  int     `json:"max_iter"`
	Tol      float64 `json:"tol"`
}

// DefaultLogisticOptions returns the documented defaults.
func DefaultLogisticOptions() LogisticOptions {
	return LogisticOptions{L2: 1, CIWeight: 10, MaxIter: 5000, Tol: 1e-9}
}

func (o LogisticOptions) validate() error {
	switch {
	case !isFinite(o.L2) || o.L2 < 0:
		return fmt.Errorf("option L2 must be finite and not negative, got %v", o.L2)
	case !isFinite(o.CIWeight) || o.CIWeight <= 0:
		return fmt.Errorf("option CIWeight must be finite and positive, got %v", o.CIWeight)
	case o.MaxIter <= 0:
		return fmt.Errorf("option MaxIter must be positive, got %d", o.MaxIter)
	case !isFinite(o.Tol) || o.Tol <= 0:
		return fmt.Errorf("option Tol must be finite and positive, got %v", o.Tol)
	}
	return nil
}

// LogisticModel is a standardized logistic regression with Platt scaling.
// The probability of a feature vector f is
// sigmoid(PlattA*z + PlattB) with z = Bias + sum_j Weights[j]*(f[j]-Means[j])/Scales[j].
// Iterations counts the Newton steps of the coefficient fit; Converged is true
// only when both the coefficient fit and the Platt fit met the tolerance.
type LogisticModel struct {
	FeatureVersion string    `json:"feature_version"`
	Means          []float64 `json:"means"`
	Scales         []float64 `json:"scales"`
	Weights        []float64 `json:"weights"`
	Bias           float64   `json:"bias"`
	PlattA         float64   `json:"platt_a"`
	PlattB         float64   `json:"platt_b"`
	Iterations     int       `json:"iterations"`
	Converged      bool      `json:"converged"`
}

// logisticProblem is a weighted logistic objective over standardized rows:
// the weighted mean log loss against the targets y (each in [0, 1]) plus
// l2/2*||w||^2/n, the bias unpenalized.
type logisticProblem struct {
	x  [][]float64
	y  []float64
	w  []float64
	l2 float64
}

// exampleLabels returns the labels of ex in order.
func exampleLabels(ex []Example) []bool {
	out := make([]bool, len(ex))
	for i, e := range ex {
		out[i] = e.Label
	}
	return out
}

// labelTargets maps labels to the hard targets 1 and 0.
func labelTargets(labels []bool) []float64 {
	out := make([]float64, len(labels))
	for i, l := range labels {
		if l {
			out[i] = 1
		}
	}
	return out
}

// plattCounts returns the weighted number of positive and negative labels.
func plattCounts(labels []bool, w []float64) (pos, neg float64) {
	for i, l := range labels {
		if l {
			pos += w[i]
		} else {
			neg += w[i]
		}
	}
	return pos, neg
}

// plattTargets returns the regularized targets of Platt scaling (Platt 1999;
// Lin, Lin and Weng 2007): (N+ + 1)/(N+ + 2) for a positive and 1/(N- + 2)
// for a negative, with N+ and N- the weighted class counts. Unlike the hard
// 0/1 targets they keep the optimum finite when the scale set is separated
// by the logit, which happens whenever it holds few positives.
func plattTargets(labels []bool, w []float64) []float64 {
	pos, neg := plattCounts(labels, w)
	hi, lo := (pos+1)/(pos+2), 1/(neg+2)
	out := make([]float64, len(labels))
	for i, l := range labels {
		if l {
			out[i] = hi
		} else {
			out[i] = lo
		}
	}
	return out
}

// exampleWeights returns the per-example weights of ex: the class weight
// n/(2*positives) or n/(2*negatives), which balances the two classes, times
// Example.Weight. Each Weight must be finite and positive, and both classes
// must be present; name labels the set in errors.
func exampleWeights(name string, ex []Example) ([]float64, error) {
	var pos, neg int
	for i, e := range ex {
		if !isFinite(e.Weight) || e.Weight <= 0 {
			return nil, fmt.Errorf("%s example %d has weight %v, want finite and positive", name, i, e.Weight)
		}
		if e.Label {
			pos++
		} else {
			neg++
		}
	}
	if pos == 0 {
		return nil, fmt.Errorf("%s set has no positive examples (%d negatives)", name, neg)
	}
	if neg == 0 {
		return nil, fmt.Errorf("%s set has no negative examples (%d positives)", name, pos)
	}
	n := float64(len(ex))
	posClass, negClass := n/(2*float64(pos)), n/(2*float64(neg))
	out := make([]float64, len(ex))
	for i, e := range ex {
		class := negClass
		if e.Label {
			class = posClass
		}
		out[i] = class * e.Weight
		if !isFinite(out[i]) {
			return nil, fmt.Errorf("%s example %d weight %v overflows", name, i, e.Weight)
		}
	}
	return out, nil
}

// standardize returns the per-column mean and population standard deviation
// of ex and the standardized rows. A constant column keeps its exact value as
// the mean and gets scale 1. Every row must have the same positive width and
// only finite values, and the statistics must not overflow.
func standardize(ex []Example) (means, scales []float64, x [][]float64, err error) {
	if len(ex) == 0 {
		return nil, nil, nil, errors.New("no examples to standardize")
	}
	d := len(ex[0].Features)
	if d == 0 {
		return nil, nil, nil, errors.New("examples have no features")
	}
	for i, e := range ex {
		if len(e.Features) != d {
			return nil, nil, nil, fmt.Errorf("example %d has %d features, want %d", i, len(e.Features), d)
		}
		for j, v := range e.Features {
			if !isFinite(v) {
				return nil, nil, nil, fmt.Errorf("example %d feature %d is not finite (%v)", i, j, v)
			}
		}
	}
	n := float64(len(ex))
	means, scales = make([]float64, d), make([]float64, d)
	for j := 0; j < d; j++ {
		first := ex[0].Features[j]
		constant := true
		var sum float64
		for _, e := range ex {
			constant = constant && e.Features[j] == first
			sum += e.Features[j]
		}
		if constant {
			means[j], scales[j] = first, 1
			continue
		}
		mean := sum / n
		var ss float64
		for _, e := range ex {
			dv := e.Features[j] - mean
			ss += dv * dv
		}
		std := math.Sqrt(ss / n)
		if !isFinite(mean) || !isFinite(std) {
			return nil, nil, nil, fmt.Errorf("feature %d statistics overflow (mean %v, std %v)", j, mean, std)
		}
		if std == 0 {
			std = 1
		}
		means[j], scales[j] = mean, std
	}
	x = make([][]float64, len(ex))
	for i, e := range ex {
		row := make([]float64, d)
		for j, v := range e.Features {
			row[j] = (v - means[j]) / scales[j]
			if !isFinite(row[j]) {
				return nil, nil, nil, fmt.Errorf("example %d feature %d does not standardize to a finite value", i, j)
			}
		}
		x[i] = row
	}
	return means, scales, x, nil
}

// logisticPoint is a logisticProblem evaluated at theta, the weights followed
// by the bias.
type logisticPoint struct {
	theta []float64
	loss  float64   // the objective
	grad  []float64 // its gradient, in the order of theta
	curv  []float64 // sigmoid'(z) of each row
}

// softplus is log(1+exp(z)), stable for large |z|. NaN stays NaN.
func softplus(z float64) float64 {
	if z > 0 {
		return z + math.Log1p(math.Exp(-z))
	}
	return math.Log1p(math.Exp(z))
}

// evaluate returns the objective, its gradient and the per-row curvature of p
// at theta. total is the sum of p.w.
func (p logisticProblem) evaluate(theta []float64, total float64) logisticPoint {
	d := len(theta) - 1
	pt := logisticPoint{theta: theta, grad: make([]float64, d+1), curv: make([]float64, len(p.x))}
	var loss float64
	for i, row := range p.x {
		z := theta[d]
		for j, v := range row {
			z += theta[j] * v
		}
		s := sigmoid(z)
		pt.curv[i] = s * (1 - s)
		loss += p.w[i] * (softplus(z) - p.y[i]*z)
		r := p.w[i] * (s - p.y[i])
		pt.grad[d] += r
		for j, v := range row {
			pt.grad[j] += r * v
		}
	}
	ridge := p.l2 / float64(len(p.x))
	var sq float64
	for j := 0; j < d; j++ {
		sq += theta[j] * theta[j]
		pt.grad[j] = pt.grad[j]/total + ridge*theta[j]
	}
	pt.grad[d] /= total
	pt.loss = loss/total + ridge*sq/2
	return pt
}

// choleskySolve solves A*x = b for a symmetric A of order m held row-major in
// its lower triangle. It overwrites a with the Cholesky factor and b with x.
// A pivot that is not positive and finite means A is not positive definite.
func choleskySolve(a []float64, m int, b []float64) error {
	for j := 0; j < m; j++ {
		s := a[j*m+j]
		for k := 0; k < j; k++ {
			s -= a[j*m+k] * a[j*m+k]
		}
		if !(s > 0) || math.IsInf(s, 0) {
			return fmt.Errorf("the system is not positive definite (pivot %d is %v)", j, s)
		}
		ljj := math.Sqrt(s)
		a[j*m+j] = ljj
		for i := j + 1; i < m; i++ {
			t := a[i*m+j]
			for k := 0; k < j; k++ {
				t -= a[i*m+k] * a[j*m+k]
			}
			a[i*m+j] = t / ljj
		}
	}
	for i := 0; i < m; i++ {
		s := b[i]
		for k := 0; k < i; k++ {
			s -= a[i*m+k] * b[k]
		}
		b[i] = s / a[i*m+i]
	}
	for i := m - 1; i >= 0; i-- {
		s := b[i]
		for k := i + 1; k < m; k++ {
			s -= a[k*m+i] * b[k]
		}
		b[i] = s / a[i*m+i]
	}
	return nil
}

// newtonStep returns the Newton step -H^-1*g at pt. H is the Hessian of the
// objective: the curvature-weighted cross products of the rows (with the
// constant bias column appended) plus the ridge on the weight diagonal.
func (p logisticProblem) newtonStep(pt logisticPoint, total float64) ([]float64, error) {
	m := len(pt.theta)
	d := m - 1
	h := make([]float64, m*m)
	xr := make([]float64, m)
	xr[d] = 1
	for i, row := range p.x {
		c := p.w[i] * pt.curv[i] / total
		if c == 0 {
			continue
		}
		copy(xr, row)
		for a := 0; a < m; a++ {
			ca := c * xr[a]
			if ca == 0 {
				continue
			}
			for b := 0; b <= a; b++ {
				h[a*m+b] += ca * xr[b]
			}
		}
	}
	ridge := p.l2 / float64(len(p.x))
	for j := 0; j < d; j++ {
		h[j*m+j] += ridge
	}
	step := make([]float64, m)
	for j, g := range pt.grad {
		step[j] = -g
	}
	if err := choleskySolve(h, m, step); err != nil {
		return nil, err
	}
	return step, nil
}

const (
	// maxHalvings bounds the backtracking line search: it tries the full
	// Newton step and then up to this many halvings of it.
	maxHalvings = 60
	// objectiveNoise is the relative rounding noise of the objective, a sum
	// of many terms. Near the optimum the objective gain of a Newton step is
	// smaller than it, so a step is accepted while the objective has not
	// risen by more than this; otherwise the search would shrink steps that
	// still reduce the gradient until they were too small to move anything.
	objectiveNoise = 1e-12
)

// backtrack halves the step along delta from cur until the objective does not
// increase (beyond rounding noise), so each iteration is a descent step. It
// reports false when neither the step nor any of its first maxHalvings
// halvings qualifies.
func (p logisticProblem) backtrack(cur logisticPoint, delta []float64, total float64) (logisticPoint, bool) {
	limit := cur.loss + objectiveNoise*(1+math.Abs(cur.loss))
	step := 1.0
	for h := 0; h <= maxHalvings; h++ {
		trial := make([]float64, len(cur.theta))
		for j := range trial {
			trial[j] = cur.theta[j] + step*delta[j]
		}
		next := p.evaluate(trial, total)
		if isFinite(next.loss) && next.loss <= limit {
			return next, true
		}
		step /= 2
	}
	return logisticPoint{}, false
}

// descend minimizes p from (w0, b0) by Newton steps with a backtracking line
// search. It stops when the largest absolute gradient component falls below
// opts.Tol (converged true) or after opts.MaxIter steps (converged false, the
// last point still returned). A gradient or step that is not finite, a step
// no halving of which lowers the objective, or a Hessian that is not positive
// definite is an error.
func descend(p logisticProblem, w0 []float64, b0 float64, opts LogisticOptions) (weights []float64, bias float64, iters int, converged bool, err error) {
	var total float64
	for _, wi := range p.w {
		total += wi
	}
	if !(total > 0) || math.IsInf(total, 0) {
		return nil, 0, 0, false, fmt.Errorf("total example weight %v, want finite and positive", total)
	}
	cur := p.evaluate(append(append([]float64(nil), w0...), b0), total)
	d := len(w0)
	for {
		worst := 0.0
		for _, g := range cur.grad {
			worst = math.Max(worst, math.Abs(g))
		}
		if !isFinite(worst) || !isFinite(cur.loss) {
			return nil, 0, iters, false, fmt.Errorf("gradient is not finite after %d iterations", iters)
		}
		if worst < opts.Tol {
			return append([]float64(nil), cur.theta[:d]...), cur.theta[d], iters, true, nil
		}
		if iters >= opts.MaxIter {
			return append([]float64(nil), cur.theta[:d]...), cur.theta[d], iters, false, nil
		}
		delta, err := p.newtonStep(cur, total)
		if err != nil {
			return nil, 0, iters, false, fmt.Errorf("newton step %d: %w", iters+1, err)
		}
		for _, v := range delta {
			if !isFinite(v) {
				return nil, 0, iters, false, fmt.Errorf("newton step %d is not finite", iters+1)
			}
		}
		next, ok := p.backtrack(cur, delta, total)
		if !ok {
			return nil, 0, iters, false, fmt.Errorf("newton step %d does not lower the objective", iters+1)
		}
		cur = next
		iters++
	}
}

// fitPlatt fits P(y) = sigmoid(a*z + b) on raw logits z with example weights w
// against the regularized Platt targets, by the same Newton solver in two
// dimensions, with no penalty. It starts from the constant prediction a=0,
// b=log((N+ + 1)/(N- + 1)) (Platt 1999): started at a=1 the rows of a scale
// set far outside the fit sit on the flat tails of the sigmoid, where the
// Hessian vanishes and no Newton step is usable.
func fitPlatt(z []float64, y []bool, w []float64, opts LogisticOptions) (a, b float64, iters int, converged bool, err error) {
	if len(z) == 0 || len(y) != len(z) || len(w) != len(z) {
		return 0, 0, 0, false, fmt.Errorf("platt fit needs equal non-empty inputs, got %d logits, %d labels, %d weights", len(z), len(y), len(w))
	}
	rows := make([][]float64, len(z))
	for i, v := range z {
		if !isFinite(v) {
			return 0, 0, 0, false, fmt.Errorf("platt fit logit %d is not finite (%v)", i, v)
		}
		rows[i] = []float64{v}
	}
	pos, neg := plattCounts(y, w)
	weights, bias, iters, converged, err := descend(
		logisticProblem{x: rows, y: plattTargets(y, w), w: w}, []float64{0}, math.Log((pos+1)/(neg+1)), opts)
	if err != nil {
		return 0, 0, iters, false, err
	}
	return weights[0], bias, iters, converged, nil
}

// validate reports whether m can score: current feature version, matching
// non-empty vectors, finite parameters and strictly positive scales.
func (m LogisticModel) validate() error {
	if m.FeatureVersion != impact.FeatureVersion {
		return fmt.Errorf("feature version %q, want %q", m.FeatureVersion, impact.FeatureVersion)
	}
	d := len(m.Weights)
	if d == 0 {
		return errors.New("model has no weights")
	}
	if len(m.Means) != d || len(m.Scales) != d {
		return fmt.Errorf("model has %d weights, %d means and %d scales", d, len(m.Means), len(m.Scales))
	}
	for j := 0; j < d; j++ {
		if !isFinite(m.Means[j]) || !isFinite(m.Weights[j]) {
			return fmt.Errorf("model mean or weight %d is not finite", j)
		}
		if !isFinite(m.Scales[j]) || m.Scales[j] <= 0 {
			return fmt.Errorf("model scale %d is %v, want finite and positive", j, m.Scales[j])
		}
	}
	if !isFinite(m.Bias) || !isFinite(m.PlattA) || !isFinite(m.PlattB) {
		return errors.New("model bias or platt parameter is not finite")
	}
	if m.Iterations < 0 {
		return fmt.Errorf("model iterations %d is negative", m.Iterations)
	}
	return nil
}

// sigmoid is the logistic function, stable for large |t|. NaN stays NaN.
func sigmoid(t float64) float64 {
	if t >= 0 {
		return 1 / (1 + math.Exp(-t))
	}
	e := math.Exp(t)
	return e / (1 + e)
}

// logit is the raw score before Platt scaling. f must have len(m.Weights)
// values and m must be valid.
func (m LogisticModel) logit(f []float64) float64 {
	z := m.Bias
	for j, v := range f {
		z += m.Weights[j] * ((v - m.Means[j]) / m.Scales[j])
	}
	return z
}

// FitLogistic fits a standardized logistic regression on fit and Platt
// scaling on scale. Class weights balance positives and negatives within each
// set and multiply Example.Weight. Both fits are Newton solves; Platt scaling
// uses the regularized targets of plattTargets. The fit is deterministic. Both
// sets must hold both classes and finite features of one width. The caller
// decides what to do with a model whose Converged is false.
func FitLogistic(fit, scale []Example, opts LogisticOptions) (LogisticModel, error) {
	if err := opts.validate(); err != nil {
		return LogisticModel{}, fmt.Errorf("logistic options: %w", err)
	}
	fitWeights, err := exampleWeights("fit", fit)
	if err != nil {
		return LogisticModel{}, err
	}
	scaleWeights, err := exampleWeights("scale", scale)
	if err != nil {
		return LogisticModel{}, err
	}
	means, scales, x, err := standardize(fit)
	if err != nil {
		return LogisticModel{}, fmt.Errorf("fit set: %w", err)
	}
	d := len(means)
	for i, e := range scale {
		if len(e.Features) != d {
			return LogisticModel{}, fmt.Errorf("scale example %d has %d features, want %d", i, len(e.Features), d)
		}
		for j, v := range e.Features {
			if !isFinite(v) {
				return LogisticModel{}, fmt.Errorf("scale example %d feature %d is not finite (%v)", i, j, v)
			}
		}
	}
	weights, bias, iters, converged, err := descend(
		logisticProblem{x: x, y: labelTargets(exampleLabels(fit)), w: fitWeights, l2: opts.L2}, make([]float64, d), 0, opts)
	if err != nil {
		return LogisticModel{}, fmt.Errorf("fit solve: %w", err)
	}
	m := LogisticModel{
		FeatureVersion: impact.FeatureVersion,
		Means:          means,
		Scales:         scales,
		Weights:        weights,
		Bias:           bias,
		PlattA:         1,
		Iterations:     iters,
	}
	logits := make([]float64, len(scale))
	for i, e := range scale {
		logits[i] = m.logit(e.Features)
	}
	a, b, _, plattConverged, err := fitPlatt(logits, exampleLabels(scale), scaleWeights, opts)
	if err != nil {
		return LogisticModel{}, fmt.Errorf("platt scaling: %w", err)
	}
	m.PlattA, m.PlattB = a, b
	m.Converged = converged && plattConverged
	if err := m.validate(); err != nil {
		return LogisticModel{}, fmt.Errorf("fitted model: %w", err)
	}
	return m, nil
}

// Prob returns the calibrated probability for one feature vector. A vector
// of the wrong length or with a non-finite value, an invalid model, or a
// non-finite score is an error and the returned probability is NaN: an
// unknown probability is never 0.
func (m LogisticModel) Prob(features []float64) (float64, error) {
	if err := m.validate(); err != nil {
		return math.NaN(), fmt.Errorf("invalid logistic model: %w", err)
	}
	if len(features) != len(m.Weights) {
		return math.NaN(), fmt.Errorf("got %d features, want %d", len(features), len(m.Weights))
	}
	for j, v := range features {
		if !isFinite(v) {
			return math.NaN(), fmt.Errorf("feature %d is not finite (%v)", j, v)
		}
	}
	t := m.PlattA*m.logit(features) + m.PlattB
	if !isFinite(t) {
		return math.NaN(), fmt.Errorf("score is not finite (%v)", t)
	}
	return sigmoid(t), nil
}
