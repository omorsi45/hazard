package eval

import (
	"math"
	"math/rand/v2"
	"slices"
	"testing"
)

const tol = 1e-12

func near(a, b float64) bool { return math.Abs(a-b) <= tol }

// valueOf returns the value of metric name, failing when it has none.
func valueOf(t *testing.T, m map[string]Metric, name string) float64 {
	t.Helper()
	got, ok := m[name]
	if !ok {
		t.Fatalf("metric %s is missing from %v", name, m)
	}
	if got.Value == nil {
		t.Fatalf("metric %s has no value (na=%q), want one", name, got.NA)
	}
	return *got.Value
}

func wantValue(t *testing.T, m map[string]Metric, name string, want float64) {
	t.Helper()
	if got := valueOf(t, m, name); !near(got, want) {
		t.Errorf("%s = %.15g, want %.15g", name, got, want)
	}
}

func wantNA(t *testing.T, m map[string]Metric, name string) {
	t.Helper()
	got, ok := m[name]
	if !ok {
		t.Fatalf("metric %s is missing from %v", name, m)
	}
	if got.Value != nil || got.Low != nil || got.High != nil || got.NA != NotAvailable {
		t.Errorf("%s = %+v, want no value and na=%q", name, got, NotAvailable)
	}
}

var allMetricNames = []string{
	MetricRecallCommit, MetricRecallFailure, MetricSelectionRatio, MetricTimeRatio,
	MetricAbstention, MetricSaving, MetricBrier, MetricTruncation, MetricUSD, MetricLatency,
}

// Recall: an event without a failing test is not in the denominator of R_c or
// R_f, but it still counts for every other metric.
func TestRecallLeavesEventsWithoutFailuresOutOfTheDenominator(t *testing.T) {
	outcomes := []Outcome{
		{Failing: 2, Caught: 2, Selected: 3, Total: 10, CostS: 30, CostT: 100},
		{Failing: 1, Caught: 0, Selected: 1, Total: 10, CostS: 10, CostT: 100},
		{Failing: 0, Caught: 0, Selected: 2, Total: 10, CostS: 20, CostT: 100},
	}
	got := Summarize(outcomes, allMetricNames, 10, 1)
	wantValue(t, got, MetricRecallCommit, 1.0/2) // one of the two events with failures was fully caught
	wantValue(t, got, MetricRecallFailure, 2.0/3)
	if n := got[MetricRecallCommit].N; n != 2 {
		t.Errorf("recall_commit n = %d, want the 2 events with a failing test", n)
	}
	if n := got[MetricRecallFailure].N; n != 3 {
		t.Errorf("recall_failure n = %d, want the 3 failing tests", n)
	}
}

func TestSelectionAndTimeRatioAreMeansOfPerEventRatios(t *testing.T) {
	outcomes := []Outcome{
		{Selected: 3, Total: 10, CostS: 30, CostT: 100},
		{Selected: 1, Total: 10, CostS: 10, CostT: 100},
		{Selected: 2, Total: 10, CostS: 50, CostT: 100},
	}
	got := Summarize(outcomes, allMetricNames, 10, 1)
	wantValue(t, got, MetricSelectionRatio, 0.2)
	wantValue(t, got, MetricTimeRatio, 0.3)
}

// Guard 2: the effective saving is over ALL events. An abstention runs the
// full suite, so it costs cost(T) and it stays in the denominator.
func TestEffectiveSavingCountsAbstentionsAndOverhead(t *testing.T) {
	outcomes := []Outcome{
		{Selected: 1, Total: 10, CostS: 10, CostT: 100, Overhead: 2},
		{Selected: 3, Total: 10, CostS: 30, CostT: 100, Overhead: 4},
		{Selected: 10, Total: 10, CostS: 100, CostT: 100, Overhead: 6, Abstained: true},
		{Selected: 10, Total: 10, CostS: 100, CostT: 100, Overhead: 8, Abstained: true},
	}
	got := Summarize(outcomes, allMetricNames, 10, 1)
	// 1 - (E[cost S] + E[overhead]) / E[cost T] = 1 - (240 + 20) / 400
	wantValue(t, got, MetricSaving, 0.35)
	wantValue(t, got, MetricAbstention, 0.5)
	wantValue(t, got, MetricTimeRatio, (0.1+0.3+1+1)/4)
}

