package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/omorsi45/hazard/internal/engine"
	"github.com/omorsi45/hazard/internal/impact"
)

// featureNames name the values of impact.Features.Vector, in its order.
var featureNames = []string{
	"hops", "reachable", "coverage_overlap", "coverage_known", "coverage_stale",
	"cofailure_rate", "cofailure_support", "test_fail_rate", "days_since_last_fail", "shared_symbol_hits",
	"same_file", "same_package",
	"kind_source", "kind_test", "kind_config", "kind_fixture", "kind_generated", "kind_docs", "kind_lock", "kind_build",
	"lines_added", "lines_removed", "files_changed", "test_median_ms", "test_is_integration",
}

const (
	noPredictor    = "n/a (the predictor was not asked)"
	certainNoScore = "n/a (a CERTAIN test is not scored)"
	noShard        = "n/a"
	noRequest      = "n/a (no request answered)"
	noLambda       = "n/a (no verified lock)"
	notApplicable  = "n/a"
	unknownValue   = "unknown"
)

// explanation is what `hazard explain` reports about one test.
type explanation struct {
	Test                 string             `json:"test"`
	InInventory          string             `json:"in_inventory"` // yes, no or unknown
	Hops                 *int               `json:"hops"`         // null when unreachable or unknown
	Reachable            bool               `json:"reachable"`
	L1                   string             `json:"l1"`
	Rank                 *float64           `json:"rank,omitempty"`
	Features             map[string]float64 `json:"features,omitempty"`
	CoverageOverlap      *float64           `json:"coverage_overlap"` // null when unknown or when the test is not a ranked candidate
	CofailureRate        *float64           `json:"cofailure_rate"`   // null when unknown or when the test is not a ranked candidate
	CoverageState        string             `json:"coverage_state"`
	PredictorProbability string             `json:"predictor_probability"` // "P (source)", or why there is none
	Shard                string             `json:"shard"`                 // the request that answered, or why there was none
	Lambda               string             `json:"lambda"`                // the threshold of the verified lock
	Decision             string             `json:"decision"`

	hopsText      string
	overlapText   string
	cofailureText string
	featureOrder  []string
	certain       bool
}

func newExplainCommand(sel selectFunc) *cobra.Command {
	cmd := &cobra.Command{
		Use: "explain", Short: "Explain one test decision",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 0 {
				return usageError{errors.New("explain accepts no positional arguments")}
			}
			return nil
		},
	}
	cmd.Flags().String("test", "", "stable test ID")
	cmd.Flags().String("base", "", "base revision")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		testID, _ := cmd.Flags().GetString("test")
		base, _ := cmd.Flags().GetString("base")
		if testID == "" {
			return usageError{errors.New("--test is required")}
		}
		if err := cmd.Context().Err(); err != nil {
			return err
		}
		root, err := repoRoot(cmd.Context())
		if err != nil {
			return err
		}
		d, err := sel(cmd.Context(), engine.Options{RepoDir: root, Base: base, Explain: true})
		if err != nil {
			return mapEngineError(err)
		}
		exp, err := explain(d, testID)
		if err != nil {
			return err
		}
		if jsonMode, _ := cmd.Flags().GetBool("json"); jsonMode {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(exp)
		}
		_, err = fmt.Fprint(cmd.OutOrStdout(), exp.text())
		return err
	}
	return cmd
}

// explain builds the explanation of testID from a decision. A test missing
// from a trusted inventory is a usage error; with no usable inventory the
// membership is reported as unknown.
func explain(d engine.Decision, testID string) (explanation, error) {
	e := explanation{
		Test: testID, InInventory: "unknown", hopsText: "unknown", L1: "not computed (" + reasonOf(d, false) + ")",
		CoverageState: "n/a", PredictorProbability: noPredictor, Shard: noShard, Lambda: noLambda,
		overlapText: notApplicable, cofailureText: notApplicable,
	}
	if d.LockDigest != "" {
		e.Lambda = formatNumber(d.Lambda)
	}
	if r := d.L1; r != nil {
		i := sort.SearchStrings(r.AllTests, testID)
		if i >= len(r.AllTests) || r.AllTests[i] != testID {
			return e, usageError{fmt.Errorf("unknown test %q: it is not in the index inventory", testID)}
		}
		e.InInventory = "yes"
		e.CoverageState = r.CoverageState
		explainL1(&e, r, testID)
	}
	score, scored := scoreOf(d.Scores, testID)
	switch {
	case scored:
		e.PredictorProbability = formatNumber(score.P) + " (" + score.Source + ")"
		e.Shard = noRequest
		if score.Shard >= 0 {
			e.Shard = strconv.Itoa(score.Shard)
		}
	case e.certain:
		e.PredictorProbability = certainNoScore
	}
	e.Decision = decisionLine(d, testID, score, scored)
	return e, nil
}

