package impact

import (
	"math"
	"sort"
	"strings"
	"unicode"

	"github.com/omorsi45/hazard/internal/change"
	"github.com/omorsi45/hazard/internal/index"
)

// Rank weights (seed 8.3) and the recency time constant in days.
const (
	weightOverlap   = 0.40
	weightHop       = 0.25
	weightCofailure = 0.20
	weightRecency   = 0.10
	weightSymbol    = 0.05
	recencyDays     = 30.0

	unknownDaysSinceFail = 365.0
)

// components are the five terms of the candidate rank, each in [0, 1].
type components struct {
	overlap, hop, cofailure, recency, symbol float64
}

// rank is the weighted sum of the components. The explicit conversions stop
// the compiler fusing a multiply and an add, so the sum is bit-identical on
// every platform and ties are decided the same way everywhere.
func (c components) rank() float64 {
	return float64(weightOverlap*c.overlap) + float64(weightHop*c.hop) +
		float64(weightCofailure*c.cofailure) + float64(weightRecency*c.recency) +
		float64(weightSymbol*c.symbol)
}

// value returns the component named by a candidate_ranking token.
func (c components) value(token string) float64 {
	switch token {
	case rankCoverageOverlap:
		return c.overlap
	case rankHops:
		return c.hop
	case rankCofailure:
		return c.cofailure
	case rankRecency:
		return c.recency
	}
	return 0
}

// ranked is a scored candidate before the cap.
type ranked struct {
	cand Candidate
	comp components
}

// rankedLess orders by Rank descending, then by the components in the order
// of the configured candidate_ranking (higher first), then by TestID. The
// result is a total order because test IDs are unique.
func rankedLess(a, b ranked, order []string) bool {
	if a.cand.Rank != b.cand.Rank {
		return a.cand.Rank > b.cand.Rank
	}
	for _, token := range order {
		if x, y := a.comp.value(token), b.comp.value(token); x != y {
			return x > y
		}
	}
	return a.cand.TestID < b.cand.TestID
}

// ranker scores residual candidates against one change.
type ranker struct {
	inv          *inventory
	hist         History
	hops         map[string]int
	changedFile  map[string]bool
	changedList  []string
	changedPkgs  map[string]bool
	symbols      map[string]bool
	read         func(string) ([]byte, error)
	symbolHits   map[string]int // test file -> distinct changed symbols it mentions
	kinds        [8]int
	linesAdded   int
	linesRemoved int
	filesChanged int
	cov          *index.Coverage
	ev           coverageEval
	fresh, stale bool
}

func newRanker(in Input, inv *inventory, files []change.ChangedFile, res *Result, ev coverageEval) *ranker {
	r := &ranker{
		inv:          inv,
		hist:         in.History,
		hops:         res.TestHops,
		changedFile:  map[string]bool{},
		changedPkgs:  map[string]bool{},
		symbols:      changedSymbols(files),
		read:         in.ReadFile,
		symbolHits:   map[string]int{},
		linesAdded:   in.Change.LinesAdded,
		linesRemoved: in.Change.LinesRemoved,
		filesChanged: len(in.Change.Files),
		ev:           ev,
		fresh:        in.Mode == ModeCoverage && res.CoverageState == covFresh,
		stale:        in.Mode == ModeCoverage && res.CoverageState != covFresh,
	}
	if r.hist == nil {
		r.hist = NoHistory{}
	}
	if in.Index != nil {
		r.cov = in.Index.Coverage
	}
	for _, id := range res.ChangedPackages {
		r.changedPkgs[id] = true
	}
	seen := map[string]bool{}
	for _, f := range in.Change.Files {
		if k := int(f.Kind); k >= 0 && k < len(r.kinds) {
			r.kinds[k]++
		}
		for _, p := range changedPaths(f) {
			r.changedFile[p] = true
			if !seen[p] {
				seen[p] = true
				r.changedList = append(r.changedList, p)
			}
		}
	}
	sort.Strings(r.changedList)
	return r
}

// score builds the Candidate and rank components of one test.
func (r *ranker) score(t index.Test) ranked {
	f, c := r.analyze(t)
	return ranked{
		cand: Candidate{
			TestID: t.ID, TestName: t.Name, TestFile: t.File, Package: t.Package,
			Purpose: t.Purpose, Asserts: append([]string(nil), t.Asserts...),
			Features: f, Rank: c.rank(),
		},
		comp: c,
	}
}

