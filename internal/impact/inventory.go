package impact

import (
	"sort"
	"strings"

	"github.com/omorsi45/hazard/internal/graph"
	"github.com/omorsi45/hazard/internal/index"
)

// inventory is the index test list joined with the graph package that owns
// each test.
type inventory struct {
	tests []index.Test        // unique by ID, sorted by ID
	pkgOf map[string]string   // test ID -> graph package ID, absent when unresolved
	byPkg map[string][]string // graph package ID -> sorted test IDs
	ids   []string            // sorted test IDs
}

// newInventory resolves the owning graph package of every index test. A test
// whose package cannot be resolved stays in the inventory with no package: it
// is never reachable, and it is still a candidate.
func newInventory(m *index.Manifest) *inventory {
	inv := &inventory{pkgOf: map[string]string{}, byPkg: map[string][]string{}}
	if m == nil {
		return inv
	}
	seen := map[string]bool{}
	for _, t := range m.Tests {
		if seen[t.ID] {
			continue
		}
		seen[t.ID] = true
		inv.tests = append(inv.tests, t)
	}
	sort.Slice(inv.tests, func(i, j int) bool { return inv.tests[i].ID < inv.tests[j].ID })
	owner := fileOwners(m.Graph)
	for _, t := range inv.tests {
		inv.ids = append(inv.ids, t.ID)
		if pkg := resolvePackage(m.Graph, owner, t); pkg != "" {
			inv.pkgOf[t.ID] = pkg
			inv.byPkg[pkg] = append(inv.byPkg[pkg], t.ID)
		}
	}
	return inv
}

// fileOwners maps every file a graph package lists to the first package, in
// ID order, that lists it.
func fileOwners(g *graph.Graph) map[string]string {
	owner := map[string]string{}
	if g == nil {
		return owner
	}
	ids := make([]string, 0, len(g.Packages))
	for id := range g.Packages {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		p := g.Packages[id]
		for _, list := range [][]string{p.TestFiles, p.Files} {
			for _, f := range list {
				if _, taken := owner[f]; !taken {
					owner[f] = id
				}
			}
		}
	}
	return owner
}

// resolvePackage finds the graph package a test belongs to. A Go test names
// its package by import path. A Python test names its module, or the module
// and class, without the "py:" prefix of the graph ID, so it resolves through
// the file that holds it, then through the module name with trailing class
// segments removed.
func resolvePackage(g *graph.Graph, owner map[string]string, t index.Test) string {
	if g == nil {
		return ""
	}
	if _, ok := g.Packages[t.Package]; ok {
		return t.Package
	}
	if id, ok := owner[t.File]; ok {
		return id
	}
	for name := t.Package; name != ""; {
		if _, ok := g.Packages["py:"+name]; ok {
			return "py:" + name
		}
		cut := strings.LastIndexByte(name, '.')
		if cut < 0 {
			break
		}
		name = name[:cut]
	}
	return ""
}

// packagesWithTests counts the packages that own at least one test.
func (inv *inventory) packagesWithTests() int { return len(inv.byPkg) }

// dirContains reports whether the repo-relative path lies under dir.
func dirContains(dir, path string) bool {
	if dir == "" || dir == "." {
		return true
	}
	return strings.HasPrefix(path, strings.TrimSuffix(dir, "/")+"/")
}
