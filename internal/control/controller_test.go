package control

import (
	"fmt"
	"maps"
	"math"
	"math/rand/v2"
	"reflect"
	"slices"
	"strconv"
	"testing"
)

// baseInput is a verified-lock input that narrows: lambda 0.3, twenty tests
// (t00..t19) of 10 ms each in four packages of five (pkg0..pkg3), nothing
// certain, no candidates and no abstention condition.
func baseInput() Input {
	all := make([]string, 20)
	ms := make(map[string]int, 20)
	pkg := make(map[string]string, 20)
	for i := range all {
		id := fmt.Sprintf("t%02d", i)
		all[i] = id
		ms[id] = 10
		pkg[id] = fmt.Sprintf("pkg%d", i/5)
	}
	return Input{
		Lambda:          0.3,
		AllTests:        all,
		TestMS:          ms,
		TestPackage:     pkg,
		MinSavingRatio:  0.2,
		ChangedPackages: 1,
		PackageCount:    4,
	}
}

// withCandidates returns in with cands as its residual candidates and a raw
// count that matches them (nothing truncated).
func withCandidates(in Input, cands ...Candidate) Input {
	in.Candidates = cands
	in.RawCandidates = len(cands)
	in.Truncated = 0
	return in
}

// describe renders the members as "id:source" or "id:source:p".
func describe(d Decision) []string {
	out := make([]string, 0, len(d.Members))
	for _, m := range d.Members {
		s := m.TestID + ":" + m.Source
		if m.P != nil {
			s += ":" + strconv.FormatFloat(*m.P, 'g', -1, 64)
		}
		out = append(out, s)
	}
	return out
}

func requireMembers(t *testing.T, d Decision, want ...string) {
	t.Helper()
	got := describe(d)
	if want == nil {
		want = []string{}
	}
	if !slices.Equal(got, want) {
		t.Errorf("members = %q, want %q", got, want)
	}
}

func TestDecideRuleOneDemandsTheFullSuiteBeforeSelection(t *testing.T) {
	tests := []struct {
		name string
		set  func(*Input)
		want string
	}{
		{"trigger", func(in *Input) { in.TriggersFull = []string{"go.mod:go.mod"} }, "trigger"},
		{"full reason is passed through", func(in *Input) { in.FullReason = "graph_error" }, "graph_error"},
		{"no_lock", func(in *Input) { in.LockReason = "no_lock" }, "no_lock"},
		{"lock_corrupt", func(in *Input) { in.LockReason = "lock_corrupt" }, "lock_corrupt"},
		{"lock_error", func(in *Input) { in.LockReason = "lock_error" }, "lock_error"},
		{"model_drift", func(in *Input) { in.LockReason = "model_drift" }, "model_drift"},
		{"rubric_mismatch", func(in *Input) { in.LockReason = "rubric_mismatch" }, "rubric_mismatch"},
		{"feature_mismatch", func(in *Input) { in.LockReason = "feature_mismatch" }, "feature_mismatch"},
		{"policy_mismatch", func(in *Input) { in.LockReason = "policy_mismatch" }, "policy_mismatch"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := withCandidates(baseInput(), Candidate{TestID: "t10", P: 1}, Candidate{TestID: "t11", P: 1})
			in.Certain = []string{"t01"}
			tc.set(&in)
			d := Decide(in)
			if !d.FullSuite || d.Reason != tc.want {
				t.Fatalf("FullSuite=%v Reason=%q, want full with %q", d.FullSuite, d.Reason, tc.want)
			}
			if len(d.Members) != 0 {
				t.Errorf("members = %q, want none: the full suite was demanded before any selection", describe(d))
			}
		})
	}
}

func TestDecideRuleOnePrecedence(t *testing.T) {
	tests := []struct {
		name                      string
		triggers                  []string
		fullReason, lockReason, w string
	}{
		{"trigger beats full reason and lock", []string{"go.mod:go.mod"}, "graph_error", "no_lock", "trigger"},
		{"full reason beats lock", nil, "graph_error", "no_lock", "graph_error"},
		{"lock alone", nil, "", "model_drift", "model_drift"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInput()
			in.TriggersFull, in.FullReason, in.LockReason = tc.triggers, tc.fullReason, tc.lockReason
			// Rule 3 conditions are also true; rule 1 still wins.
			in.Drift = DriftStatus{State: "alarm", Alarm: true}
			in.CoverageStale, in.CoverageRequired = true, true
			if d := Decide(in); !d.FullSuite || d.Reason != tc.w {
				t.Errorf("FullSuite=%v Reason=%q, want full with %q", d.FullSuite, d.Reason, tc.w)
			}
		})
	}
}

