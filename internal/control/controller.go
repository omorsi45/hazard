package control

import (
	"cmp"
	"slices"
	"strconv"
)

// Sources of a Member.
const (
	sourceCertain   = "certain"
	sourceAlwaysRun = "always_run"
	sourceAdded     = "added"
	sourceTier      = "tier"
)

// Reasons Decide reports besides the ones the caller passes in.
const (
	reasonTrigger          = "trigger"
	reasonTierSuite        = "tier_suite"
	reasonCoverageStale    = "coverage_stale"
	reasonPredictorFailure = "predictor_failure"
	reasonTruncated        = "truncated"
	reasonTooBroad         = "too_broad"
	reasonDrift            = "drift"
	reasonNotWorthIt       = "not_worth_it"
)

// warningDurationsUnknown prefixes the warning that counts the tests whose
// duration was imputed.
const warningDurationsUnknown = "durations_unknown:"

// Tiers an advisory answer may name. Any other value, like "targeted" and "",
// changes nothing: a tier can only widen the selection.
const (
	tierModule = "module"
	tierSuite  = "suite"
)

// Candidate is one residual candidate the predictor scored (or failed to).
type Candidate struct {
	TestID string
	P      float64 // already validated in [0,1]; a failed shard or a skipped budget gives 1
	Failed bool    // the predictor could not score it (shard failure, budget, malformed answer)
}

// Input is everything Decide needs. It carries no behavior: the caller
// gathers it from the lock, the deterministic layer and the predictor.
type Input struct {
	LockReason       string      // "" when a verified lock is in force; otherwise no_lock|lock_corrupt|lock_error|model_drift|rubric_mismatch|feature_mismatch|policy_mismatch
	Lambda           float64     // from the verified lock
	FullReason       string      // non-empty when L1 or an adapter already demands the full suite
	TriggersFull     []string    // full-suite triggers the change matched
	Certain          []string    // CERTAIN test IDs (direct, coverage, graph, fixture), any order
	AlwaysRun        []string    // quarantined/unstable test IDs, any order
	Candidates       []Candidate // every residual candidate, any order, IDs unique
	RawCandidates    int         // L1 residual count before the cap
	Truncated        int         // RawCandidates - len(Candidates)
	CoverageStale    bool
	CoverageRequired bool
	ChangedPackages  int               // packages touched by the change
	PackageCount     int               // packages owning at least one test
	AllTests         []string          // full inventory
	TestPackage      map[string]string // test ID -> package, for module widening
	TestMS           map[string]int    // median runtime per test; missing => unknown
	MinSavingRatio   float64
	Tier             string // "", "targeted", "module", "suite"
	Drift            DriftStatus
}

// Member is one test of the narrowed selection. Source is certain,
// always_run, added or tier; P is the predicted probability of an added
// member and nil for every other source.
type Member struct {
	TestID string
	Source string
	P      *float64
}

// Decision is the controller's answer.
type Decision struct {
	FullSuite     bool
	Reason        string   // abstention reason when FullSuite
	Members       []Member // sorted by TestID, unique; the narrowed selection (see Decide)
	EstMSSelected int
	EstMSFull     int
	Warnings      []string
}

// Decide turns the deterministic set and the predicted probabilities into a
// final decision. Rule 1 demands the full suite before any selection (a
// trigger, a FullReason or a LockReason, in that order) and leaves Members
// empty. Otherwise the selection is the union of Certain, AlwaysRun and every
// candidate that is failed or has P >= Lambda; a union never drops a member.
// The module tier then adds every test of each package that has a selected
// test, and the suite tier abstains with the narrowed selection kept. Last,
// the first matching abstention of rule 3 turns the decision into the full
// suite; Members still lists what would have run.
func Decide(in Input) Decision {
	if reason := demandedFullSuite(in); reason != "" {
		return Decision{FullSuite: true, Reason: reason}
	}
	sel := selection(in)
	d := Decision{Members: sortedMembers(sel)}
	switch in.Tier {
	case tierModule:
		widenToModules(sel, in)
		d.Members = sortedMembers(sel)
	case tierSuite:
		d.FullSuite, d.Reason = true, reasonTierSuite
	}
	d.EstMSSelected, d.EstMSFull, d.Warnings = estimateCost(in, d.Members)
	if !d.FullSuite {
		if reason := abstentionReason(in, d); reason != "" {
			d.FullSuite, d.Reason = true, reason
		}
	}
	return d
}

// demandedFullSuite returns the rule 1 reason, or "" when none applies.
func demandedFullSuite(in Input) string {
	switch {
	case len(in.TriggersFull) > 0:
		return reasonTrigger
	case in.FullReason != "":
		return in.FullReason
	default:
		return in.LockReason
	}
}

// selection returns the union of the certain, always-run and added tests by
// test ID. A test in both Certain and AlwaysRun stays certain.
func selection(in Input) map[string]Member {
	sel := make(map[string]Member, len(in.Certain)+len(in.AlwaysRun)+len(in.Candidates))
	for _, id := range in.AlwaysRun {
		sel[id] = Member{TestID: id, Source: sourceAlwaysRun}
	}
	for _, id := range in.Certain {
		sel[id] = Member{TestID: id, Source: sourceCertain}
	}
	for id, c := range mergeCandidates(in.Candidates) {
		if _, ok := sel[id]; ok {
			continue
		}
		if c.failed || !(c.p < in.Lambda) { // "not below" so a NaN lambda selects everything
			p := c.p
			sel[id] = Member{TestID: id, Source: sourceAdded, P: &p}
		}
	}
	return sel
}

