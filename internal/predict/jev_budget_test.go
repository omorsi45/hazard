package predict

import (
	"fmt"
	"math"
	"net/http"
	"strings"
	"testing"

	"github.com/omorsi45/hazard/internal/config"
)

// shardReservation returns the reservation of the first shard of a call.
func shardReservation(t *testing.T, j *Jev, n int) float64 {
	t.Helper()
	shards, err := j.Payload(goldenChange(), candidatesN(n))
	if err != nil || len(shards) == 0 {
		t.Fatalf("Payload: %d shards, %v", len(shards), err)
	}
	return reservation(&shard{body: shards[0]})
}

func TestJevBudgetReservationFormula(t *testing.T) {
	// 7000 bytes estimate 2000 tokens: 2000 / 1e6 * 0.042 * 1.5.
	got := reservation(&shard{body: make([]byte, 7000)})
	if want := 0.000126; math.Abs(got-want) > 1e-12 {
		t.Fatalf("reservation = %v, want %v", got, want)
	}
	if DefaultPricePerMTok != 0.042 {
		t.Fatalf("DefaultPricePerMTok = %v", DefaultPricePerMTok)
	}
}

// TestJevBudgetReservationStopsTheSecondShard is the budget guard: with room
// for one reservation, the second shard is not sent and its tests are 1.0 with
// the source budget_exhausted.
func TestJevBudgetReservationStopsTheSecondShard(t *testing.T) {
	f := newFake(t, answerAll(t, servedSnapshot, 0.001, constant(0.2)))
	probe := f.jev(t, func(o *JevOptions) { o.Config.QuestionsPerRequest = 2 })
	budget := shardReservation(t, probe, 4) * 1.2

	j := f.jev(t, func(o *JevOptions) {
		o.Config.QuestionsPerRequest = 2
		o.BudgetUSD = budget
	})
	probs, err := j.Predict(t.Context(), goldenChange(), candidatesN(4))
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	if f.count() != 1 {
		t.Fatalf("%d requests with room for one shard, want 1", f.count())
	}
	for i, p := range probs {
		switch {
		case i < 2 && (p.P != 0.2 || p.Source != "jev"):
			t.Fatalf("prob %d = %+v, want the answer of the first shard", i, p)
		case i >= 2 && (p.P != 1 || p.Source != "budget_exhausted" || p.Shard != -1):
			t.Fatalf("prob %d = %+v, want 1.0 budget_exhausted", i, p)
		}
	}
	st := j.LastStats()
	if st.BudgetSkipped != 1 || st.Requests != 1 || st.FailedShards != 0 {
		t.Fatalf("stats = %+v", st)
	}
	if !strings.Contains(strings.Join(st.Warnings, "\n"), "budget") {
		t.Fatalf("warnings %v do not mention the budget", st.Warnings)
	}
}

func TestJevBudgetStopsIssuingRequestsOnceExhausted(t *testing.T) {
	f := newFake(t, answerAll(t, servedSnapshot, 0.001, constant(0.2)))
	probe := f.jev(t, func(o *JevOptions) { o.Config.QuestionsPerRequest = 2 })
	budget := shardReservation(t, probe, 8) * 1.2

	j := f.jev(t, func(o *JevOptions) {
		o.Config.QuestionsPerRequest = 2
		o.BudgetUSD = budget
	})
	probs, err := j.Predict(t.Context(), goldenChange(), candidatesN(8))
	if err != nil {
		t.Fatal(err)
	}
	if f.count() != 1 {
		t.Fatalf("%d requests, want 1: once the budget is out no later shard is sent", f.count())
	}
	skipped := 0
	for _, p := range probs {
		if p.Source == "budget_exhausted" {
			skipped++
		}
	}
	if skipped != 6 || j.LastStats().BudgetSkipped != 3 {
		t.Fatalf("%d candidates and %d shards skipped, want 6 and 3", skipped, j.LastStats().BudgetSkipped)
	}
}

func TestJevBudgetActualCostReplacesTheReservation(t *testing.T) {
	f := newFake(t, answerAll(t, servedSnapshot, 0.5, constant(0.2)))
	probe := f.jev(t, func(o *JevOptions) { o.Config.QuestionsPerRequest = 2 })
	// Room for several reservations, far less than the 0.5 the first shard bills.
	budget := shardReservation(t, probe, 4) * 10

	j := f.jev(t, func(o *JevOptions) {
		o.Config.QuestionsPerRequest = 2
		o.Config.Concurrency = 1
		o.BudgetUSD = budget
	})
	probs, err := j.Predict(t.Context(), goldenChange(), candidatesN(4))
	if err != nil {
		t.Fatal(err)
	}
	if f.count() != 1 || probs[2].Source != "budget_exhausted" {
		t.Fatalf("%d requests, prob 2 = %+v: the billed cost must count against the budget", f.count(), probs[2])
	}
	if got := j.LastStats().CostUSD; got != 0.5 {
		t.Fatalf("CostUSD = %v, want 0.5", got)
	}
}

func TestJevBudgetCostIsTheSumOfUsageCost(t *testing.T) {
	f := newFake(t, func(call int, body []byte, w http.ResponseWriter, r *http.Request) {
		answerAll(t, servedSnapshot, float64(call)*0.0001, constant(0.2))(call, body, w, r)
	})
	j := f.jev(t, func(o *JevOptions) {
		o.Config.QuestionsPerRequest = 2
		o.Config.Concurrency = 1
	})
	probs, err := j.Predict(t.Context(), goldenChange(), candidatesN(6))
	if err != nil {
		t.Fatal(err)
	}
	st := j.LastStats()
	if math.Abs(st.CostUSD-0.0006) > 1e-12 {
		t.Fatalf("CostUSD = %v, want 0.0006", st.CostUSD)
	}
	for i, p := range probs {
		want := float64(i/2+1) * 0.0001 / 2
		if math.Abs(p.CostUSD-want) > 1e-12 {
			t.Fatalf("prob %d cost = %v, want its share %v", i, p.CostUSD, want)
		}
	}
}