// G07 at controller level: a full-suite trigger wins over any probability.
func TestG07TriggerBeatsEveryProbability(t *testing.T) {
	in := withCandidates(baseInput(),
		Candidate{TestID: "t10", P: 1}, Candidate{TestID: "t11", P: 1}, Candidate{TestID: "t12", P: 1})
	in.TriggersFull = []string{"go.mod:go.mod"}
	d := Decide(in)
	if !d.FullSuite || d.Reason != "trigger" {
		t.Fatalf("FullSuite=%v Reason=%q, want full with trigger", d.FullSuite, d.Reason)
	}
}

// G01 to G04 at decision level: each failed lock check abstains to the full
// suite with that reason, whatever the probabilities say.
func TestG01ToG04LockFailureAbstains(t *testing.T) {
	for _, reason := range []string{"lock_corrupt", "model_drift", "rubric_mismatch", "feature_mismatch"} {
		t.Run(reason, func(t *testing.T) {
			in := withCandidates(baseInput(), Candidate{TestID: "t10", P: 0.9})
			in.Certain = []string{"t01"}
			in.LockReason = reason
			d := Decide(in)
			if !d.FullSuite || d.Reason != reason {
				t.Fatalf("FullSuite=%v Reason=%q, want full with %q", d.FullSuite, d.Reason, reason)
			}
		})
	}
}

func TestDecideUnionSources(t *testing.T) {
	in := withCandidates(baseInput(),
		Candidate{TestID: "t05", P: 0.9},
		Candidate{TestID: "t06", P: 0.3},  // exactly lambda: selected
		Candidate{TestID: "t07", P: 0.29}, // below lambda: left out
		Candidate{TestID: "t08", P: 0, Failed: true},
		Candidate{TestID: "t01", P: 0.8}, // also certain: stays certain
		Candidate{TestID: "t09", P: 0.1},
		Candidate{TestID: "t04", P: 0.95}, // also always-run: stays always_run
	)
	in.Certain = []string{"t03", "t01"}
	in.AlwaysRun = []string{"t02", "t01", "t04"}
	d := Decide(in)
	if d.FullSuite {
		t.Fatalf("unexpected full suite: %q", d.Reason)
	}
	requireMembers(t, d,
		"t01:certain", "t02:always_run", "t03:certain", "t04:always_run",
		"t05:added:0.9", "t06:added:0.3", "t08:added:1")
}

func TestDecideFailedCandidateIsAlwaysAddedWithPOne(t *testing.T) {
	in := withCandidates(baseInput(),
		Candidate{TestID: "t10", P: 0, Failed: true},
		Candidate{TestID: "t11", P: 0.01},
		Candidate{TestID: "t12", P: 0.2, Failed: true},
		Candidate{TestID: "t13", P: 0.01},
		Candidate{TestID: "t14", P: 0.01},
		Candidate{TestID: "t15", P: 0.01},
		Candidate{TestID: "t16", P: 0.01},
		Candidate{TestID: "t17", P: 0.01},
		Candidate{TestID: "t18", P: 0.01},
	)
	requireMembers(t, Decide(in), "t10:added:1", "t12:added:1")
}

func TestDecideMalformedProbabilityIsNeverDropped(t *testing.T) {
	for _, p := range []float64{math.NaN(), -0.1, 1.1, math.Inf(1), math.Inf(-1)} {
		t.Run(fmt.Sprint(p), func(t *testing.T) {
			in := withCandidates(baseInput(),
				Candidate{TestID: "t10", P: p},
				Candidate{TestID: "t11", P: 0}, Candidate{TestID: "t12", P: 0}, Candidate{TestID: "t13", P: 0},
				Candidate{TestID: "t14", P: 0}, Candidate{TestID: "t15", P: 0}, Candidate{TestID: "t16", P: 0},
				Candidate{TestID: "t17", P: 0}, Candidate{TestID: "t18", P: 0}, Candidate{TestID: "t19", P: 0},
			)
			requireMembers(t, Decide(in), "t10:added:1")
		})
	}
}

