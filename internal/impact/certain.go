package impact

import (
	"sort"

	"github.com/omorsi45/hazard/internal/change"
	"github.com/omorsi45/hazard/internal/graph"
	"github.com/omorsi45/hazard/internal/index"
)

// reasonOrder lists the CERTAIN reasons from the strongest to the weakest. A
// test selected by several rules keeps the first.
var reasonOrder = []string{reasonDirect, reasonCoverage, reasonGraph, reasonFixture, reasonAlwaysRun}

// certainSet accumulates the CERTAIN members and their strongest reason.
type certainSet map[string]int

func (c certainSet) add(testID, reason string) {
	rank := len(reasonOrder)
	for i, r := range reasonOrder {
		if r == reason {
			rank = i
			break
		}
	}
	if prev, ok := c[testID]; !ok || rank < prev {
		c[testID] = rank
	}
}

func (c certainSet) has(testID string) bool {
	_, ok := c[testID]
	return ok
}

// members returns the set sorted by test ID.
func (c certainSet) members() []Member {
	out := make([]Member, 0, len(c))
	for id, rank := range c {
		out = append(out, Member{TestID: id, Reason: reasonOrder[rank]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TestID < out[j].TestID })
	return out
}

// addDirect selects the tests whose own file changed. A changed test file
// also selects every test of the package that owns it, because helpers in a
// test file are shared across the package and are not instrumented by
// coverage.
func addDirect(c certainSet, inv *inventory, m *index.Manifest, files []change.ChangedFile) {
	changed := map[string]bool{}
	for _, f := range files {
		changed[f.Path] = true
		if f.OldPath != "" {
			changed[f.OldPath] = true
		}
	}
	for _, t := range inv.tests {
		if changed[t.File] {
			c.add(t.ID, reasonDirect)
		}
	}
	for _, f := range files {
		if f.Kind != change.KindTest && f.OldKind != change.KindTest {
			continue
		}
		for _, pkg := range testFileOwners(m.Graph, f) {
			for _, id := range inv.byPkg[pkg] {
				c.add(id, reasonDirect)
			}
		}
	}
}

// testFileOwners returns the graph packages that own a changed test file. A
// test-kind file that is not source code (test data) has no listed owner, so
// every package whose directory contains it owns it.
func testFileOwners(g *graph.Graph, f change.ChangedFile) []string {
	paths := changedPaths(f)
	seen := map[string]bool{}
	var owners []string
	add := func(id string) {
		if !seen[id] {
			seen[id] = true
			owners = append(owners, id)
		}
	}
	for _, p := range paths {
		if id, ok := g.PackageOfFile(p); ok {
			add(id)
		}
	}
	if fileLanguage(f) == "" {
		for _, p := range paths {
			for _, id := range packagesContaining(g, p) {
				add(id)
			}
		}
	}
	return owners
}

// addFixture selects every test of each package whose directory contains a
// changed fixture file.
func addFixture(c certainSet, inv *inventory, m *index.Manifest, files []change.ChangedFile) {
	for _, f := range files {
		if f.Kind != change.KindFixture && f.OldKind != change.KindFixture {
			continue
		}
		for _, p := range changedPaths(f) {
			for _, pkg := range packagesContaining(m.Graph, p) {
				for _, id := range inv.byPkg[pkg] {
					c.add(id, reasonFixture)
				}
			}
		}
	}
}

// addAlwaysRun selects the caller's always-run tests and every unstable
// test. An always-run ID missing from the inventory is kept: the caller owns
// the decision about an ID the index cannot see.
func addAlwaysRun(c certainSet, inv *inventory, always map[string]string) {
	for id := range always {
		c.add(id, reasonAlwaysRun)
	}
	for _, t := range inv.tests {
		if t.Unstable {
			c.add(t.ID, reasonAlwaysRun)
		}
	}
}

// changedPaths returns Path and, when set and different, OldPath.
func changedPaths(f change.ChangedFile) []string {
	if f.OldPath != "" && f.OldPath != f.Path {
		return []string{f.Path, f.OldPath}
	}
	return []string{f.Path}
}

// packagesContaining returns the IDs, sorted, of the packages whose directory
// contains path.
func packagesContaining(g *graph.Graph, p string) []string {
	if g == nil {
		return nil
	}
	var ids []string
	for id, pkg := range g.Packages {
		if dirContains(pkg.Dir, p) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}
