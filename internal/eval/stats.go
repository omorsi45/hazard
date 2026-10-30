// Package eval measures Hazard's selection configurations (the ablation of
// seed 12.2) on the held-out events of a population and writes the report.
package eval

import (
	"math"
	"math/rand/v2"
	"slices"
)

// SyntheticLabel names the mutation population wherever its numbers appear.
const SyntheticLabel = "synthetic (mutation labels)"

// NotAvailable is the text of a cell that has no measurement behind it.
const NotAvailable = "N/A"

// Metric names, in the order the tables list them.
const (
	MetricRecallCommit   = "recall_commit"
	MetricRecallFailure  = "recall_failure"
	MetricSelectionRatio = "selection_ratio"
	MetricTimeRatio      = "time_ratio"
	MetricAbstention     = "abstention_rate"
	MetricSaving         = "effective_saving"
	MetricBrier          = "brier"
	MetricTruncation     = "truncation_miss"
	MetricUSD            = "usd_per_commit"
	MetricLatency        = "latency_p95_ms"
)

// bins is the number of equal-width reliability bins (seed 12.1).
const bins = 10

// Confidence level of the percentile interval: the 2.5% and 97.5% quantiles.
const (
	ciLow  = 0.025
	ciHigh = 0.975
)

// Outcome is what one configuration did on one event. An abstention is the
// full suite: Selected is Total, CostS is CostT and every failing test is
// Caught.
type Outcome struct {
	Kind        string  // change kind of the event: "go" or "cfg"
	Failing     int     // |F(c)|
	Caught      int     // |F(c) intersect S(c)|
	Selected    int     // |S(c)|
	Total       int     // |T(c)|
	CostS       float64 // cost(S(c)) in ms
	CostT       float64 // cost(T(c)) in ms
	Overhead    float64 // ms spent selecting
	USD         float64 // model spend of the selection
	Abstained   bool
	TruncMissed int     // failing tests cut by the candidate cap
	BrierSum    float64 // sum of (p - y)^2 over the event's scored candidates
	BrierN      int     // scored candidates of the event
}

// Metric is one cell: a value with its percentile bootstrap interval, or the
// reason there is none. N counts what the value is over (events, failing
// tests or scored pairs); BootN the replicates that defined it.
type Metric struct {
	Value *float64 `json:"value"`
	Low   *float64 `json:"ci_low,omitempty"`
	High  *float64 `json:"ci_high,omitempty"`
	N     int      `json:"n"`
	BootN int      `json:"boot_n,omitempty"`
	NA    string   `json:"na,omitempty"`
}

// Bin is one equal-width bin of the reliability table. MeanP and Observed are
// nil for an empty bin: no pair, no rate.
type Bin struct {
	Index    int      `json:"bin"`
	Lo       float64  `json:"lo"`
	Hi       float64  `json:"hi"`
	Count    int      `json:"count"`
	MeanP    *float64 `json:"mean_p"`
	Observed *float64 `json:"observed_rate"`
}

// ProbLabel is one (change, test) pair: the predicted probability and whether
// the test failed.
type ProbLabel struct {
	P     float64
	Label bool
}

// ratio is num/den, or 1 (no saving) when the denominator is not positive.
func ratio(num, den float64) float64 {
	if !(den > 0) {
		return 1
	}
	return num / den
}

// tally accumulates the sums every metric is a function of, so the point
// estimate and each bootstrap replicate come from the same code.
type tally struct {
	n                 int
	positive, hit     int // events with a failing test, and those fully caught
	failing, caught   int
	selRatio, timeRat float64
	costS, costT      float64
	overhead, usd     float64
	abstained         int
	truncated         int
	brierSum          float64
	brierN            int
}

func (t *tally) add(o Outcome) {
	t.n++
	if o.Failing > 0 {
		t.positive++
		if o.Caught >= o.Failing {
			t.hit++
		}
	}
	t.failing += o.Failing
	t.caught += o.Caught
	t.selRatio += ratio(float64(o.Selected), float64(o.Total))
	t.timeRat += ratio(o.CostS, o.CostT)
	// Every event is added, abstentions included: an abstention costs the full
	// suite, and dropping it would inflate the saving.
	t.costS += o.CostS
	t.costT += o.CostT
	t.overhead += o.Overhead
	t.usd += o.USD
	if o.Abstained {
		t.abstained++
	}
	t.truncated += o.TruncMissed
	t.brierSum += o.BrierSum
	t.brierN += o.BrierN
}

// support is the number of items the metric name is over.
func (t *tally) support(name string) int {
	switch name {
	case MetricRecallCommit:
		return t.positive
	case MetricRecallFailure, MetricTruncation:
		return t.failing
	case MetricBrier:
		return t.brierN
	}
	return t.n
}