// G05 monotone authority: a predictor that says 0 for everything cannot
// remove a CERTAIN or always-run test.
func TestG05MonotoneAuthority(t *testing.T) {
	in := withCandidates(baseInput(),
		Candidate{TestID: "t04", P: 0}, Candidate{TestID: "t05", P: 0}, Candidate{TestID: "t06", P: 0},
		Candidate{TestID: "t07", P: 0}, Candidate{TestID: "t08", P: 0}, Candidate{TestID: "t09", P: 0},
	)
	in.Certain = []string{"t01", "t02"}
	in.AlwaysRun = []string{"t03"}
	d := Decide(in)
	if d.FullSuite {
		t.Fatalf("unexpected full suite: %q", d.Reason)
	}
	requireMembers(t, d, "t01:certain", "t02:certain", "t03:always_run")
}

func TestDecideMembersAreSortedAndUnique(t *testing.T) {
	in := withCandidates(baseInput(),
		Candidate{TestID: "t12", P: 1}, Candidate{TestID: "t03", P: 1}, Candidate{TestID: "t12", P: 0.5},
		Candidate{TestID: "t09", P: 1},
	)
	in.Certain = []string{"t15", "t02", "t15"}
	in.AlwaysRun = []string{"t11", "t02", "t00"}
	d := Decide(in)
	var ids []string
	for _, m := range d.Members {
		ids = append(ids, m.TestID)
	}
	want := []string{"t00", "t02", "t03", "t09", "t11", "t12", "t15"}
	if !slices.Equal(ids, want) {
		t.Errorf("member IDs = %q, want sorted and unique %q", ids, want)
	}
}

// A duplicated candidate ID is a caller bug, but the answer must not depend
// on which copy comes first: the higher probability is the one recorded.
func TestDecideDuplicateCandidateKeepsTheHighestP(t *testing.T) {
	for _, order := range [][2]float64{{0.9, 0.4}, {0.4, 0.9}} {
		in := withCandidates(baseInput(),
			Candidate{TestID: "t10", P: order[0]}, Candidate{TestID: "t10", P: order[1]})
		requireMembers(t, Decide(in), "t10:added:0.9")
	}
}

func TestDecideTierModuleWidensToEveryTestOfASelectedPackage(t *testing.T) {
	in := withCandidates(baseInput(), Candidate{TestID: "t06", P: 0.9}, Candidate{TestID: "t07", P: 0.1})
	in.Certain = []string{"t01"}
	in.Tier = "module"
	d := Decide(in)
	if d.FullSuite {
		t.Fatalf("unexpected full suite: %q", d.Reason)
	}
	// pkg0 (t00..t04) because t01 is certain, pkg1 (t05..t09) because t06 is added.
	requireMembers(t, d,
		"t00:tier", "t01:certain", "t02:tier", "t03:tier", "t04:tier",
		"t05:tier", "t06:added:0.9", "t07:tier", "t08:tier", "t09:tier")
}

func TestDecideTierModuleWidensFromAlwaysRunToo(t *testing.T) {
	in := baseInput()
	in.AlwaysRun = []string{"t17"}
	in.Tier = "module"
	requireMembers(t, Decide(in), "t15:tier", "t16:tier", "t17:always_run", "t18:tier", "t19:tier")
}

func TestDecideTierModuleSkipsSelectedTestsWithoutAPackage(t *testing.T) {
	in := baseInput()
	in.Certain = []string{"t01", "unknown-test"}
	in.Tier = "module"
	d := Decide(in)
	requireMembers(t, d, "t00:tier", "t01:certain", "t02:tier", "t03:tier", "t04:tier", "unknown-test:certain")
}

