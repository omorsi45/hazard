package control

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// Policy is everything that changes a decision besides the lock threshold:
// the candidate cap and ranking, the graph and coverage rules, the widening
// and abstention knobs, the feature version, the predictor and the privacy
// level. Its digest goes into the lock (seed 7.1, Lock.PolicyDigest), so
// changing any field invalidates a lock calibrated under the old policy.
type Policy struct {
	FeatureVersion    string   `json:"feature_version"`
	Predictor         string   `json:"predictor"`
	MaxHops           int      `json:"max_hops"`
	MaxCandidates     int      `json:"max_candidates"`
	CoverageRequired  bool     `json:"coverage_required"`
	MinSavingRatio    float64  `json:"min_saving_ratio"`
	CandidateRanking  []string `json:"candidate_ranking"`
	FullSuiteTriggers []string `json:"full_suite_triggers"`
	GeneratedGlobs    []string `json:"generated_globs"`
	Redact            string   `json:"redact"`
	TierEnabled       bool     `json:"tier_enabled"`

	// L1Mode is the mode the deterministic layer ran in ("graph" or
	// "coverage"); the rest define the inventory and the coverage window the
	// floor was measured over.
	L1Mode                string   `json:"l1_mode"`
	CoverageMaxAgeCommits int      `json:"coverage_max_age_commits"`
	GoEnabled             bool     `json:"go_enabled"`
	GoPackages            []string `json:"go_packages"`
	GoTags                []string `json:"go_tags"`
	PythonEnabled         bool     `json:"python_enabled"`
	PythonTestpaths       []string `json:"python_testpaths"`
}

// Digest returns "sha256:" followed by the hex SHA-256 of the compact JSON of
// the policy. Fields marshal in declaration order and slices keep the order
// given, because a ranking or a trigger list is ordered input to the
// decision. A nil slice and an empty one are the same policy and digest
// alike. If the policy cannot be marshaled (a NaN or infinite ratio) it
// returns "", which never matches a stored digest.
func (p Policy) Digest() string {
	p.CandidateRanking = nonNil(p.CandidateRanking)
	p.FullSuiteTriggers = nonNil(p.FullSuiteTriggers)
	p.GeneratedGlobs = nonNil(p.GeneratedGlobs)
	p.GoPackages = nonNil(p.GoPackages)
	p.GoTags = nonNil(p.GoTags)
	p.PythonTestpaths = nonNil(p.PythonTestpaths)
	b, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// nonNil returns s, or an empty slice when s is nil, so both marshal as [].
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