func TestBrierTruncationUSDAndLatency(t *testing.T) {
	outcomes := make([]Outcome, 0, 20)
	for i := 1; i <= 20; i++ {
		outcomes = append(outcomes, Outcome{Total: 4, Selected: 1, CostS: 1, CostT: 4, Overhead: float64(i)})
	}
	outcomes[0].BrierSum, outcomes[0].BrierN = 0.01+0.01, 2 // (0.1, 0) and (0.9, 1)
	outcomes[1].BrierSum, outcomes[1].BrierN = 0.25, 1      // (0.5, 1)
	outcomes[2].Failing, outcomes[2].Caught, outcomes[2].TruncMissed = 3, 3, 0
	outcomes[3].Failing, outcomes[3].Caught, outcomes[3].TruncMissed = 1, 0, 1
	outcomes[0].USD, outcomes[1].USD = 0.02, 0.04

	got := Summarize(outcomes, allMetricNames, 10, 1)
	wantValue(t, got, MetricBrier, 0.27/3)
	if n := got[MetricBrier].N; n != 3 {
		t.Errorf("brier n = %d, want the 3 pairs", n)
	}
	wantValue(t, got, MetricTruncation, 1.0/4)
	wantValue(t, got, MetricUSD, 0.06/20)
	// p95 of 1..20 by linear interpolation: position 0.95*19 = 18.05
	wantValue(t, got, MetricLatency, 19.05)
	if got[MetricLatency].Low != nil || got[MetricUSD].Low != nil {
		t.Errorf("latency and usd are point estimates, got intervals %+v %+v", got[MetricLatency], got[MetricUSD])
	}
}

// Guard 3 at its root: nothing measured is N/A, never zero.
func TestEmptyPopulationIsNotAvailableNeverZero(t *testing.T) {
	got := Summarize(nil, allMetricNames, 50, 1)
	for _, name := range allMetricNames {
		wantNA(t, got, name)
	}
}

func TestMetricsWithoutTheirDenominatorAreNotAvailable(t *testing.T) {
	survivors := []Outcome{
		{Selected: 1, Total: 10, CostS: 10, CostT: 100},
		{Selected: 2, Total: 10, CostS: 20, CostT: 100},
	}
	got := Summarize(survivors, allMetricNames, 20, 1)
	wantNA(t, got, MetricRecallCommit)
	wantNA(t, got, MetricRecallFailure)
	wantNA(t, got, MetricTruncation)
	wantNA(t, got, MetricBrier)
	wantValue(t, got, MetricAbstention, 0) // measured: none of the 2 events abstained
	wantValue(t, got, MetricSelectionRatio, 0.15)
}

func TestPercentile(t *testing.T) {
	sample := []float64{1, 2, 3, 4, 5}
	for _, tc := range []struct{ q, want float64 }{{0, 1}, {0.25, 2}, {0.5, 3}, {1, 5}, {0.025, 1.1}, {0.975, 4.9}} {
		if got := percentile(sample, tc.q); !near(got, tc.want) {
			t.Errorf("percentile(%v) = %.15g, want %.15g", tc.q, got, tc.want)
		}
	}
	if got := percentile([]float64{7}, 0.975); got != 7 {
		t.Errorf("percentile of one value = %v, want it", got)
	}
}

// mixedOutcomes is a sample whose recall varies between resamples.
func mixedOutcomes() []Outcome {
	out := make([]Outcome, 0, 12)
	for i := 0; i < 12; i++ {
		o := Outcome{Failing: 1, Caught: 1, Selected: 2, Total: 10, CostS: 20, CostT: 100}
		if i%3 == 0 {
			o.Caught = 0
		}
		out = append(out, o)
	}
	return out
}

// The interval is the percentile interval of the recall over reps resamples of
// the events drawn with PCG(seed, 0), one draw per event in event order. The
// point estimate stays the full-sample value.
func TestBootstrapIsThePercentileIntervalOfSeededResamples(t *testing.T) {
	outcomes := mixedOutcomes()
	const reps, seed = 200, 7
	got := Summarize(outcomes, []string{MetricRecallCommit}, reps, seed)[MetricRecallCommit]

	rng := rand.New(rand.NewPCG(seed, 0))
	var samples []float64
	for r := 0; r < reps; r++ {
		hits := 0
		for i := 0; i < len(outcomes); i++ {
			if outcomes[rng.IntN(len(outcomes))].Caught == 1 {
				hits++
			}
		}
		samples = append(samples, float64(hits)/float64(len(outcomes)))
	}
	slices.Sort(samples)

	if got.Value == nil || !near(*got.Value, 8.0/12) {
		t.Fatalf("point estimate = %v, want the full-sample 8/12", got.Value)
	}
	if got.Low == nil || got.High == nil || !near(*got.Low, percentile(samples, 0.025)) || !near(*got.High, percentile(samples, 0.975)) {
		t.Errorf("interval = [%v, %v], want [%v, %v]", ptr(got.Low), ptr(got.High), percentile(samples, 0.025), percentile(samples, 0.975))
	}
	if got.BootN != reps {
		t.Errorf("boot_n = %d, want %d", got.BootN, reps)
	}
}