func TestDecideTierSuiteAbstainsAndKeepsTheNarrowedSelection(t *testing.T) {
	in := withCandidates(baseInput(), Candidate{TestID: "t06", P: 0.9})
	in.Certain = []string{"t01"}
	in.Tier = "suite"
	d := Decide(in)
	if !d.FullSuite || d.Reason != "tier_suite" {
		t.Fatalf("FullSuite=%v Reason=%q, want full with tier_suite", d.FullSuite, d.Reason)
	}
	requireMembers(t, d, "t01:certain", "t06:added:0.9")
}

func TestDecideTierTargetedAndUnknownChangeNothing(t *testing.T) {
	for _, tier := range []string{"", "targeted", "something-else"} {
		t.Run("tier="+tier, func(t *testing.T) {
			in := withCandidates(baseInput(), Candidate{TestID: "t06", P: 0.9})
			in.Certain = []string{"t01"}
			in.Tier = tier
			d := Decide(in)
			if d.FullSuite {
				t.Fatalf("unexpected full suite: %q", d.Reason)
			}
			requireMembers(t, d, "t01:certain", "t06:added:0.9")
		})
	}
}

// certainIDs returns the first n inventory IDs of baseInput: t00 .. t(n-1).
func certainIDs(n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("t%02d", i)
	}
	return ids
}

// scoredCandidates returns total candidates t05.. with P = 0, the first
// `failed` of them failed.
func scoredCandidates(total, failed int) []Candidate {
	cands := make([]Candidate, total)
	for i := range cands {
		cands[i] = Candidate{TestID: fmt.Sprintf("t%02d", 5+i), Failed: i < failed}
	}
	return cands
}

func TestDecideAbstentionBoundaries(t *testing.T) {
	zeroCost := func(in *Input) {
		for id := range in.TestMS {
			in.TestMS[id] = 0
		}
	}
	tests := []struct {
		name string
		set  func(*Input)
		want string
	}{
		{"coverage stale and required", func(in *Input) { in.CoverageStale, in.CoverageRequired = true, true }, "coverage_stale"},
		{"coverage stale but not required", func(in *Input) { in.CoverageStale = true }, ""},
		{"coverage required but fresh", func(in *Input) { in.CoverageRequired = true }, ""},

		{"one of four candidates failed is exactly 25%", func(in *Input) { *in = withCandidates(*in, scoredCandidates(4, 1)...) }, ""},
		{"two of eight candidates failed is exactly 25%", func(in *Input) { *in = withCandidates(*in, scoredCandidates(8, 2)...) }, ""},
		{"two of four candidates failed", func(in *Input) { *in = withCandidates(*in, scoredCandidates(4, 2)...) }, "predictor_failure"},
		{"three of nine candidates failed", func(in *Input) { *in = withCandidates(*in, scoredCandidates(9, 3)...) }, "predictor_failure"},
		{"the only candidate failed", func(in *Input) { *in = withCandidates(*in, scoredCandidates(1, 1)...) }, "predictor_failure"},
		{"invalid probabilities count as failed", func(in *Input) {
			*in = withCandidates(*in, scoredCandidates(4, 0)...)
			in.Candidates[0].P, in.Candidates[1].P = math.NaN(), 1.5
		}, "predictor_failure"},
		{"no candidates is not a predictor failure", func(in *Input) { *in = withCandidates(*in) }, ""},

		{"1 of 10 candidates truncated is exactly 10%", func(in *Input) {
			*in = withCandidates(*in, scoredCandidates(9, 0)...)
			in.RawCandidates, in.Truncated = 10, 1
		}, ""},
		{"2 of 10 candidates truncated", func(in *Input) {
			*in = withCandidates(*in, scoredCandidates(8, 0)...)
			in.RawCandidates, in.Truncated = 10, 2
		}, "truncated"},
		{"a Truncated count that understates the cut", func(in *Input) {
			*in = withCandidates(*in, scoredCandidates(8, 0)...)
			in.RawCandidates, in.Truncated = 10, 0
		}, "truncated"},
		{"truncation reported with no raw candidates", func(in *Input) { in.Truncated = 1 }, "truncated"},

		{"4 of 10 packages changed is exactly 40%", func(in *Input) { in.PackageCount, in.ChangedPackages = 10, 4 }, ""},
		{"5 of 10 packages changed", func(in *Input) { in.PackageCount, in.ChangedPackages = 10, 5 }, "too_broad"},
		{"package count unknown", func(in *Input) { in.PackageCount, in.ChangedPackages = 0, 3 }, ""},

		{"drift alarm", func(in *Input) { in.Drift = DriftStatus{State: "alarm", Alarm: true} }, "drift"},
		{"drift warming is not an alarm", func(in *Input) { in.Drift = DriftStatus{State: "warming"} }, ""},

		{"saving exactly the minimum", func(in *Input) { in.MinSavingRatio, in.Certain = 0.25, certainIDs(15) }, ""},
		{"saving below the minimum", func(in *Input) { in.MinSavingRatio, in.Certain = 0.25, certainIDs(16) }, "not_worth_it"},
		{"no saving when the full suite costs nothing", func(in *Input) { zeroCost(in) }, "not_worth_it"},
		{"no saving needed when the full suite costs nothing", func(in *Input) { zeroCost(in); in.MinSavingRatio = 0 }, ""},
		{"empty inventory saves nothing", func(in *Input) { in.AllTests, in.TestMS = nil, nil }, "not_worth_it"},
		{"an unusable minimum saving ratio", func(in *Input) { in.MinSavingRatio = math.NaN() }, "not_worth_it"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInput()
			tc.set(&in)
			d := Decide(in)
			if d.FullSuite != (tc.want != "") || d.Reason != tc.want {
				t.Errorf("FullSuite=%v Reason=%q, want reason %q", d.FullSuite, d.Reason, tc.want)
			}
		})
	}
}

