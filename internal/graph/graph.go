// Package graph builds the local package and module dependency graph used to
// decide which tests can observe a changed package.
package graph

import (
	"path"
	"sort"
	"strings"
)

// Package is one node of the graph: a Go package or a Python module.
type Package struct {
	ID          string   // Go import path, or "py:<dotted module>"
	Language    string   // "go" | "python"
	Dir         string   // repo-relative, forward slashes
	Files       []string // repo-relative non-test source files
	TestFiles   []string // repo-relative test files
	Imports     []string // direct local imports from non-test files
	TestImports []string // direct local imports from test files
	External    []string // non-local imports, informational
	Dynamic     bool     // Python dynamic import seen: treat as unknown dependency
}

// Graph maps package IDs to packages.
type Graph struct {
	Packages map[string]*Package
}

// Merge returns a graph holding every package of the non-nil inputs. A later
// graph replaces an earlier package with the same ID.
func Merge(gs ...*Graph) *Graph {
	merged := &Graph{Packages: map[string]*Package{}}
	for _, g := range gs {
		if g == nil {
			continue
		}
		for id, p := range g.Packages {
			merged.Packages[id] = p
		}
	}
	return merged
}

// PackageOfFile returns the package that lists rel in Files or TestFiles, or
// the Go package whose Dir directly contains the .go file rel.
func (g *Graph) PackageOfFile(rel string) (string, bool) {
	if g == nil {
		return "", false
	}
	ids := make([]string, 0, len(g.Packages))
	for id := range g.Packages {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		p := g.Packages[id]
		for _, list := range [][]string{p.Files, p.TestFiles} {
			for _, f := range list {
				if f == rel {
					return id, true
				}
			}
		}
	}
	if strings.HasSuffix(rel, ".go") {
		dir := path.Dir(rel)
		for _, id := range ids {
			if p := g.Packages[id]; p.Language == "go" && p.Dir == dir {
				return id, true
			}
		}
	}
	return "", false
}