// value returns the metric, and false when its denominator is empty: nothing
// measured is not a zero.
func (t *tally) value(name string) (float64, bool) {
	if t.support(name) == 0 {
		return 0, false
	}
	n := float64(t.n)
	switch name {
	case MetricRecallCommit:
		return float64(t.hit) / float64(t.positive), true
	case MetricRecallFailure:
		return float64(t.caught) / float64(t.failing), true
	case MetricSelectionRatio:
		return t.selRatio / n, true
	case MetricTimeRatio:
		return t.timeRat / n, true
	case MetricAbstention:
		return float64(t.abstained) / n, true
	case MetricSaving:
		if !(t.costT > 0) {
			return 0, false
		}
		return 1 - (t.costS+t.overhead)/t.costT, true
	case MetricBrier:
		return t.brierSum / float64(t.brierN), true
	case MetricTruncation:
		return float64(t.truncated) / float64(t.failing), true
	case MetricUSD:
		return t.usd / n, true
	}
	return 0, false
}

// bootstrapped lists the metrics that carry a percentile interval; latency is
// a quantile of the overhead and USD a spend, both reported as point values.
func bootstrapped(name string) bool { return name != MetricLatency && name != MetricUSD }

// Summarize computes the named metrics over outcomes. Each metric that has an
// interval gets the percentile interval of reps bootstrap replicates: events
// are resampled with replacement, one draw per event in event order from a
// PCG(seed, 0) generator that every call starts afresh. The value is the
// full-sample one, never the mean of the replicates.
func Summarize(outcomes []Outcome, names []string, reps int, seed uint64) map[string]Metric {
	var point tally
	for _, o := range outcomes {
		point.add(o)
	}
	samples := map[string][]float64{}
	if n := len(outcomes); n > 0 {
		rng := rand.New(rand.NewPCG(seed, 0))
		for r := 0; r < reps; r++ {
			var rep tally
			for i := 0; i < n; i++ {
				rep.add(outcomes[rng.IntN(n)])
			}
			for _, name := range names {
				if v, ok := rep.value(name); ok && bootstrapped(name) {
					samples[name] = append(samples[name], v)
				}
			}
		}
	}

	out := make(map[string]Metric, len(names))
	for _, name := range names {
		if name == MetricLatency {
			out[name] = latency(outcomes)
			continue
		}
		v, ok := point.value(name)
		if !ok {
			out[name] = Metric{N: point.support(name), NA: NotAvailable}
			continue
		}
		m := Metric{Value: &v, N: point.support(name)}
		if s := samples[name]; len(s) > 0 && bootstrapped(name) {
			slices.Sort(s)
			lo, hi := percentile(s, ciLow), percentile(s, ciHigh)
			m.Low, m.High, m.BootN = &lo, &hi, len(s)
		}
		out[name] = m
	}
	return out
}

// latency is the 95th percentile of the time spent selecting.
func latency(outcomes []Outcome) Metric {
	if len(outcomes) == 0 {
		return Metric{NA: NotAvailable}
	}
	ms := make([]float64, len(outcomes))
	for i, o := range outcomes {
		ms[i] = o.Overhead
	}
	slices.Sort(ms)
	v := percentile(ms, 0.95)
	return Metric{Value: &v, N: len(outcomes)}
}

// percentile is the q-quantile of an ascending sample, interpolating linearly
// between the neighbours of position q*(n-1).
func percentile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return math.NaN()
	}
	pos := min(max(q, 0), 1) * float64(len(sorted)-1)
	lo := int(math.Floor(pos))
	hi := min(lo+1, len(sorted)-1)
	return sorted[lo] + (sorted[hi]-sorted[lo])*(pos-float64(lo))
}

// BinOf is the reliability bin of a probability: floor(p * 10), with 1.0 in
// the last bin.
func BinOf(p float64) int {
	return min(max(int(math.Floor(p*bins)), 0), bins-1)
}

// ReliabilityBins tabulates pairs into ten equal-width bins: how many pairs
// fall in each, their mean predicted probability and the rate at which the
// test failed. A bin without pairs has no mean and no rate.
func ReliabilityBins(pairs []ProbLabel) []Bin {
	var count [bins]int
	var sumP, sumY [bins]float64
	for _, pr := range pairs {
		b := BinOf(pr.P)
		count[b]++
		sumP[b] += pr.P
		if pr.Label {
			sumY[b]++
		}
	}
	out := make([]Bin, bins)
	for i := range out {
		out[i] = Bin{Index: i, Lo: float64(i) / bins, Hi: float64(i+1) / bins, Count: count[i]}
		if count[i] > 0 {
			mean, rate := sumP[i]/float64(count[i]), sumY[i]/float64(count[i])
			out[i].MeanP, out[i].Observed = &mean, &rate
		}
	}
	return out
}