// Every rule 3 condition is made true, then cleared one at a time in the order
// of the brief: the first condition still true names the abstention.
func TestDecideAbstentionOrder(t *testing.T) {
	in := withCandidates(baseInput(), scoredCandidates(4, 2)...)
	in.Certain = []string{"t00"}
	in.CoverageStale, in.CoverageRequired = true, true
	in.RawCandidates, in.Truncated = 10, 6
	in.ChangedPackages = 3
	in.Drift = DriftStatus{State: "alarm", Alarm: true}
	in.MinSavingRatio = 0.99

	steps := []struct {
		want  string
		clear func(*Input)
	}{
		{"coverage_stale", func(in *Input) { in.CoverageStale = false }},
		{"predictor_failure", func(in *Input) {
			for i := range in.Candidates {
				in.Candidates[i].Failed = false
			}
		}},
		{"truncated", func(in *Input) { in.RawCandidates, in.Truncated = len(in.Candidates), 0 }},
		{"too_broad", func(in *Input) { in.ChangedPackages = 1 }},
		{"drift", func(in *Input) { in.Drift = DriftStatus{State: "ok"} }},
		{"not_worth_it", func(in *Input) { in.MinSavingRatio = 0.2 }},
		{"", func(*Input) {}},
	}
	for _, step := range steps {
		d := Decide(in)
		if d.Reason != step.want || d.FullSuite != (step.want != "") {
			t.Fatalf("FullSuite=%v Reason=%q, want reason %q", d.FullSuite, d.Reason, step.want)
		}
		step.clear(&in)
	}
}

func TestDecideTierSuiteComesBeforeTheOtherAbstentions(t *testing.T) {
	in := baseInput()
	in.Tier = "suite"
	in.Drift = DriftStatus{State: "alarm", Alarm: true}
	in.CoverageStale, in.CoverageRequired = true, true
	if d := Decide(in); d.Reason != "tier_suite" {
		t.Errorf("Reason = %q, want tier_suite", d.Reason)
	}
}

func TestDecideAbstentionKeepsTheNarrowedSelectionAndItsCost(t *testing.T) {
	in := baseInput()
	in.Certain = []string{"t02", "t01"}
	in.Drift = DriftStatus{State: "alarm", Alarm: true}
	d := Decide(in)
	if !d.FullSuite || d.Reason != "drift" {
		t.Fatalf("FullSuite=%v Reason=%q, want full with drift", d.FullSuite, d.Reason)
	}
	requireMembers(t, d, "t01:certain", "t02:certain")
	if d.EstMSSelected != 20 || d.EstMSFull != 200 {
		t.Errorf("EstMSSelected=%d EstMSFull=%d, want 20 and 200", d.EstMSSelected, d.EstMSFull)
	}
}