// decisionLine is the final decision for one test: the full suite and why, the
// source and reason of a selected test, or why a scored test was left out.
func decisionLine(d engine.Decision, testID string, score engine.TestScore, scored bool) string {
	if d.FullSuite {
		return "full suite: " + reasonOf(d, false)
	}
	for _, s := range d.Tests {
		if s.TestID != testID {
			continue
		}
		var why []string
		for _, part := range []string{s.Source, s.Reason} {
			if part != "" {
				why = append(why, part)
			}
		}
		if len(why) == 0 {
			return "selected"
		}
		return "selected (" + strings.Join(why, ", ") + ")"
	}
	if scored {
		return fmt.Sprintf("not selected (P %s below lambda %s)", formatNumber(score.P), formatNumber(d.Lambda))
	}
	return "not selected"
}

// scoreOf finds what the predictor said about a test.
func scoreOf(scores []engine.TestScore, testID string) (engine.TestScore, bool) {
	for _, s := range scores {
		if s.TestID == testID {
			return s, true
		}
	}
	return engine.TestScore{}, false
}

// formatNumber renders a probability or a threshold with at most six
// significant digits.
func formatNumber(v float64) string { return strconv.FormatFloat(v, 'g', 6, 64) }

// explainL1 fills the L1 part of e for a test in the inventory.
func explainL1(e *explanation, r *impact.Result, testID string) {
	if h, ok := r.TestHops[testID]; ok {
		e.Hops, e.Reachable, e.hopsText = &h, true, fmt.Sprint(h)
	} else {
		e.hopsText = "unreachable"
	}
	if r.FullReason != "" {
		e.L1 = "full suite demanded (" + r.FullReason + ")"
		return
	}
	for _, m := range r.Certain {
		if m.TestID == testID {
			e.L1, e.certain = "certain ("+m.Reason+")", true
			return
		}
	}
	for _, c := range r.Candidates {
		if c.TestID != testID {
			continue
		}
		rank := c.Rank
		e.L1, e.Rank = fmt.Sprintf("candidate (rank %.4f)", c.Rank), &rank
		e.Features = map[string]float64{}
		for i, v := range c.Features.Vector() {
			e.Features[featureNames[i]] = v
			e.featureOrder = append(e.featureOrder, featureNames[i])
		}
		explainHistory(e, c.Features)
		return
	}
	e.L1 = "residual, not ranked"
	if r.Truncated > 0 {
		e.L1 += fmt.Sprintf(" (candidate cap cut %d of %d)", r.Truncated, r.RawCandidates)
	}
}

// explainHistory reports the coverage overlap and the co-failure rate of a
// candidate, or that they are unknown. Unknown is not zero.
func explainHistory(e *explanation, f impact.Features) {
	e.overlapText, e.cofailureText = unknownValue, unknownValue
	if f.CoverageKnown {
		v := f.CoverageOverlap
		e.CoverageOverlap, e.overlapText = &v, formatNumber(v)
	}
	if f.CofailureSupport > 0 {
		v := f.CofailureRate
		e.CofailureRate, e.cofailureText = &v, formatNumber(v)
	}
}

// text is the human-readable report.
func (e explanation) text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "test: %s\n", e.Test)
	fmt.Fprintf(&b, "in inventory: %s\n", e.InInventory)
	fmt.Fprintf(&b, "hops: %s\n", e.hopsText)
	fmt.Fprintf(&b, "l1: %s\n", e.L1)
	if len(e.featureOrder) > 0 {
		parts := make([]string, len(e.featureOrder))
		for i, name := range e.featureOrder {
			parts[i] = fmt.Sprintf("%s=%g", name, e.Features[name])
		}
		fmt.Fprintf(&b, "features: %s\n", strings.Join(parts, " "))
	}
	fmt.Fprintf(&b, "coverage overlap: %s\n", e.overlapText)
	fmt.Fprintf(&b, "cofailure rate: %s\n", e.cofailureText)
	fmt.Fprintf(&b, "coverage state: %s\n", e.CoverageState)
	fmt.Fprintf(&b, "predictor probability: %s\n", e.PredictorProbability)
	fmt.Fprintf(&b, "shard: %s\n", e.Shard)
	fmt.Fprintf(&b, "lambda: %s\n", e.Lambda)
	fmt.Fprintf(&b, "decision: %s\n", e.Decision)
	return b.String()
}
