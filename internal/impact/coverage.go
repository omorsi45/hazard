package impact

import (
	"sort"

	"github.com/omorsi45/hazard/internal/change"
	"github.com/omorsi45/hazard/internal/index"
)

// Coverage states reported in Result.CoverageState.
const (
	covFresh        = "fresh"
	covMissing      = "missing"
	covNotRequested = "not_requested"
	covStalePrefix  = "stale:"

	fullCoverageRequiredPrefix = "coverage_required:"
)

// coverageStateOf decides whether the index coverage may drive selection.
// Staleness is judged only from facts the caller supplies, and every doubt is
// stale: a stale state widens the selection to the unbounded graph rule.
func coverageStateOf(in Input, inv *inventory, files []change.ChangedFile) string {
	if in.Mode != ModeCoverage {
		return covNotRequested
	}
	cov := in.Index.Coverage
	switch {
	case cov == nil:
		return covMissing
	case in.CoverageAge < 0:
		return covStalePrefix + "not_ancestor"
	case in.CoverageAge > in.Impact.CoverageMaxAgeCommits:
		return covStalePrefix + "too_old"
	}
	for _, f := range files {
		if f.Status == "A" || f.Status == "R" {
			return covStalePrefix + "added_file"
		}
	}
	if inventoryDrifted(cov, inv) {
		return covStalePrefix + "inventory_changed"
	}
	return covFresh
}

// inventoryDrifted reports whether the symmetric difference between the
// coverage test IDs and the index test IDs exceeds 5% of the index inventory.
func inventoryDrifted(cov *index.Coverage, inv *inventory) bool {
	indexed := make(map[string]bool, len(inv.ids))
	diff := 0
	for _, id := range inv.ids {
		indexed[id] = true
		if _, ok := cov.Tests[id]; !ok {
			diff++
		}
	}
	for id := range cov.Tests {
		if !indexed[id] {
			diff++
		}
	}
	return diff*20 > len(inv.ids)
}

// span is an inclusive 1-based line range.
type span struct{ lo, hi int }

// mergeSpans returns the sorted union of the spans with overlapping or
// touching ranges joined.
func mergeSpans(in []span) []span {
	spans := append([]span(nil), in...)
	sort.Slice(spans, func(i, j int) bool { return spans[i].lo < spans[j].lo })
	var out []span
	for _, s := range spans {
		if s.hi < s.lo {
			s.lo, s.hi = s.hi, s.lo
		}
		if n := len(out); n > 0 && s.lo <= out[n-1].hi+1 {
			if s.hi > out[n-1].hi {
				out[n-1].hi = s.hi
			}
			continue
		}
		out = append(out, s)
	}
	return out
}

func spanLen(spans []span) int {
	n := 0
	for _, s := range spans {
		n += s.hi - s.lo + 1
	}
	return n
}

// spanOverlap counts the lines two merged span lists share.
func spanOverlap(a, b []span) int {
	n := 0
	for i, j := 0, 0; i < len(a) && j < len(b); {
		lo, hi := max(a[i].lo, b[j].lo), min(a[i].hi, b[j].hi)
		if lo <= hi {
			n += hi - lo + 1
		}
		if a[i].hi < b[j].hi {
			i++
		} else {
			j++
		}
	}
	return n
}

// oldSideSpans returns the base-side lines a file's hunks change: OldStart
// through OldStart+OldLines-1, and for a pure insertion the line before and
// the line after it.
func oldSideSpans(hunks []change.Hunk) []span {
	var spans []span
	for _, h := range hunks {
		if h.OldLines > 0 {
			spans = append(spans, span{h.OldStart, h.OldStart + h.OldLines - 1})
		} else {
			spans = append(spans, span{h.OldStart, h.OldStart + 1})
		}
	}
	return mergeSpans(spans)
}

// coverageEval is the per-file view of the changed source lines.
type coverageEval struct {
	known   map[string][]span // base path -> changed old-side lines, files whose coverage applies
	total   int               // changed old-side lines over every changed source file
	unknown []string          // packages of changed source files whose coverage does not apply
}