// features builds the f2 record of one test.
func (r *ranker) features(t index.Test) Features {
	f, _ := r.analyze(t)
	return f
}

// analyze computes the f2 features and the rank components. Unknown history
// contributes zero to the rank and is recorded as unknown (365 days, no
// support, no runtime); a non-finite value is unknown, never evidence.
func (r *ranker) analyze(t index.Test) (Features, components) {
	f := Features{
		Hops:             unreachableHops,
		ChangedFileKinds: r.kinds,
		LinesAdded:       r.linesAdded,
		LinesRemoved:     r.linesRemoved,
		FilesChanged:     r.filesChanged,
	}
	var c components
	if hops, ok := r.hops[t.ID]; ok {
		f.Hops, f.Reachable = hops, true
		c.hop = 1 / (1 + float64(hops))
	}
	f.SameFile = r.changedFile[t.File]
	if pkg, ok := r.inv.pkgOf[t.ID]; ok {
		f.SamePackage = r.changedPkgs[pkg]
	}
	f.CoverageStale = r.stale
	if tc, ok := r.coverageOf(t.ID); ok && r.fresh && tc.Status == "covered" {
		f.CoverageKnown = true
		if r.ev.total > 0 {
			f.CoverageOverlap = float64(r.ev.coveredLines(tc)) / float64(r.ev.total)
			c.overlap = f.CoverageOverlap
		}
	}
	rate, support := r.hist.Cofailure(t.ID, r.changedList)
	f.CofailureRate, f.CofailureSupport = unit(rate), max(support, 0)
	c.cofailure = f.CofailureRate
	f.TestFailRate = unit(r.hist.FailRate(t.ID))
	f.DaysSinceLastFail = unknownDaysSinceFail
	if days, ok := r.hist.DaysSinceLastFail(t.ID); ok && !math.IsNaN(days) && !math.IsInf(days, 0) {
		f.DaysSinceLastFail = max(days, 0)
		c.recency = math.Exp(-f.DaysSinceLastFail / recencyDays)
	}
	if ms, ok := r.hist.MedianMS(t.ID); ok && ms > 0 {
		f.TestMedianMS = ms
	}
	f.SharedSymbolHits = r.hitsIn(t.File)
	if f.SharedSymbolHits > 0 {
		c.symbol = 1
	}
	name := strings.ToLower(t.Name + "\x00" + t.File)
	f.TestIsIntegration = strings.Contains(name, "integration") || strings.Contains(name, "e2e")
	return f, c
}

func (r *ranker) coverageOf(id string) (index.TestCoverage, bool) {
	if r.cov == nil {
		return index.TestCoverage{}, false
	}
	tc, ok := r.cov.Tests[id]
	return tc, ok
}

// hitsIn counts the distinct changed symbols the test file mentions as whole
// identifiers. Without a reader, or when the file cannot be read, the count
// is unknown and recorded as 0; the symbol term only orders candidates.
func (r *ranker) hitsIn(file string) int {
	if r.read == nil || len(r.symbols) == 0 {
		return 0
	}
	if n, done := r.symbolHits[file]; done {
		return n
	}
	n := 0
	if src, err := r.read(file); err == nil {
		found := map[string]bool{}
		forEachIdentifier(string(src), func(word string) {
			if r.symbols[word] {
				found[word] = true
			}
		})
		n = len(found)
	}
	r.symbolHits[file] = n
	return n
}

// changedSymbols returns the identifiers of every symbol the change touches,
// excluding the synthetic "__file__" and the blank identifier. A method
// symbol such as "(*T).M" or "T.M" yields both T and M.
func changedSymbols(files []change.ChangedFile) map[string]bool {
	out := map[string]bool{}
	for _, f := range files {
		for _, h := range f.Hunks {
			for _, s := range h.Symbols {
				if s == "__file__" {
					continue
				}
				forEachIdentifier(s, func(word string) {
					if word != "_" {
						out[word] = true
					}
				})
			}
		}
	}
	return out
}

// forEachIdentifier calls fn with every maximal run of letters, digits and
// underscores in s.
func forEachIdentifier(s string, fn func(string)) {
	start := -1
	for i, r := range s {
		if r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			fn(s[start:i])
			start = -1
		}
	}
	if start >= 0 {
		fn(s[start:])
	}
}

// unit maps a rate to [0, 1]; a non-finite value is unknown and reads as 0.
func unit(x float64) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return 0
	}
	return min(max(x, 0), 1)
}