func TestJevBudgetUnusableUsageCostCountsTheReservation(t *testing.T) {
	cases := map[string]string{
		"no usage":            `{"model":"%s","answers":{"t_001":{"type":"noul","noul":0.3},"t_002":{"type":"noul","noul":0.3}}}`,
		"no cost":             `{"model":"%s","answers":{"t_001":{"type":"noul","noul":0.3},"t_002":{"type":"noul","noul":0.3}},"usage":{"input_tokens":10}}`,
		"cost is text":        `{"model":"%s","answers":{"t_001":{"type":"noul","noul":0.3},"t_002":{"type":"noul","noul":0.3}},"usage":{"cost":"cheap"}}`,
		"cost is null":        `{"model":"%s","answers":{"t_001":{"type":"noul","noul":0.3},"t_002":{"type":"noul","noul":0.3}},"usage":{"cost":null}}`,
		"negative cost":       `{"model":"%s","answers":{"t_001":{"type":"noul","noul":0.3},"t_002":{"type":"noul","noul":0.3}},"usage":{"cost":-1}}`,
		"usage not an object": `{"model":"%s","answers":{"t_001":{"type":"noul","noul":0.3},"t_002":{"type":"noul","noul":0.3}},"usage":7}`,
	}
	for name, template := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFake(t, func(_ int, _ []byte, w http.ResponseWriter, _ *http.Request) {
				fmt.Fprintf(w, template, servedSnapshot)
			})
			j := f.jev(t, nil)
			want := shardReservation(t, j, 2)
			probs, err := j.Predict(t.Context(), goldenChange(), candidatesN(2))
			if err != nil {
				t.Fatal(err)
			}
			if probs[0].P != 0.3 || probs[0].Source != "jev" {
				t.Fatalf("prob = %+v, the answers are still good", probs[0])
			}
			st := j.LastStats()
			if math.Abs(st.CostUSD-want) > 1e-12 {
				t.Fatalf("CostUSD = %v, want the reservation %v", st.CostUSD, want)
			}
			if !strings.Contains(strings.Join(st.Warnings, "\n"), "usage.cost") {
				t.Fatalf("warnings %v do not mention usage.cost", st.Warnings)
			}
		})
	}
}

func TestJevBudgetFixtureWithoutCost(t *testing.T) {
	f := newFake(t, func(_ int, _ []byte, w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(readFixture(t, "response-no-cost.json"))
	})
	j := f.jev(t, nil)
	probs, err := j.Predict(t.Context(), goldenChange(), goldenCandidates())
	if err != nil {
		t.Fatal(err)
	}
	if probs[0].P != 0.4 || probs[1].P != 0.2 {
		t.Fatalf("probs = %+v", probs)
	}
	shards, _ := j.Payload(goldenChange(), goldenCandidates())
	if want := reservation(&shard{body: shards[0]}); math.Abs(j.LastStats().CostUSD-want) > 1e-12 {
		t.Fatalf("CostUSD = %v, want %v", j.LastStats().CostUSD, want)
	}
}

func TestJevBudgetOptionOverridesTheConfiguration(t *testing.T) {
	f := newFake(t, answerAll(t, servedSnapshot, 0.0001, constant(0.2)))
	tiny := func(o *JevOptions) { o.Config.BudgetUSDPerRun = 1e-12 }

	cfgOnly := f.jev(t, tiny)
	probs, err := cfgOnly.Predict(t.Context(), goldenChange(), candidatesN(2))
	if err != nil {
		t.Fatal(err)
	}
	if f.count() != 0 || probs[0].Source != "budget_exhausted" {
		t.Fatalf("%d requests, prob %+v: the configured cap must apply when the option is zero", f.count(), probs[0])
	}

	overridden := f.jev(t, func(o *JevOptions) {
		tiny(o)
		o.BudgetUSD = 1
	})
	probs, err = overridden.Predict(t.Context(), goldenChange(), candidatesN(2))
	if err != nil {
		t.Fatal(err)
	}
	if f.count() != 1 || probs[0].Source != "jev" {
		t.Fatalf("%d requests, prob %+v: the option must override the configuration", f.count(), probs[0])
	}
}

func TestJevBudgetOfZeroSendsNothing(t *testing.T) {
	f := newFake(t, answerAll(t, servedSnapshot, 0.0001, constant(0.2)))
	j := f.jev(t, func(o *JevOptions) { o.Config.BudgetUSDPerRun = 0 })
	probs, err := j.Predict(t.Context(), goldenChange(), candidatesN(3))
	if err != nil {
		t.Fatal(err)
	}
	if f.count() != 0 {
		t.Fatalf("%d requests with a budget of zero", f.count())
	}
	for _, p := range probs {
		if p.P != 1 || p.Source != "budget_exhausted" {
			t.Fatalf("prob = %+v", p)
		}
	}
}

func TestJevBudgetRejectsInvalidAmounts(t *testing.T) {
	for _, v := range []float64{math.NaN(), math.Inf(1)} {
		if _, err := NewJev(JevOptions{Config: jevConfig(), Privacy: config.Defaults().Privacy, APIKey: testKey, BudgetUSD: v}); err == nil {
			t.Fatalf("NewJev accepted the budget %v", v)
		}
	}
}
