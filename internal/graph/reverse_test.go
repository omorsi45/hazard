package graph

import (
	"reflect"
	"testing"
)

func pkgs(ps ...*Package) *Graph {
	g := &Graph{Packages: map[string]*Package{}}
	for _, p := range ps {
		g.Packages[p.ID] = p
	}
	return g
}

func TestReverseHopsDistances(t *testing.T) {
	// d imports b and c; b and c import a; e imports d; f is unrelated.
	g := pkgs(
		&Package{ID: "m/a"},
		&Package{ID: "m/b", Imports: []string{"m/a"}},
		&Package{ID: "m/c", Imports: []string{"m/a"}},
		&Package{ID: "m/d", Imports: []string{"m/b", "m/c"}},
		&Package{ID: "m/e", Imports: []string{"m/d"}},
		&Package{ID: "m/f"},
	)
	got := g.ReverseHops([]string{"m/a"})
	want := map[string]int{"m/a": 0, "m/b": 1, "m/c": 1, "m/d": 2, "m/e": 3}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReverseHops(a) = %v, want %v", got, want)
	}
	if _, ok := got["m/f"]; ok {
		t.Fatalf("unrelated package reachable: %v", got)
	}
}

func TestReverseHopsShortestFromAnyChanged(t *testing.T) {
	g := pkgs(
		&Package{ID: "m/a"},
		&Package{ID: "m/x"},
		&Package{ID: "m/b", Imports: []string{"m/a"}},
		&Package{ID: "m/c", Imports: []string{"m/b"}},
		&Package{ID: "m/d", Imports: []string{"m/c", "m/x"}},
	)
	got := g.ReverseHops([]string{"m/a", "m/x"})
	want := map[string]int{"m/a": 0, "m/x": 0, "m/b": 1, "m/c": 2, "m/d": 1}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReverseHops = %v, want %v", got, want)
	}
}

func TestReverseHopsTestOnlyEdgesCount(t *testing.T) {
	g := pkgs(
		&Package{ID: "m/a"},
		&Package{ID: "m/b", TestImports: []string{"m/a"}},
	)
	got := g.ReverseHops([]string{"m/a"})
	if got["m/b"] != 1 || len(got) != 2 {
		t.Fatalf("test-only importer must be one hop away: %v", got)
	}
}

func TestReverseHopsCyclesTerminate(t *testing.T) {
	g := pkgs(
		&Package{ID: "m/a", Imports: []string{"m/b"}},
		&Package{ID: "m/b", Imports: []string{"m/c"}},
		&Package{ID: "m/c", Imports: []string{"m/a"}},
	)
	got := g.ReverseHops([]string{"m/a"})
	want := map[string]int{"m/a": 0, "m/c": 1, "m/b": 2}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cycle result = %v, want %v", got, want)
	}
}

func TestReverseHopsUnknownAndEmpty(t *testing.T) {
	g := pkgs(&Package{ID: "m/a"}, &Package{ID: "m/b", Imports: []string{"m/a", "m/missing"}})
	if got := g.ReverseHops(nil); len(got) != 0 {
		t.Fatalf("no changed packages must reach nothing: %v", got)
	}
	if got := g.ReverseHops([]string{"m/unknown"}); len(got) != 0 {
		t.Fatalf("unknown changed package must reach nothing: %v", got)
	}
	var nilGraph *Graph
	if got := nilGraph.ReverseHops([]string{"m/a"}); len(got) != 0 {
		t.Fatalf("nil graph: %v", got)
	}
	if got := g.ReverseHops([]string{"m/missing"}); len(got) != 0 {
		t.Fatalf("an import of a package outside the graph must not create a node: %v", got)
	}
}

func TestMerge(t *testing.T) {
	a := pkgs(&Package{ID: "m/a", Language: "go"})
	b := pkgs(&Package{ID: "py:x", Language: "python"}, &Package{ID: "m/a", Language: "go", Dir: "replaced"})
	got := Merge(a, nil, b)
	if len(got.Packages) != 2 || got.Packages["py:x"] == nil || got.Packages["m/a"].Dir != "replaced" {
		t.Fatalf("merge result: %+v", got.Packages)
	}
	if empty := Merge(); empty == nil || empty.Packages == nil || len(empty.Packages) != 0 {
		t.Fatalf("empty merge must be a usable graph: %+v", empty)
	}
}

func TestPackageOfFile(t *testing.T) {
	g := pkgs(
		&Package{ID: "m", Language: "go", Dir: ".", Files: []string{"main.go"}},
		&Package{ID: "m/internal/a", Language: "go", Dir: "internal/a", Files: []string{"internal/a/a.go"}, TestFiles: []string{"internal/a/a_test.go"}},
		&Package{ID: "py:pkg.mod", Language: "python", Dir: "pkg", Files: []string{"pkg/mod.py"}},
		&Package{ID: "py:tests.test_mod", Language: "python", Dir: "tests", TestFiles: []string{"tests/test_mod.py"}},
	)
	for _, tc := range []struct {
		file, want string
		ok         bool
	}{
		{"main.go", "m", true},
		{"internal/a/a.go", "m/internal/a", true},
		{"internal/a/a_test.go", "m/internal/a", true},
		{"internal/a/excluded_by_tag.go", "m/internal/a", true},
		{"root_other.go", "m", true},
		{"pkg/mod.py", "py:pkg.mod", true},
		{"tests/test_mod.py", "py:tests.test_mod", true},
		{"pkg/unlisted.py", "", false},
		{"internal/a/readme.md", "", false},
		{"internal/b/b.go", "", false},
		{"internal/a/sub/deep.go", "", false},
	} {
		got, ok := g.PackageOfFile(tc.file)
		if got != tc.want || ok != tc.ok {
			t.Errorf("PackageOfFile(%q) = %q, %v; want %q, %v", tc.file, got, ok, tc.want, tc.ok)
		}
	}
}
