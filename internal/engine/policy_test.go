package engine

import (
	"reflect"
	"testing"

	"github.com/omorsi45/hazard/internal/config"
	"github.com/omorsi45/hazard/internal/control"
	"github.com/omorsi45/hazard/internal/impact"
	"github.com/omorsi45/hazard/internal/index"
)

func TestBuildPolicyIsTheConfiguredSelectionPolicy(t *testing.T) {
	t.Parallel()
	cfg := config.Defaults()

	got := BuildPolicy(cfg, "logistic")

	want := control.Policy{
		FeatureVersion:    impact.FeatureVersion,
		Predictor:         "logistic",
		MaxHops:           cfg.Impact.MaxHops,
		MaxCandidates:     cfg.Impact.MaxCandidates,
		CoverageRequired:  cfg.Impact.CoverageRequired,
		MinSavingRatio:    cfg.Control.MinSavingRatio,
		CandidateRanking:  cfg.Impact.CandidateRanking,
		FullSuiteTriggers: cfg.Repo.FullSuiteTriggers,
		GeneratedGlobs:    cfg.Repo.GeneratedGlobs,
		Redact:            cfg.Privacy.Redact,
		TierEnabled:       false,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("policy\n got: %+v\nwant: %+v", got, want)
	}
}

// TestBuildPolicyDigestFollowsEverythingThatChangesADecision: a lock is bound
// to the digest, so every knob that changes which tests are selected must move
// it, and a knob that cannot change a selection must not.
func TestBuildPolicyDigestFollowsEverythingThatChangesADecision(t *testing.T) {
	t.Parallel()
	base := BuildPolicy(config.Defaults(), "logistic").Digest()
	if base == "" {
		t.Fatal("the default policy has no digest")
	}
	moves := map[string]func(*config.Config){
		"max hops":          func(c *config.Config) { c.Impact.MaxHops++ },
		"max candidates":    func(c *config.Config) { c.Impact.MaxCandidates++ },
		"coverage required": func(c *config.Config) { c.Impact.CoverageRequired = !c.Impact.CoverageRequired },
		"min saving ratio":  func(c *config.Config) { c.Control.MinSavingRatio += 0.05 },
		"candidate ranking": func(c *config.Config) { c.Impact.CandidateRanking = []string{"hops"} },
		"full suite triggers": func(c *config.Config) {
			c.Repo.FullSuiteTriggers = append([]string{"extra.cfg"}, c.Repo.FullSuiteTriggers...)
		},
		"generated globs": func(c *config.Config) { c.Repo.GeneratedGlobs = []string{"**/*.gen.go"} },
		"redaction":       func(c *config.Config) { c.Privacy.Redact = "none" },
	}
	for name, move := range moves {
		cfg := config.Defaults()
		move(&cfg)
		if BuildPolicy(cfg, "logistic").Digest() == base {
			t.Errorf("changing %s did not change the policy digest", name)
		}
	}
	if BuildPolicy(config.Defaults(), "static").Digest() == base {
		t.Error("another predictor kind did not change the policy digest")
	}

	same := map[string]func(*config.Config){
		"jev budget":     func(c *config.Config) { c.Predictor.Jev.BudgetUSDPerRun *= 2 },
		"telemetry path": func(c *config.Config) { c.Telemetry.Path = "elsewhere.jsonl" },
		"drift window":   func(c *config.Config) { c.Control.Drift.WindowRuns++ },
	}
	for name, move := range same {
		cfg := config.Defaults()
		move(&cfg)
		if BuildPolicy(cfg, "logistic").Digest() != base {
			t.Errorf("changing %s moved the policy digest, which would void every lock for nothing", name)
		}
	}
}

func testManifest() *index.Manifest {
	return &index.Manifest{Tests: []index.Test{
		{ID: "go:m/a:TestA", Package: "m/a"},
		{ID: "go:m/b:TestB", Package: "m/b"},
		{ID: "py:t.py::test_c", Package: "t"},
	}}
}

