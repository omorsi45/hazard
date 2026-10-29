package predict

import (
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/omorsi45/hazard/internal/impact"
)

// sealStatic round-trips a through WriteStatic and ReadStatic so the result
// carries a verified digest.
func sealStatic(t *testing.T, a StaticArtifact) StaticArtifact {
	t.Helper()
	path := filepath.Join(t.TempDir(), "static.json")
	if err := WriteStatic(path, a); err != nil {
		t.Fatalf("WriteStatic: %v", err)
	}
	out, err := ReadStatic(path)
	if err != nil {
		t.Fatalf("ReadStatic: %v", err)
	}
	return out
}

func sealLogistic(t *testing.T, a LogisticArtifact) LogisticArtifact {
	t.Helper()
	path := filepath.Join(t.TempDir(), "logistic.json")
	if err := WriteLogistic(path, a); err != nil {
		t.Fatalf("WriteLogistic: %v", err)
	}
	out, err := ReadLogistic(path)
	if err != nil {
		t.Fatalf("ReadLogistic: %v", err)
	}
	return out
}

func localProbsString(ps []LocalProb) string {
	parts := make([]string, len(ps))
	for i, p := range ps {
		parts[i] = fmt.Sprintf("%s=%v", p.TestID, p.P)
	}
	return strings.Join(parts, " ")
}

func TestStaticVersionIsTheArtifactDigest(t *testing.T) {
	a := sealStatic(t, sampleStatic())
	if got, want := NewStatic(a).Version(), "p0/"+a.Digest; got != want {
		t.Errorf("Version() = %q, want %q", got, want)
	}
	if !strings.HasPrefix(NewStatic(a).Version(), "p0/sha256:") {
		t.Errorf("Version() = %q, want a p0/sha256: prefix", NewStatic(a).Version())
	}
}

func TestStaticScoreMapsRankThroughTheIsotonicSteps(t *testing.T) {
	s := NewStatic(sealStatic(t, sampleStatic())) // X 0.1 0.5 2, Y 0.05 0.25 0.75
	cands := []impact.Candidate{
		{TestID: "t7", Rank: 100},
		{TestID: "t1", Rank: 0},
		{TestID: "t5", Rank: 0.5},
		{TestID: "t3", Rank: 0.4},
		{TestID: "t4", Rank: 1},
		{TestID: "t6", Rank: 2},
		{TestID: "t2", Rank: 0.1},
	}
	got, err := s.Score(cands)
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	want := []LocalProb{
		{"t1", 0.05}, {"t2", 0.05}, {"t3", 0.05}, {"t4", 0.25}, {"t5", 0.25}, {"t6", 0.75}, {"t7", 0.75},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Score = %s\nwant    %s", localProbsString(got), localProbsString(want))
	}
	if cands[0].TestID != "t7" || cands[1].TestID != "t1" {
		t.Error("Score reordered its input")
	}
}

func TestStaticScoreOfNoCandidates(t *testing.T) {
	got, err := NewStatic(sealStatic(t, sampleStatic())).Score(nil)
	if err != nil || len(got) != 0 {
		t.Errorf("Score(nil) = %v, %v, want no probabilities and no error", got, err)
	}
}

func TestStaticScoreRefusesAnUnknownRank(t *testing.T) {
	s := NewStatic(sealStatic(t, sampleStatic()))
	for name, rank := range map[string]float64{"NaN": math.NaN(), "+Inf": math.Inf(1), "-Inf": math.Inf(-1)} {
		t.Run(name, func(t *testing.T) {
			got, err := s.Score([]impact.Candidate{{TestID: "ok", Rank: 1}, {TestID: "bad", Rank: rank}})
			if err == nil {
				t.Fatalf("Score = %s with no error", localProbsString(got))
			}
			if got != nil {
				t.Errorf("Score returned %s alongside an error", localProbsString(got))
			}
			if !strings.Contains(err.Error(), "bad") {
				t.Errorf("error %q does not name the test", err)
			}
		})
	}
}

