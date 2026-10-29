package impact

import (
	"errors"
	"fmt"
	"path"
	"sort"

	"github.com/omorsi45/hazard/internal/change"
	"github.com/omorsi45/hazard/internal/index"
)

// Full-suite directive prefixes and reasons.
const (
	fullNoIndex         = "no_index"
	fullIndexIncomplete = "index_incomplete"
	fullTriggerPrefix   = "trigger:"
	fullUnmappedPrefix  = "unmapped_change:"
	fullDynamicPrefix   = "python_dynamic_import:"
)

// L1 computes the CERTAIN set, ranked candidates and features for one change.
// A non-empty Result.FullReason means the caller must run the full suite; in
// that case the selection fields are empty. L1 returns an error only for an
// invalid invocation.
func L1(in Input) (*Result, error) {
	if err := validate(in); err != nil {
		return nil, err
	}
	inv := newInventory(in.Index)
	res := &Result{TestHops: map[string]int{}, AllTests: inv.ids}
	if in.Index != nil {
		res.PackageCount = inv.packagesWithTests()
	}
	switch {
	case in.Index == nil:
		res.FullReason = fullNoIndex
		return res, nil
	case !in.Index.Complete:
		res.FullReason = fullIndexIncomplete
		return res, nil
	case len(in.Change.TriggersFull) > 0:
		res.FullReason = fullTriggerPrefix + in.Change.TriggersFull[0]
		return res, nil
	}
	files := sortedFiles(in.Change)
	m := mapChange(in.Index, files)
	if m.unmapped != "" {
		res.FullReason = fullUnmappedPrefix + m.unmapped
		return res, nil
	}
	if m.pythonChanged {
		if dyn := dynamicPackage(in.Index); dyn != "" {
			res.FullReason = fullDynamicPrefix + dyn
			return res, nil
		}
	}
	res.ChangedPackages = m.changed
	hops := in.Index.Graph.ReverseHops(m.changed)
	for id, pkg := range inv.pkgOf {
		if h, ok := hops[pkg]; ok {
			res.TestHops[id] = h
		}
	}
	res.CoverageState = coverageStateOf(in, inv, files)
	if in.Mode == ModeCoverage && res.CoverageState != covFresh && in.Impact.CoverageRequired {
		res.FullReason = fullCoverageRequiredPrefix + res.CoverageState
		return res, nil
	}
	certain := certainSet{}
	addDirect(certain, inv, in.Index, files)
	var ev coverageEval
	switch {
	case in.Mode == ModeGraph:
		for id, h := range res.TestHops {
			if h <= in.Impact.MaxHops {
				certain.add(id, reasonGraph)
			}
		}
	case res.CoverageState == covFresh:
		ev = evalCoverage(in, files, m)
		selectByCoverage(certain, in, inv, ev, res.TestHops)
	default:
		// Missing or stale coverage never narrows: every reachable test is
		// selected, whatever the hop bound.
		for id := range res.TestHops {
			certain.add(id, reasonGraph)
		}
	}
	addFixture(certain, inv, in.Index, files)
	addAlwaysRun(certain, inv, in.AlwaysRun)
	res.Certain = certain.members()
	rankCandidates(res, in, inv, files, certain, ev)
	return res, nil
}

// rankCandidates ranks every inventory test that is not CERTAIN and keeps the
// highest ranked up to the cap. The cap applies to candidates only: it never
// removes a CERTAIN member.
func rankCandidates(res *Result, in Input, inv *inventory, files []change.ChangedFile, certain certainSet, ev coverageEval) {
	rk := newRanker(in, inv, files, res, ev)
	var scored []ranked
	for _, t := range inv.tests {
		if !certain.has(t.ID) {
			scored = append(scored, rk.score(t))
		}
	}
	sort.Slice(scored, func(i, j int) bool { return rankedLess(scored[i], scored[j], in.Impact.CandidateRanking) })
	keep := min(candidateCap(in), len(scored))
	res.RawCandidates, res.Truncated = len(scored), len(scored)-keep
	for _, s := range scored[:keep] {
		res.Candidates = append(res.Candidates, s.cand)
	}
}