func TestDecideCost(t *testing.T) {
	tests := []struct {
		name         string
		all          []string
		ms           map[string]int
		certain      []string
		wantSelected int
		wantFull     int
		wantWarnings []string
	}{
		{
			name: "known durations", all: []string{"a", "b", "c"}, ms: map[string]int{"a": 10, "b": 30, "c": 5},
			certain: []string{"a", "c"}, wantSelected: 15, wantFull: 45,
		},
		{
			name: "a zero duration is known", all: []string{"a", "b"}, ms: map[string]int{"a": 0, "b": 8},
			certain: []string{"a"}, wantSelected: 0, wantFull: 8,
		},
		{
			name: "unknown uses the median of the known values", all: []string{"a", "b", "c", "d", "e"},
			ms: map[string]int{"a": 10, "b": 20, "c": 90}, certain: []string{"a", "d"},
			wantSelected: 30, wantFull: 10 + 20 + 90 + 20 + 20, wantWarnings: []string{"durations_unknown:2"},
		},
		{
			name: "an even number of known values rounds the median up", all: []string{"a", "b", "c"},
			ms: map[string]int{"a": 10, "b": 31}, certain: []string{"a", "c"},
			wantSelected: 31, wantFull: 10 + 31 + 21, wantWarnings: []string{"durations_unknown:1"},
		},
		{
			name: "no known duration counts tests", all: []string{"a", "b", "c", "d"}, ms: nil,
			certain: []string{"a", "b"}, wantSelected: 2, wantFull: 4, wantWarnings: []string{"durations_unknown:4"},
		},
		{
			name: "a negative duration is unknown", all: []string{"a", "b"}, ms: map[string]int{"a": -5, "b": 40},
			certain: []string{"a"}, wantSelected: 40, wantFull: 80, wantWarnings: []string{"durations_unknown:1"},
		},
		{
			name: "a selected test outside the inventory is costed and counted once", all: []string{"a", "b"},
			ms: map[string]int{"a": 10, "b": 30}, certain: []string{"ghost", "a"},
			wantSelected: 30, wantFull: 40, wantWarnings: []string{"durations_unknown:1"},
		},
		{
			name: "a repeated inventory entry is costed once", all: []string{"a", "b", "a"},
			ms: map[string]int{"a": 10, "b": 30}, certain: []string{"b"}, wantSelected: 30, wantFull: 40,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := Decide(Input{Lambda: 0.3, AllTests: tc.all, TestMS: tc.ms, Certain: tc.certain})
			if d.EstMSSelected != tc.wantSelected || d.EstMSFull != tc.wantFull {
				t.Errorf("EstMSSelected=%d EstMSFull=%d, want %d and %d", d.EstMSSelected, d.EstMSFull, tc.wantSelected, tc.wantFull)
			}
			if !slices.Equal(d.Warnings, tc.wantWarnings) {
				t.Errorf("warnings = %q, want %q", d.Warnings, tc.wantWarnings)
			}
		})
	}
}

// Property tests (seed 13.3). Case i draws its input from a generator seeded
// with (propSeed, i), so the case number in a failure message replays it.
const propSeed uint64 = 0x48415a415244

const propCases = 2000

func propRand(i int) *rand.Rand { return rand.New(rand.NewPCG(propSeed, uint64(i))) }

func pick[T any](r *rand.Rand, xs ...T) T { return xs[r.IntN(len(xs))] }