func TestStaticScoreRefusesAnUnverifiedArtifact(t *testing.T) {
	sealed := sealStatic(t, sampleStatic())
	changed := sealed
	changed.Model = IsotonicModel{X: append([]float64(nil), sealed.Model.X...), Y: append([]float64(nil), sealed.Model.Y...)}
	changed.Model.Y[2] = 0.99
	cases := map[string]StaticArtifact{
		"never written":     sampleStatic(),
		"changed in memory": changed,
	}
	for name, a := range cases {
		t.Run(name, func(t *testing.T) {
			s := NewStatic(a)
			got, err := s.Score([]impact.Candidate{{TestID: "t", Rank: 1}})
			if err == nil {
				t.Fatalf("Score = %s with no error", localProbsString(got))
			}
			if !strings.Contains(err.Error(), "digest") {
				t.Errorf("error %q does not name the digest", err)
			}
		})
	}
	t.Run("invalid model", func(t *testing.T) {
		a := sealed
		a.Model = IsotonicModel{}
		if _, err := NewStatic(a).Score([]impact.Candidate{{TestID: "t", Rank: 1}}); err == nil {
			t.Error("Score accepted an artifact with an empty model")
		}
	})
}

// vectorModel is a hand-made model over the 25-value feature vector:
// z = 0.25 + 2*((hops-1)/2) - reachable + 4*testFailRate, then sigmoid(2z-1).
func vectorModel() LogisticModel {
	d := len(impact.Features{}.Vector())
	m := LogisticModel{
		FeatureVersion: impact.FeatureVersion,
		Means:          make([]float64, d),
		Scales:         make([]float64, d),
		Weights:        make([]float64, d),
		Bias:           0.25,
		PlattA:         2,
		PlattB:         -1,
		Iterations:     7,
		Converged:      true,
	}
	for j := range m.Scales {
		m.Scales[j] = 1
	}
	m.Means[0], m.Scales[0] = 1, 2
	m.Weights[0], m.Weights[1], m.Weights[7] = 2, -1, 4
	return m
}

func vectorArtifact(m LogisticModel) LogisticArtifact {
	return LogisticArtifact{
		Schema:         ArtifactSchema,
		FeatureVersion: impact.FeatureVersion,
		Model:          m,
		Training:       sampleTraining(DefaultLogisticOptions()),
	}
}

func sigmoidRef(t float64) float64 { return 1 / (1 + math.Exp(-t)) }

func TestLogisticPredictorVersionAndWeightsDigest(t *testing.T) {
	a := sealLogistic(t, vectorArtifact(vectorModel()))
	l := NewLogistic(a)
	if got, want := l.Version(), "p1/"+a.Digest; got != want {
		t.Errorf("Version() = %q, want %q", got, want)
	}
	if got := l.WeightsDigest(); got != a.Digest || !digestForm.MatchString(got) {
		t.Errorf("WeightsDigest() = %q, want the artifact digest %q", got, a.Digest)
	}
}

func TestLogisticPredictorScoreKnownVectors(t *testing.T) {
	if n := len(impact.Features{}.Vector()); n != 25 {
		t.Fatalf("feature vector has %d values, the fixtures assume 25", n)
	}
	l := NewLogistic(sealLogistic(t, vectorArtifact(vectorModel())))
	cands := []impact.Candidate{
		// x0 = (3-1)/2 = 1, reachable 1, fail rate 0.25: z = 0.25+2-1+1 = 2.25, t = 3.5.
		{TestID: "b", Features: impact.Features{Hops: 3, Reachable: true, TestFailRate: 0.25}},
		// x0 = 0, reachable 1: z = 0.25-1 = -0.75, t = -2.5.
		{TestID: "c", Features: impact.Features{Hops: 1, Reachable: true}},
		// unreachable gives hops 99: x0 = 49, z = 0.25+98 = 98.25, t = 195.5 (saturates at 1).
		{TestID: "a", Features: impact.Features{}},
	}
	got, err := l.Score(cands)
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	want := []LocalProb{{"a", sigmoidRef(195.5)}, {"b", sigmoidRef(3.5)}, {"c", sigmoidRef(-2.5)}}
	if len(got) != len(want) {
		t.Fatalf("Score = %s, want %s", localProbsString(got), localProbsString(want))
	}
	for i := range want {
		if got[i].TestID != want[i].TestID || !approx(got[i].P, want[i].P, 1e-12) {
			t.Errorf("Score[%d] = %s=%v, want %s=%v", i, got[i].TestID, got[i].P, want[i].TestID, want[i].P)
		}
	}
	if cands[0].TestID != "b" || cands[2].TestID != "a" {
		t.Error("Score reordered its input")
	}
}