// blockUnion returns, per file, the merged lines that the blocks of any test
// contain, whatever the test's status.
func blockUnion(cov *index.Coverage) map[string][]span {
	byFile := map[string][]span{}
	for _, tc := range cov.Tests {
		for _, b := range tc.Blocks {
			byFile[b.File] = append(byFile[b.File], span{b.StartLine, b.EndLine})
		}
	}
	for file, spans := range byFile {
		byFile[file] = mergeSpans(spans)
	}
	return byFile
}

// evalCoverage classifies every changed source file as coverage-known or
// coverage-unknown. Coverage applies to a file only when the base content
// digest equals the digest recorded at collection, the diff gives its changed
// lines, and every changed old-side line lies in a block of some test. A line
// in no block of any test is not evidence that no test depends on it: it may
// not be instrumented at all (a package-level variable initializer is never a
// block), and a profile of executed blocks cannot tell that from a line no
// test ran. Such a file is unknown, like a digest mismatch.
func evalCoverage(in Input, files []change.ChangedFile, m changeMapping) coverageEval {
	ev := coverageEval{known: map[string][]span{}}
	cov := in.Index.Coverage
	unknown := map[string]bool{}
	var union map[string][]span // built on the first file that needs it
	for _, f := range files {
		if f.Kind == change.KindTest || fileLanguage(f) == "" {
			continue
		}
		base := f.Path
		if f.OldPath != "" {
			base = f.OldPath
		}
		spans := oldSideSpans(f.Hunks)
		ev.total += spanLen(spans)
		baseDigest, haveBase := in.BaseDigests[base]
		covDigest, haveCov := cov.FileDigests[base]
		if !f.Binary && len(f.Hunks) > 0 && haveBase && haveCov && baseDigest != "" && baseDigest == covDigest {
			if union == nil {
				union = blockUnion(cov)
			}
			if spanOverlap(union[base], spans) == spanLen(spans) {
				ev.known[base] = mergeSpans(append(ev.known[base], spans...))
				continue
			}
		}
		for _, id := range m.filePkgs[f.Path] {
			unknown[id] = true
		}
	}
	for id := range unknown {
		ev.unknown = append(ev.unknown, id)
	}
	sort.Strings(ev.unknown)
	return ev
}

// coveredLines counts the changed old-side lines of coverage-known files that
// the test's blocks cover.
func (ev coverageEval) coveredLines(tc index.TestCoverage) int {
	byFile := map[string][]span{}
	for _, b := range tc.Blocks {
		if _, ok := ev.known[b.File]; ok {
			byFile[b.File] = append(byFile[b.File], span{b.StartLine, b.EndLine})
		}
	}
	n := 0
	for file, spans := range byFile {
		n += spanOverlap(mergeSpans(spans), ev.known[file])
	}
	return n
}

// selectByCoverage applies rule 4d for fresh coverage: a test with coverage
// selects on a block overlapping a changed old-side line of a coverage-known
// file. A test without usable coverage, and every test reachable from a
// coverage-unknown file, falls back to the unbounded graph rule, which is a
// proven superset of coverage selection for statically linked code.
func selectByCoverage(c certainSet, in Input, inv *inventory, ev coverageEval, allHops map[string]int) {
	cov := in.Index.Coverage
	for _, t := range inv.tests {
		tc, ok := cov.Tests[t.ID]
		if !ok || tc.Status != "covered" {
			if _, reachable := allHops[t.ID]; reachable {
				c.add(t.ID, reasonGraph)
			}
			continue
		}
		if ev.coveredLines(tc) > 0 {
			c.add(t.ID, reasonCoverage)
		}
	}
	unknownHops := in.Index.Graph.ReverseHops(ev.unknown)
	for _, t := range inv.tests {
		if _, reachable := unknownHops[inv.pkgOf[t.ID]]; reachable {
			c.add(t.ID, reasonGraph)
		}
	}
}