// validate rejects inputs no safe selection can be computed from.
func validate(in Input) error {
	if in.Change == nil {
		return errors.New("impact: nil change set")
	}
	if in.Mode != ModeGraph && in.Mode != ModeCoverage {
		return fmt.Errorf("impact: unknown mode %d", in.Mode)
	}
	if in.Impact.MaxHops < 0 {
		return fmt.Errorf("impact: negative max_hops %d", in.Impact.MaxHops)
	}
	if candidateCap(in) <= 0 {
		return errors.New("impact: candidate cap must be positive")
	}
	for _, token := range in.Impact.CandidateRanking {
		switch token {
		case rankCoverageOverlap, rankHops, rankCofailure, rankRecency:
		default:
			return fmt.Errorf("impact: unknown candidate_ranking token %q", token)
		}
	}
	return nil
}

// candidateCap is the number of candidates kept.
func candidateCap(in Input) int {
	if in.MaxCandidates > 0 {
		return in.MaxCandidates
	}
	return in.Impact.MaxCandidates
}

// Tokens of config.Impact.CandidateRanking.
const (
	rankCoverageOverlap = "coverage_overlap"
	rankHops            = "hops"
	rankCofailure       = "cofailure"
	rankRecency         = "recency"
)

func sortedFiles(cs *change.ChangeSet) []change.ChangedFile {
	files := append([]change.ChangedFile(nil), cs.Files...)
	sort.SliceStable(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files
}

// changeMapping is the graph package association of a change set.
type changeMapping struct {
	changed       []string            // sorted union of mapped package IDs
	filePkgs      map[string][]string // changed Path -> mapped package IDs
	unmapped      string              // Path of the first enabled-language file with no package
	pythonChanged bool                // a Python file changed (either side)
}

// mapChange maps each changed file of an enabled language to graph packages
// through its path and, when it has one, its old path.
func mapChange(m *index.Manifest, files []change.ChangedFile) changeMapping {
	out := changeMapping{filePkgs: map[string][]string{}}
	union := map[string]bool{}
	for _, f := range files {
		lang := fileLanguage(f)
		if lang == "" {
			continue
		}
		if lang == "python" {
			out.pythonChanged = true
		}
		paths := []string{f.Path}
		if f.OldPath != "" && f.OldPath != f.Path {
			paths = append(paths, f.OldPath)
		}
		var pkgs []string
		for _, p := range paths {
			if id, ok := m.Graph.PackageOfFile(p); ok && !contains(pkgs, id) {
				pkgs = append(pkgs, id)
			}
		}
		if len(pkgs) == 0 {
			if out.unmapped == "" {
				out.unmapped = f.Path
			}
			continue
		}
		sort.Strings(pkgs)
		out.filePkgs[f.Path] = pkgs
		for _, id := range pkgs {
			union[id] = true
		}
	}
	for id := range union {
		out.changed = append(out.changed, id)
	}
	sort.Strings(out.changed)
	return out
}

// fileLanguage is the enabled language of a changed file, "" when none. A
// file renamed away from an enabled language still counts as that language.
func fileLanguage(f change.ChangedFile) string {
	if f.Language == "go" || f.Language == "python" {
		return f.Language
	}
	switch path.Ext(f.OldPath) {
	case ".go":
		return "go"
	case ".py":
		return "python"
	}
	return ""
}

// dynamicPackage returns the first Python package, in ID order, that imports
// dynamically.
func dynamicPackage(m *index.Manifest) string {
	if m.Graph == nil {
		return ""
	}
	first := ""
	for id, p := range m.Graph.Packages {
		if p.Language == "python" && p.Dynamic && (first == "" || id < first) {
			first = id
		}
	}
	return first
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
