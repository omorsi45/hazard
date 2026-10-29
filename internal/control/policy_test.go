package control

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"reflect"
	"regexp"
	"testing"
)

func basePolicy() Policy {
	return Policy{
		FeatureVersion:    "f2",
		Predictor:         "ensemble",
		MaxHops:           3,
		MaxCandidates:     200,
		CoverageRequired:  true,
		MinSavingRatio:    0.2,
		CandidateRanking:  []string{"hops", "coverage"},
		FullSuiteTriggers: []string{"go.mod", "go.sum"},
		GeneratedGlobs:    []string{"**/*.pb.go"},
		Redact:            "structure",
		TierEnabled:       false,
	}
}

func TestPolicyDigestIsTheHashOfTheCanonicalJSON(t *testing.T) {
	// The field names, their order and the number formatting are part of the
	// digest, so a rename or reorder shows up here.
	const wantJSON = `{"feature_version":"f2","predictor":"ensemble","max_hops":3,"max_candidates":200,"coverage_required":true,"min_saving_ratio":0.2,"candidate_ranking":["hops","coverage"],"full_suite_triggers":["go.mod","go.sum"],"generated_globs":["**/*.pb.go"],"redact":"structure","tier_enabled":false}`
	sum := sha256.Sum256([]byte(wantJSON))
	want := "sha256:" + hex.EncodeToString(sum[:])
	if got := basePolicy().Digest(); got != want {
		t.Errorf("Digest() = %q, want %q", got, want)
	}
	if !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(basePolicy().Digest()) {
		t.Errorf("Digest() = %q, want sha256: and 64 lowercase hex digits", basePolicy().Digest())
	}
}

func TestPolicyDigestEqualPoliciesAgree(t *testing.T) {
	a, b := basePolicy(), basePolicy()
	if a.Digest() == "" || a.Digest() != b.Digest() {
		t.Errorf("equal policies gave %q and %q", a.Digest(), b.Digest())
	}
}

func TestPolicyDigestChangesWithEveryField(t *testing.T) {
	changes := map[string]func(*Policy){
		"FeatureVersion":    func(p *Policy) { p.FeatureVersion = "f3" },
		"Predictor":         func(p *Policy) { p.Predictor = "logistic" },
		"MaxHops":           func(p *Policy) { p.MaxHops++ },
		"MaxCandidates":     func(p *Policy) { p.MaxCandidates++ },
		"CoverageRequired":  func(p *Policy) { p.CoverageRequired = !p.CoverageRequired },
		"MinSavingRatio":    func(p *Policy) { p.MinSavingRatio = 0.25 },
		"CandidateRanking":  func(p *Policy) { p.CandidateRanking = []string{"hops"} },
		"FullSuiteTriggers": func(p *Policy) { p.FullSuiteTriggers = append(p.FullSuiteTriggers, "Dockerfile") },
		"GeneratedGlobs":    func(p *Policy) { p.GeneratedGlobs = nil },
		"Redact":            func(p *Policy) { p.Redact = "hash" },
		"TierEnabled":       func(p *Policy) { p.TierEnabled = !p.TierEnabled },
	}
	typ := reflect.TypeOf(Policy{})
	for i := 0; i < typ.NumField(); i++ {
		if _, ok := changes[typ.Field(i).Name]; !ok {
			t.Errorf("Policy.%s has no case here: add it, and check that it belongs in the digest", typ.Field(i).Name)
		}
	}

	base := basePolicy().Digest()
	seen := map[string]string{base: "the base policy"}
	for field, change := range changes {
		p := basePolicy()
		change(&p)
		d := p.Digest()
		if d == base {
			t.Errorf("changing %s left the digest unchanged", field)
		}
		if other, dup := seen[d]; dup {
			t.Errorf("changing %s gave the same digest as %s", field, other)
		}
		seen[d] = field
	}
}

func TestPolicyDigestKeepsSliceOrder(t *testing.T) {
	for name, reorder := range map[string]func(*Policy){
		"CandidateRanking":  func(p *Policy) { p.CandidateRanking = []string{"coverage", "hops"} },
		"FullSuiteTriggers": func(p *Policy) { p.FullSuiteTriggers = []string{"go.sum", "go.mod"} },
	} {
		p := basePolicy()
		reorder(&p)
		if p.Digest() == basePolicy().Digest() {
			t.Errorf("reordering %s left the digest unchanged: the order is part of the policy", name)
		}
	}
}

func TestPolicyDigestTreatsNilAndEmptySlicesAlike(t *testing.T) {
	nilled, emptied := basePolicy(), basePolicy()
	nilled.GeneratedGlobs, nilled.CandidateRanking = nil, nil
	emptied.GeneratedGlobs, emptied.CandidateRanking = []string{}, []string{}
	if nilled.Digest() != emptied.Digest() {
		t.Errorf("nil slices gave %q, empty slices %q", nilled.Digest(), emptied.Digest())
	}
}

func TestPolicyDigestOfAPolicyThatCannotBeEncodedIsEmpty(t *testing.T) {
	// An empty digest never equals a stored one, so the lock check fails.
	p := basePolicy()
	p.MinSavingRatio = math.NaN()
	if got := p.Digest(); got != "" {
		t.Errorf("Digest() = %q, want empty for a NaN ratio", got)
	}
}