func TestLogisticPredictorScoreOfNoCandidates(t *testing.T) {
	l := NewLogistic(sealLogistic(t, vectorArtifact(vectorModel())))
	if got, err := l.Score(nil); err != nil || len(got) != 0 {
		t.Errorf("Score(nil) = %v, %v, want no probabilities and no error", got, err)
	}
}

func TestLogisticPredictorScoreMatchesAFittedModel(t *testing.T) {
	// A fitted model scores a candidate exactly as the model scores its vector.
	fit, scale := vectorExamples()
	m, err := FitLogistic(fit, scale, DefaultLogisticOptions())
	if err != nil {
		t.Fatalf("FitLogistic: %v", err)
	}
	l := NewLogistic(sealLogistic(t, vectorArtifact(m)))
	f := impact.Features{Hops: 2, Reachable: true, TestFailRate: 0.25}
	got, err := l.Score([]impact.Candidate{{TestID: "t", Features: f}})
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	want, err := m.Prob(f.Vector())
	if err != nil {
		t.Fatalf("Prob: %v", err)
	}
	if len(got) != 1 || got[0].P != want {
		t.Errorf("Score = %v, want %v", got, want)
	}
	if !(want > 0.01 && want < 0.99) {
		t.Errorf("fixture probability %v is saturated, pick a candidate inside the training range", want)
	}
}

// vectorExamples builds fit and scale sets over full feature vectors where a
// higher fail rate and a nearer test raise the chance of a positive label, with
// enough overlap that a mid-range candidate is not saturated.
func vectorExamples() (fit, scale []Example) {
	g := lcg(5)
	build := func(n int, prefix string) []Example {
		var out []Example
		for i := 0; i < n; i++ {
			label := i%2 == 0
			f := impact.Features{Hops: 1 + int(g.next()*4), Reachable: true, TestFailRate: g.next() * 0.4}
			if label {
				f.TestFailRate += 0.1
				f.Hops = 1 + int(g.next()*3)
			}
			out = append(out, Example{Features: f.Vector(), Label: label, Weight: 1, Group: fmt.Sprintf("%s%03d", prefix, i)})
		}
		return out
	}
	return build(120, "f"), build(60, "s")
}

func TestLogisticPredictorScoreRefusesUnknown(t *testing.T) {
	good := vectorArtifact(vectorModel())
	// Weights that cancel an overflowing standardized value give a NaN score:
	// (3-1)/1e-320 and 1/1e-320 both overflow to +Inf and Inf-Inf is NaN.
	overflow := vectorModel()
	for j := range overflow.Scales {
		overflow.Scales[j] = 1e-320
		overflow.Weights[j] = 1
		if j%2 == 1 {
			overflow.Weights[j] = -1
		}
	}
	cases := []struct {
		name string
		a    LogisticArtifact
		c    impact.Candidate
	}{
		{"NaN feature", good, impact.Candidate{TestID: "bad", Features: impact.Features{Reachable: true, CoverageOverlap: math.NaN()}}},
		{"Inf feature", good, impact.Candidate{TestID: "bad", Features: impact.Features{Reachable: true, TestFailRate: math.Inf(1)}}},
		{"model of the wrong width", handLogistic(), impact.Candidate{TestID: "bad", Features: impact.Features{Reachable: true}}},
		{"NaN score", vectorArtifact(overflow), impact.Candidate{TestID: "bad", Features: impact.Features{Hops: 3, Reachable: true}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := NewLogistic(sealLogistic(t, tc.a))
			got, err := l.Score([]impact.Candidate{tc.c, {TestID: "ok", Features: impact.Features{Hops: 1, Reachable: true}}})
			if err == nil {
				t.Fatalf("Score = %s with no error", localProbsString(got))
			}
			if got != nil {
				t.Errorf("Score returned %s alongside an error", localProbsString(got))
			}
			if !strings.Contains(err.Error(), "bad") {
				t.Errorf("error %q does not name the test", err)
			}
		})
	}
}