// genInput draws a random input that reaches every branch of Decide: all
// reasons of rule 1 and rule 3, the three tiers, unknown durations, failed
// and truncated candidates, and probabilities that sit exactly on Lambda.
func genInput(r *rand.Rand) Input {
	nTests, nPkgs := 4+r.IntN(37), 3+r.IntN(8)
	in := Input{
		Lambda:         pick(r, 0.1, 0.25, 0.5, 0.75, 0.02+0.96*r.Float64()),
		TestPackage:    map[string]string{},
		TestMS:         map[string]int{},
		MinSavingRatio: pick(r, 0, 0.1, 0.3, 0.6),
		Tier:           pick(r, "", "", "targeted", "targeted", "module", "module", "suite"),
	}
	knowsDurations := r.Float64() > 0.05
	pkgs := map[string]bool{}
	for i := 0; i < nTests; i++ {
		id := fmt.Sprintf("t%02d", i)
		in.AllTests = append(in.AllTests, id)
		in.TestPackage[id] = fmt.Sprintf("pkg%d", r.IntN(nPkgs))
		pkgs[in.TestPackage[id]] = true
		if knowsDurations && r.Float64() < 0.8 {
			in.TestMS[id] = 1 + r.IntN(50)
		}
	}
	in.PackageCount = len(pkgs)
	in.ChangedPackages = pick(r, 1, 1, r.IntN(len(pkgs)+1))

	failRate := pick(r, 0.0, 0.0, 0.05, 0.4)
	for _, id := range in.AllTests {
		if r.Float64() < 0.1 {
			in.Certain = append(in.Certain, id)
		}
		if r.Float64() < 0.06 {
			in.AlwaysRun = append(in.AlwaysRun, id)
		}
		if r.Float64() < 0.45 {
			c := Candidate{TestID: id, P: pick(r, 0, in.Lambda, 1, r.Float64())}
			if r.Float64() < failRate {
				c.Failed, c.P = true, pick(r, 0.0, 1.0)
			}
			in.Candidates = append(in.Candidates, c)
		}
	}
	in.RawCandidates = len(in.Candidates)
	if r.Float64() < 0.15 {
		in.RawCandidates += 1 + r.IntN(4)
	}
	in.Truncated = in.RawCandidates - len(in.Candidates)

	in.CoverageStale, in.CoverageRequired = r.Float64() < 0.2, r.Float64() < 0.3
	in.Drift = DriftStatus{Alarm: r.Float64() < 0.05}
	if r.Float64() < 0.04 {
		in.LockReason = pick(r, "no_lock", "lock_corrupt", "lock_error", "model_drift", "rubric_mismatch", "feature_mismatch", "policy_mismatch")
	}
	if r.Float64() < 0.03 {
		in.FullReason = pick(r, "graph_error", "no_merge_base")
	}
	if r.Float64() < 0.03 {
		in.TriggersFull = []string{"go.mod:go.mod"}
	}
	shuffle(r, in.AllTests)
	shuffle(r, in.Candidates)
	return in
}

func shuffle[T any](r *rand.Rand, xs []T) {
	r.Shuffle(len(xs), func(i, j int) { xs[i], xs[j] = xs[j], xs[i] })
}

// cloneInput copies every slice and map of in, so a mutation of the original
// shows up as a difference from the clone.
func cloneInput(in Input) Input {
	in.TriggersFull = slices.Clone(in.TriggersFull)
	in.Certain = slices.Clone(in.Certain)
	in.AlwaysRun = slices.Clone(in.AlwaysRun)
	in.Candidates = slices.Clone(in.Candidates)
	in.AllTests = slices.Clone(in.AllTests)
	in.TestPackage = maps.Clone(in.TestPackage)
	in.TestMS = maps.Clone(in.TestMS)
	return in
}

func memberIDs(d Decision) map[string]bool {
	ids := make(map[string]bool, len(d.Members))
	for _, m := range d.Members {
		ids[m.TestID] = true
	}
	return ids
}

// requireSuperset fails the test when a member of small is missing from big.
func requireSuperset(t *testing.T, i int, what string, big, small Decision) {
	t.Helper()
	have := memberIDs(big)
	for id := range memberIDs(small) {
		if !have[id] {
			t.Fatalf("case %d (seed %#x): %s dropped member %q", i, propSeed, what, id)
		}
	}
}

func TestPropertyGeneratorReachesEveryDecisionPath(t *testing.T) {
	reasons := map[string]int{}
	narrowed, withAdded := 0, 0
	for i := 0; i < propCases; i++ {
		d := Decide(genInput(propRand(i)))
		if d.FullSuite {
			reasons[d.Reason]++
			continue
		}
		narrowed++
		for _, m := range d.Members {
			if m.Source == "added" {
				withAdded++
				break
			}
		}
	}
	t.Logf("%d cases: %d narrowed (%d with an added member), full-suite reasons %v", propCases, narrowed, withAdded, reasons)
	for _, reason := range []string{
		"trigger", "graph_error", "model_drift", "tier_suite", "coverage_stale",
		"predictor_failure", "truncated", "too_broad", "drift", "not_worth_it",
	} {
		if reasons[reason] == 0 {
			t.Errorf("no generated case reached the %q decision (seed %#x, %d cases): %v", reason, propSeed, propCases, reasons)
		}
	}
	if narrowed < propCases/10 || withAdded < propCases/10 {
		t.Errorf("only %d narrowed decisions and %d with an added member in %d cases", narrowed, withAdded, propCases)
	}
}

