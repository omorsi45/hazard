package graph

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writeTree creates files (slash-separated relative path -> content) under dir.
func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

const goModule = "module example.com/m\n\ngo 1.21\n"

func TestLoadGoDiamond(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"go.mod":  goModule,
		"a/a.go":  "package a\n\nfunc A() int { return 1 }\n",
		"b/b.go":  "package b\n\nimport \"example.com/m/a\"\n\nfunc B() int { return a.A() }\n",
		"c/c.go":  "package c\n\nimport (\n\t\"unicode/utf8\"\n\n\t\"example.com/m/a\"\n)\n\nfunc C() int { return utf8.RuneLen('x') + a.A() }\n",
		"d/d.go":  "package d\n\nimport (\n\t\"example.com/m/b\"\n\t\"example.com/m/c\"\n)\n\nfunc D() (int, string) { return b.B(), c.C() }\n",
		"root.go": "package m\n\nimport \"example.com/m/d\"\n\nfunc Root() { d.D() }\n",
	})
	g, err := LoadGo(context.Background(), dir, []string{"./..."}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"example.com/m":   {"example.com/m/d"},
		"example.com/m/a": nil,
		"example.com/m/b": {"example.com/m/a"},
		"example.com/m/c": {"example.com/m/a"},
		"example.com/m/d": {"example.com/m/b", "example.com/m/c"},
	}
	if len(g.Packages) != len(want) {
		t.Fatalf("packages = %v", keys(g))
	}
	for id, imports := range want {
		p := g.Packages[id]
		if p == nil {
			t.Fatalf("missing %s in %v", id, keys(g))
		}
		if p.Language != "go" || p.ID != id {
			t.Errorf("%s: language/id = %q/%q", id, p.Language, p.ID)
		}
		if !reflect.DeepEqual(p.Imports, imports) {
			t.Errorf("%s imports = %v, want %v", id, p.Imports, imports)
		}
	}
	root := g.Packages["example.com/m"]
	if root.Dir != "." || !reflect.DeepEqual(root.Files, []string{"root.go"}) {
		t.Errorf("root dir/files = %q %v", root.Dir, root.Files)
	}
	c := g.Packages["example.com/m/c"]
	if c.Dir != "c" || !reflect.DeepEqual(c.Files, []string{"c/c.go"}) || !reflect.DeepEqual(c.External, []string{"unicode/utf8"}) {
		t.Errorf("c = %+v", c)
	}
	hops := g.ReverseHops([]string{"example.com/m/a"})
	if hops["example.com/m/d"] != 2 || hops["example.com/m"] != 3 {
		t.Errorf("hops over loaded graph = %v", hops)
	}
}

func TestLoadGoTestOnlyImports(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"go.mod":      goModule,
		"a/a.go":      "package a\n\nfunc A() int { return 1 }\n",
		"b/b.go":      "package b\n\nfunc B() int { return 2 }\n",
		"e/e.go":      "package e\n\nfunc E() int { return 3 }\n",
		"e/e_test.go": "package e\n\nimport (\n\t\"testing\"\n\n\t\"example.com/m/a\"\n)\n\nfunc TestE(t *testing.T) { _ = a.A() }\n",
		"e/x_test.go": "package e_test\n\nimport (\n\t\"testing\"\n\n\t\"example.com/m/b\"\n\t\"example.com/m/e\"\n)\n\nfunc TestX(t *testing.T) { _ = b.B() + e.E() }\n",
	})
	g, err := LoadGo(context.Background(), dir, []string{"./..."}, nil)
	if err != nil {
		t.Fatal(err)
	}
	e := g.Packages["example.com/m/e"]
	if e == nil {
		t.Fatalf("missing e in %v", keys(g))
	}
	if len(e.Imports) != 0 {
		t.Errorf("non-test imports = %v, want none", e.Imports)
	}
	if want := []string{"example.com/m/a", "example.com/m/b"}; !reflect.DeepEqual(e.TestImports, want) {
		t.Errorf("test imports = %v, want %v (self import excluded)", e.TestImports, want)
	}
	if want := []string{"e/e_test.go", "e/x_test.go"}; !reflect.DeepEqual(e.TestFiles, want) {
		t.Errorf("test files = %v, want %v", e.TestFiles, want)
	}
	if want := []string{"e/e.go"}; !reflect.DeepEqual(e.Files, want) {
		t.Errorf("files = %v, want %v", e.Files, want)
	}
	if want := []string{"testing"}; !reflect.DeepEqual(e.External, want) {
		t.Errorf("external = %v, want %v", e.External, want)
	}
	if got := g.ReverseHops([]string{"example.com/m/a"}); got["example.com/m/e"] != 1 {
		t.Errorf("test-only edge missing from hops: %v", got)
	}
}