func ptr(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

func TestBootstrapIsDeterministicForAFixedSeed(t *testing.T) {
	outcomes := mixedOutcomes()
	a := Summarize(outcomes, allMetricNames, 100, 3)
	b := Summarize(outcomes, allMetricNames, 100, 3)
	for _, name := range allMetricNames {
		if !metricEqual(a[name], b[name]) {
			t.Errorf("%s differs between runs with the same seed: %+v vs %+v", name, a[name], b[name])
		}
	}
	c := Summarize(outcomes, []string{MetricRecallCommit}, 100, 4)
	if metricEqual(a[MetricRecallCommit], c[MetricRecallCommit]) {
		t.Errorf("another seed gave the same interval %+v", c[MetricRecallCommit])
	}
}

func metricEqual(a, b Metric) bool {
	return ptr(a.Value) == ptr(b.Value) && ptr(a.Low) == ptr(b.Low) && ptr(a.High) == ptr(b.High) && a.N == b.N && a.BootN == b.BootN && a.NA == b.NA
}

func TestBootstrapOfIdenticalEventsCollapsesToThePoint(t *testing.T) {
	outcomes := make([]Outcome, 9)
	for i := range outcomes {
		outcomes[i] = Outcome{Failing: 1, Caught: 1, Selected: 1, Total: 4, CostS: 1, CostT: 4}
	}
	m := Summarize(outcomes, []string{MetricRecallCommit, MetricSaving}, 100, 1)
	rc := m[MetricRecallCommit]
	if rc.Value == nil || *rc.Value != 1 || rc.Low == nil || *rc.Low != 1 || rc.High == nil || *rc.High != 1 {
		t.Errorf("recall_commit = %+v, want 1 [1, 1]", rc)
	}
	wantValue(t, m, MetricSaving, 0.75)
}

func TestBinOfPlacesTheEdges(t *testing.T) {
	for _, tc := range []struct {
		p    float64
		want int
	}{{0, 0}, {0.0999, 0}, {0.1, 1}, {0.5, 5}, {0.95, 9}, {0.9999, 9}, {1, 9}} {
		if got := BinOf(tc.p); got != tc.want {
			t.Errorf("BinOf(%v) = %d, want %d", tc.p, got, tc.want)
		}
	}
}

func TestReliabilityBinsCountMeanAndObservedRate(t *testing.T) {
	pairs := []ProbLabel{
		{0.0, false}, {0.05, false}, {0.15, true}, {0.5, false}, {0.95, true}, {1.0, false},
	}
	bins := ReliabilityBins(pairs)
	if len(bins) != 10 {
		t.Fatalf("got %d bins, want 10", len(bins))
	}
	type want struct {
		count          int
		meanP, observe float64
	}
	for index, w := range map[int]want{
		0: {2, 0.025, 0}, 1: {1, 0.15, 1}, 5: {1, 0.5, 0}, 9: {2, 0.975, 0.5},
	} {
		b := bins[index]
		if b.Index != index || b.Count != w.count || b.MeanP == nil || b.Observed == nil || !near(*b.MeanP, w.meanP) || !near(*b.Observed, w.observe) {
			t.Errorf("bin %d = %+v mean=%v obs=%v, want count %d mean %v observed %v", index, b, ptr(b.MeanP), ptr(b.Observed), w.count, w.meanP, w.observe)
		}
	}
	for _, index := range []int{2, 3, 4, 6, 7, 8} {
		b := bins[index]
		if b.Count != 0 || b.MeanP != nil || b.Observed != nil {
			t.Errorf("empty bin %d = %+v, want count 0 and no mean or rate (not zero)", index, b)
		}
	}
	if !near(bins[3].Lo, 0.3) || !near(bins[3].Hi, 0.4) {
		t.Errorf("bin 3 spans [%v, %v), want [0.3, 0.4)", bins[3].Lo, bins[3].Hi)
	}
}