// P01: adding a candidate to the residual set never removes a member.
func TestPropertyP01AddingACandidateNeverRemovesAMember(t *testing.T) {
	for i := 0; i < propCases; i++ {
		r := propRand(i)
		in := genInput(r)
		before := Decide(cloneInput(in))

		have := map[string]bool{}
		for _, c := range in.Candidates {
			have[c.TestID] = true
		}
		extra := "zz-extra"
		for _, id := range in.AllTests {
			if !have[id] {
				extra = id
				break
			}
		}
		c := Candidate{TestID: extra, P: pick(r, 0, in.Lambda, 1, r.Float64())}
		c.Failed = r.Float64() < 0.1
		in.Candidates = append(in.Candidates, c)
		in.RawCandidates++

		requireSuperset(t, i, fmt.Sprintf("adding candidate %+v", c), Decide(in), before)
	}
}

// P02: lowering Lambda never shrinks the selection.
func TestPropertyP02LoweringLambdaNeverShrinksTheSelection(t *testing.T) {
	for i := 0; i < propCases; i++ {
		r := propRand(i)
		in := genInput(r)
		high := Decide(cloneInput(in))

		low := cloneInput(in)
		low.Lambda = in.Lambda * r.Float64()
		requireSuperset(t, i, fmt.Sprintf("lowering lambda %v to %v", in.Lambda, low.Lambda), Decide(low), high)
	}
}

// P03: Decide is deterministic and leaves its input alone.
func TestPropertyP03DecideIsDeterministic(t *testing.T) {
	for i := 0; i < propCases; i++ {
		in := genInput(propRand(i))
		pristine := cloneInput(in)
		first, second := Decide(in), Decide(in)
		if !reflect.DeepEqual(first, second) {
			t.Fatalf("case %d (seed %#x): two calls differ:\n%+v\n%+v", i, propSeed, first, second)
		}
		if !reflect.DeepEqual(in, pristine) {
			t.Fatalf("case %d (seed %#x): Decide modified its input", i, propSeed)
		}
	}
}

// P04: the decision does not depend on the order of Certain, AlwaysRun and
// Candidates, nor on how Candidates is cut into shards and concatenated.
func TestPropertyP04SelectionIsInvariantToOrderAndShards(t *testing.T) {
	for i := 0; i < propCases; i++ {
		r := propRand(i)
		in := genInput(r)
		want := Decide(cloneInput(in))

		shuffled := cloneInput(in)
		shuffle(r, shuffled.Certain)
		shuffle(r, shuffled.AlwaysRun)
		shuffled.Candidates = reshard(r, in.Candidates)

		if got := Decide(shuffled); !reflect.DeepEqual(got, want) {
			t.Fatalf("case %d (seed %#x): reordered input changed the decision:\nwant %+v\ngot  %+v", i, propSeed, want, got)
		}
	}
}

// reshard cuts cands into one to five shards at random points, shuffles each
// shard and the order of the shards, and concatenates them.
func reshard(r *rand.Rand, cands []Candidate) []Candidate {
	var shards [][]Candidate
	rest := slices.Clone(cands)
	for n := 1 + r.IntN(5); n > 1 && len(rest) > 0; n-- {
		cut := r.IntN(len(rest) + 1)
		shards = append(shards, rest[:cut])
		rest = rest[cut:]
	}
	shards = append(shards, rest)
	r.Shuffle(len(shards), func(a, b int) { shards[a], shards[b] = shards[b], shards[a] })
	var out []Candidate
	for _, shard := range shards {
		shuffle(r, shard)
		out = append(out, shard...)
	}
	return out
}