func TestLoadGoBuildTags(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"go.mod":       goModule,
		"f/f.go":       "package f\n\nfunc F() int { return 1 }\n",
		"g/g.go":       "package g\n\nimport \"example.com/m/f\"\n\nfunc G() int { return f.F() }\n",
		"g/special.go": "//go:build special\n\npackage g\n\nimport \"example.com/m/h\"\n\nfunc S() int { return h.H() }\n",
		"h/h.go":       "package h\n\nfunc H() int { return 2 }\n",
	})
	plain, err := LoadGo(context.Background(), dir, []string{"./..."}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := plain.Packages["example.com/m/g"]; !reflect.DeepEqual(got.Files, []string{"g/g.go"}) || !reflect.DeepEqual(got.Imports, []string{"example.com/m/f"}) {
		t.Errorf("untagged g = %+v", got)
	}
	tagged, err := LoadGo(context.Background(), dir, []string{"./..."}, []string{"special"})
	if err != nil {
		t.Fatal(err)
	}
	got := tagged.Packages["example.com/m/g"]
	if !reflect.DeepEqual(got.Files, []string{"g/g.go", "g/special.go"}) || !reflect.DeepEqual(got.Imports, []string{"example.com/m/f", "example.com/m/h"}) {
		t.Errorf("tagged g = %+v", got)
	}
}

func TestLoadGoPackageErrorIsError(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"syntax error": {
			"go.mod": goModule,
			"a/a.go": "package a\n\nimport \"fmt\n",
		},
		"unresolved local import": {
			"go.mod": goModule,
			"a/a.go": "package a\n\nimport \"example.com/m/missing\"\n\nfunc A() int { return missing.X }\n",
		},
		"mixed package names": {
			"go.mod": goModule,
			"a/a.go": "package a\n",
			"a/b.go": "package b\n\nfunc B() {}\n",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeTree(t, dir, files)
			g, err := LoadGo(context.Background(), dir, []string{"./..."}, nil)
			if err == nil || g != nil {
				t.Fatalf("package error must fail the load, got graph=%v err=%v", g, err)
			}
		})
	}
}

func TestLoadGoCanceledContext(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"go.mod": goModule, "a/a.go": "package a\n"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	g, err := LoadGo(ctx, dir, []string{"./..."}, nil)
	if err == nil || g != nil || !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("canceled load: graph=%v err=%v", g, err)
	}
}

func TestLoadGoArgs(t *testing.T) {
	json := "-json=" + goListFields
	if got, want := GoListArgs([]string{"./..."}, nil), []string{"list", json, "./..."}; !reflect.DeepEqual(got, want) {
		t.Errorf("args = %v, want %v", got, want)
	}
	if got, want := GoListArgs([]string{"./a", "./b"}, []string{"x", "y"}), []string{"list", json, "-tags", "x,y", "./a", "./b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("args = %v, want %v", got, want)
	}
}

func keys(g *Graph) []string {
	var ids []string
	for id := range g.Packages {
		ids = append(ids, id)
	}
	return ids
}