func TestLogisticPredictorScoreRefusesAnUnverifiedArtifact(t *testing.T) {
	sealed := sealLogistic(t, vectorArtifact(vectorModel()))
	changed := sealed
	changed.Model.Weights = append([]float64(nil), sealed.Model.Weights...)
	changed.Model.Weights[0] = 3
	cases := map[string]LogisticArtifact{
		"never written":     vectorArtifact(vectorModel()),
		"changed in memory": changed,
	}
	for name, a := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := NewLogistic(a).Score([]impact.Candidate{{TestID: "t", Features: impact.Features{Reachable: true}}})
			if err == nil {
				t.Fatalf("Score = %s with no error", localProbsString(got))
			}
			if !strings.Contains(err.Error(), "digest") {
				t.Errorf("error %q does not name the digest", err)
			}
		})
	}
}

func TestSplitGroups(t *testing.T) {
	keys := func(m map[string]bool) []string {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	dates := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("2026-09-%03d/c%d", i+1, i)
		}
		return out
	}
	shuffled := func(in []string) []string {
		out := append([]string(nil), in...)
		for i := range out {
			j := (i*7 + 3) % len(out)
			out[i], out[j] = out[j], out[i]
		}
		return out
	}
	cases := []struct {
		name         string
		groups       []string
		frac         float64
		wantFit      []string
		wantScaleLen int
	}{
		{"eighty percent of ten", shuffled(dates(10)), 0.8, dates(10)[:8], 2},
		{"duplicates collapse", []string{"b", "a", "a", "c", "b"}, 0.5, []string{"a"}, 2},
		{"floor not round", dates(5), 0.5, dates(5)[:2], 3},
		{"floating point product just below an integer", dates(100), 0.57, dates(100)[:57], 43},
		{"fraction zero", dates(4), 0, nil, 4},
		{"fraction one", dates(4), 1, dates(4), 0},
		{"fraction below zero", dates(4), -0.5, nil, 4},
		{"fraction above one", dates(4), 1.5, dates(4), 0},
		{"NaN fraction fits nothing", dates(4), math.NaN(), nil, 4},
		{"single group", []string{"only"}, 0.8, nil, 1},
		{"no groups", nil, 0.8, nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := append([]string(nil), tc.groups...)
			fit, scale := SplitGroups(tc.groups, tc.frac)
			if fit == nil || scale == nil {
				t.Fatalf("SplitGroups returned a nil map: fit %v scale %v", fit == nil, scale == nil)
			}
			wantFit := tc.wantFit
			if wantFit == nil {
				wantFit = []string{}
			}
			if got := keys(fit); !reflect.DeepEqual(got, wantFit) {
				t.Errorf("fit = %v, want %v", got, wantFit)
			}
			if len(scale) != tc.wantScaleLen {
				t.Errorf("scale has %d groups, want %d: %v", len(scale), tc.wantScaleLen, keys(scale))
			}
			for g := range fit {
				if scale[g] {
					t.Errorf("group %q is on both sides", g)
				}
			}
			if !reflect.DeepEqual(tc.groups, in) {
				t.Error("SplitGroups modified its input")
			}
			fit2, scale2 := SplitGroups(tc.groups, tc.frac)
			if !reflect.DeepEqual(fit, fit2) || !reflect.DeepEqual(scale, scale2) {
				t.Error("SplitGroups is not deterministic")
			}
		})
	}
}

func TestSplitGroupsCoversEveryGroupOnce(t *testing.T) {
	groups := []string{"g5", "g1", "g3", "g1", "g2", "g4"}
	fit, scale := SplitGroups(groups, 0.6)
	seen := map[string]int{}
	for g := range fit {
		seen[g]++
	}
	for g := range scale {
		seen[g]++
	}
	if len(seen) != 5 {
		t.Fatalf("split covers %d groups, want 5: %v", len(seen), seen)
	}
	for g, n := range seen {
		if n != 1 {
			t.Errorf("group %q appears %d times", g, n)
		}
	}
	// Sorted unique groups g1..g5 with floor(5*0.6)=3 in the fit set.
	for _, g := range []string{"g1", "g2", "g3"} {
		if !fit[g] {
			t.Errorf("%q should be in the fit set", g)
		}
	}
}