// widenToModules adds, as tier members, every test of every package that has
// a member in sel. A selected test with no known package widens nothing: the
// tier is advisory and may only add tests.
func widenToModules(sel map[string]Member, in Input) {
	pkgs := make(map[string]bool)
	for id := range sel {
		if pkg, ok := in.TestPackage[id]; ok {
			pkgs[pkg] = true
		}
	}
	for _, id := range in.AllTests {
		if _, ok := sel[id]; ok {
			continue
		}
		if pkg, ok := in.TestPackage[id]; ok && pkgs[pkg] {
			sel[id] = Member{TestID: id, Source: sourceTier}
		}
	}
}

// abstentionReason returns the first rule 3 reason that holds for the
// selection d, or "". The thresholds are strict: exactly 25% failed, exactly
// 10% truncated and exactly 40% of the packages changed do not abstain.
func abstentionReason(in Input, d Decision) string {
	failed := 0
	for _, c := range in.Candidates {
		if isFailed(c) {
			failed++
		}
	}
	// Trust the larger of the reported and the implied truncation: a caller
	// that understates the cut must not hide it.
	truncated := max(in.Truncated, in.RawCandidates-len(in.Candidates))

	switch {
	case in.CoverageStale && in.CoverageRequired:
		return reasonCoverageStale
	case failed*4 > len(in.Candidates):
		return reasonPredictorFailure
	case truncated*10 > in.RawCandidates:
		return reasonTruncated
	case in.PackageCount > 0 && in.ChangedPackages*10 > in.PackageCount*4:
		return reasonTooBroad
	case in.Drift.Alarm:
		return reasonDrift
	case !(saving(d) >= in.MinSavingRatio): // also true when the ratio is NaN
		return reasonNotWorthIt
	}
	return ""
}

// saving is the estimated share of the full suite's runtime that the
// selection d avoids. A full suite that costs nothing offers no saving.
func saving(d Decision) float64 {
	if d.EstMSFull <= 0 {
		return 0
	}
	return 1 - float64(d.EstMSSelected)/float64(d.EstMSFull)
}

// estimateCost returns the estimated runtime of members and of AllTests, and
// the durations_unknown warning when some test had no known duration. A test
// without a duration costs the median of the known ones (rounded up), or 1
// when none is known, in which case both sums are test counts. A negative
// duration is not a measurement and counts as unknown.
func estimateCost(in Input, members []Member) (selected, full int, warnings []string) {
	known := make([]int, 0, len(in.TestMS))
	for _, ms := range in.TestMS {
		if ms >= 0 {
			known = append(known, ms)
		}
	}
	imputed := 1
	if len(known) > 0 {
		slices.Sort(known)
		imputed = medianRoundedUp(known)
	}

	unknown := make(map[string]bool)
	cost := func(id string) int {
		if ms, ok := in.TestMS[id]; ok && ms >= 0 {
			return ms
		}
		unknown[id] = true
		return imputed
	}
	inventory := make(map[string]bool, len(in.AllTests))
	for _, id := range in.AllTests {
		if !inventory[id] {
			inventory[id] = true
			full += cost(id)
		}
	}
	for _, m := range members {
		selected += cost(m.TestID)
	}
	if len(unknown) > 0 {
		warnings = []string{warningDurationsUnknown + strconv.Itoa(len(unknown))}
	}
	return selected, full, warnings
}

// medianRoundedUp returns the median of a sorted, non-empty slice; for an even
// length it is the mean of the middle pair rounded up, which can only make a
// test look more expensive.
func medianRoundedUp(sorted []int) int {
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	lo, hi := sorted[mid-1], sorted[mid]
	return lo + (hi-lo+1)/2
}

// scored is what Decide keeps of a candidate: its probability, with 1 for a
// failed or invalid one, and whether it failed.
type scored struct {
	p      float64
	failed bool
}

// isFailed reports whether the predictor produced no usable score: it said so
// itself, or the probability is outside [0, 1] (which includes NaN). Unknown
// is never zero, so such a candidate is treated as P = 1.
func isFailed(c Candidate) bool {
	return c.Failed || !(c.P >= 0 && c.P <= 1)
}

// mergeCandidates keys the candidates by test ID. IDs are meant to be unique;
// if one repeats, the highest probability and any failure win, so the answer
// does not depend on the order of the copies.
func mergeCandidates(cands []Candidate) map[string]scored {
	merged := make(map[string]scored, len(cands))
	for _, c := range cands {
		s := scored{p: c.P, failed: isFailed(c)}
		if s.failed {
			s.p = 1
		}
		if prev, ok := merged[c.TestID]; ok {
			s.failed = s.failed || prev.failed
			s.p = max(s.p, prev.p)
		}
		merged[c.TestID] = s
	}
	return merged
}

// sortedMembers returns the members of sel ordered by test ID, which makes the
// decision independent of map iteration and of input order.
func sortedMembers(sel map[string]Member) []Member {
	members := make([]Member, 0, len(sel))
	for _, m := range sel {
		members = append(members, m)
	}
	slices.SortFunc(members, func(a, b Member) int { return cmp.Compare(a.TestID, b.TestID) })
	return members
}