func testL1() *impact.Result {
	return &impact.Result{
		Certain: []impact.Member{
			{TestID: "go:m/a:TestA", Reason: "direct"},
			{TestID: "go:m/b:TestB", Reason: "always_run"},
		},
		RawCandidates:   9,
		Truncated:       4,
		AllTests:        []string{"go:m/a:TestA", "go:m/b:TestB", "py:t.py::test_c"},
		ChangedPackages: []string{"m/a", "m/x"},
		PackageCount:    7,
		CoverageState:   "fresh",
	}
}

func TestControllerInputMapsTheDeterministicLayer(t *testing.T) {
	t.Parallel()
	cfg := config.Defaults()
	cfg.Impact.CoverageRequired = true
	cfg.Control.MinSavingRatio = 0.3
	cands := []control.Candidate{{TestID: "py:t.py::test_c", P: 0.4}, {TestID: "x", P: 1, Failed: true}}
	drift := control.DriftStatus{State: "ok", N: 5}
	ms := map[string]int{"go:m/a:TestA": 12}

	got := ControllerInput(cfg, testManifest(), testL1(), cands, 0.07, ms, drift)

	want := control.Input{
		Lambda:           0.07,
		Certain:          []string{"go:m/a:TestA"},
		AlwaysRun:        []string{"go:m/b:TestB"},
		Candidates:       cands,
		RawCandidates:    9,
		Truncated:        4,
		CoverageStale:    false,
		CoverageRequired: true,
		ChangedPackages:  2,
		PackageCount:     7,
		AllTests:         []string{"go:m/a:TestA", "go:m/b:TestB", "py:t.py::test_c"},
		TestPackage:      map[string]string{"go:m/a:TestA": "m/a", "go:m/b:TestB": "m/b", "py:t.py::test_c": "t"},
		TestMS:           ms,
		MinSavingRatio:   0.3,
		Tier:             "",
		Drift:            drift,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("input\n got: %+v\nwant: %+v", got, want)
	}
}

func TestControllerInputLeavesTheFullSuiteDemandsToTheCaller(t *testing.T) {
	t.Parallel()
	l1 := testL1()
	l1.FullReason = "trigger:go.mod:go.mod" // a result that demands the full suite

	got := ControllerInput(config.Defaults(), testManifest(), l1, nil, 0.05, nil, control.DriftStatus{})

	if got.LockReason != "" || got.FullReason != "" || len(got.TriggersFull) != 0 {
		t.Errorf("LockReason %q FullReason %q TriggersFull %v, want all left for the caller", got.LockReason, got.FullReason, got.TriggersFull)
	}
}

func TestControllerInputCoverageIsStaleUnlessFreshOrNotRequested(t *testing.T) {
	t.Parallel()
	for state, stale := range map[string]bool{
		"fresh":                false,
		"not_requested":        false,
		"missing":              true,
		"stale:too_old":        true,
		"stale:not_ancestor":   true,
		"stale:added_file":     true,
		"stale:inventory_gone": true,
		"":                     true, // an unknown state is not evidence of fresh coverage
		"something new":        true,
	} {
		l1 := testL1()
		l1.CoverageState = state

		got := ControllerInput(config.Defaults(), testManifest(), l1, nil, 0.05, nil, control.DriftStatus{})

		if got.CoverageStale != stale {
			t.Errorf("coverage state %q: CoverageStale = %v, want %v", state, got.CoverageStale, stale)
		}
	}
}

func TestControllerInputSplitsAlwaysRunFromCertain(t *testing.T) {
	t.Parallel()
	l1 := &impact.Result{Certain: []impact.Member{
		{TestID: "z", Reason: "always_run"}, {TestID: "a", Reason: "graph"}, {TestID: "m", Reason: "fixture"},
		{TestID: "b", Reason: "always_run"}, {TestID: "c", Reason: "coverage"},
	}}

	got := ControllerInput(config.Defaults(), &index.Manifest{}, l1, nil, 0.05, nil, control.DriftStatus{})

	if want := []string{"a", "m", "c"}; !reflect.DeepEqual(got.Certain, want) {
		t.Errorf("Certain = %v, want %v", got.Certain, want)
	}
	if want := []string{"z", "b"}; !reflect.DeepEqual(got.AlwaysRun, want) {
		t.Errorf("AlwaysRun = %v, want %v", got.AlwaysRun, want)
	}
}
