package index

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/omorsi45/hazard/internal/graph"
)

const discoverMain = `package p

import "testing"

// TestAlpha checks the alpha path. It also covers beta.
func TestAlpha(t *testing.T) {
	got, want := 1, 2
	if got != want {
		t.Errorf("got %d", got)
	}
	if err := run(); err != nil {
		t.Fatal(err)
	}
	if got == 3 {
		t.Log("not an assertion")
	}
	if got > 0 {
		if want < 0 {
			t.Fatalf("nested")
		}
	}
	if got != want {
		t.Errorf("duplicate condition")
	}
	if got < 0 {
	} else {
		t.Error("else branch is not the body")
	}
}

func TestBeta(t *testing.T) {}

func TestHTTPServerV2(t *testing.T) {}

func Test(t *testing.T) {}

func Test_snake_case(t *testing.T) {}

func FuzzGamma(f *testing.F) {}

func Testlower(t *testing.T)            {}
func TestExtra(t *testing.T, x int)     {}
func TestValue(t testing.T)             {}
func TestResult(t *testing.T) int       { return 0 }
func TestGeneric[T any](t *testing.T)   {}
func FuzzWrong(t *testing.T)            {}
func TestWrongType(f *testing.F)        {}
func BenchmarkSpeed(b *testing.B)       {}
func ExampleThing()                     {}
func helperTest(t *testing.T)           {}
func (s *suite) TestMethod(t *testing.T) {}

type suite struct{}

func run() error { return nil }
`

const discoverLimits = `package p

import "testing"

func TestMany(t *testing.T) {
	x := 0
	if x == 1 { t.Error("1") }
	if x == 2 { t.Error("2") }
	if x == 3 { t.Error("3") }
	if x == 4 { t.Error("4") }
	if x == 5 { t.Error("5") }
	if x == 6 { t.Error("6") }
	if x == 7 { t.Error("7") }
}

func TestLong(t *testing.T) {
	name := "x"
	if name != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Error("long")
	}
	if name == "a" ||
		name == "b" ||
		name == "c" {
		t.Error("multiline")
	}
}
`

const discoverExternal = `package p_test

import (
	"testing"
	tt "testing"
)

var _ = testing.Short

func TestExternal(t *tt.T) {}
`

func discoverGraph() *graph.Graph {
	return &graph.Graph{Packages: map[string]*graph.Package{
		"example.com/m/p": {ID: "example.com/m/p", Language: "go", Dir: "p", TestFiles: []string{"p/limits_test.go", "p/p_test.go", "p/x_test.go"}},
		"py:other":        {ID: "py:other", Language: "python", Dir: ".", TestFiles: []string{"test_other.py"}},
	}}
}

func TestDiscoverGo(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"p/p_test.go":      discoverMain,
		"p/limits_test.go": discoverLimits,
		"p/x_test.go":      discoverExternal,
		"test_other.py":    "def test_a():\n    pass\n",
	})
	tests, err := discoverGo(dir, discoverGraph())
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Test{}
	for _, tc := range tests {
		if _, dup := byName[tc.Name]; dup {
			t.Fatalf("duplicate %s", tc.Name)
		}
		byName[tc.Name] = tc
	}
	var names []string
	for name := range byName {
		names = append(names, name)
	}
	wantNames := []string{"FuzzGamma", "Test", "TestAlpha", "TestBeta", "TestExternal", "TestHTTPServerV2", "TestLong", "TestMany", "Test_snake_case"}
	if !sameSet(names, wantNames) {
		t.Fatalf("discovered %v, want %v", names, wantNames)
	}
	alpha := byName["TestAlpha"]
	if alpha.ID != "go:example.com/m/p:TestAlpha" || alpha.Language != "go" || alpha.Package != "example.com/m/p" || alpha.File != "p/p_test.go" || alpha.RunnerID != "TestAlpha" || alpha.Line != 6 || alpha.Unstable {
		t.Errorf("alpha = %+v", alpha)
	}
	if alpha.Purpose != "TestAlpha checks the alpha path." {
		t.Errorf("purpose = %q", alpha.Purpose)
	}
	wantAsserts := []string{"got != want", "err != nil", "want < 0"}
	if !reflect.DeepEqual(alpha.Asserts, wantAsserts) {
		t.Errorf("asserts = %q, want %q", alpha.Asserts, wantAsserts)
	}
	for name, purpose := range map[string]string{
		"TestBeta":         "Test Beta",
		"TestHTTPServerV2": "Test HTTP Server V2",
		"Test":             "Test",
		"Test_snake_case":  "Test snake case",
		"FuzzGamma":        "Fuzz Gamma",
	} {
		if byName[name].Purpose != purpose {
			t.Errorf("%s purpose = %q, want %q", name, byName[name].Purpose, purpose)
		}
	}
	if ext := byName["TestExternal"]; ext.ID != "go:example.com/m/p:TestExternal" || ext.File != "p/x_test.go" || ext.Line != 10 {
		t.Errorf("external test = %+v", ext)
	}
	many := byName["TestMany"].Asserts
	if want := []string{"x == 1", "x == 2", "x == 3", "x == 4", "x == 5"}; !reflect.DeepEqual(many, want) {
		t.Errorf("asserts must stop at 5 in source order: %q", many)
	}
	long := byName["TestLong"].Asserts
	if len(long) != 2 || len([]rune(long[0])) != 80 || !strings.HasPrefix(long[0], `name != "aaaa`) {
		t.Errorf("long condition not truncated to 80: %q", long)
	}
	if len(long) == 2 && long[1] != `name == "a" || name == "b" || name == "c"` {
		t.Errorf("multi-line condition not collapsed: %q", long[1])
	}
}

func TestDiscoverGoTestsMatchesBuild(t *testing.T) {
	t.Parallel()
	files := map[string]string{
		"go.mod":           goFixtureMod,
		"p/p.go":           "package p\n",
		"p/p_test.go":      discoverMain,
		"p/limits_test.go": discoverLimits,
		"p/x_test.go":      discoverExternal,
	}
	dir := gitRepo(t, files)
	m, err := Build(context.Background(), dir, goOnly(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	var got []Test
	for _, rel := range []string{"p/limits_test.go", "p/p_test.go", "p/x_test.go"} {
		tests, err := DiscoverGoTests("example.com/m/p", rel, []byte(files[rel]))
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, tests...)
	}
	sort.Slice(got, func(i, j int) bool { return got[i].ID < got[j].ID })
	if len(got) != 9 {
		t.Fatalf("discovered %d tests, want 9: %+v", len(got), got)
	}
	if !reflect.DeepEqual(got, m.Tests) {
		t.Errorf("DiscoverGoTests differs from Build:\n got %+v\nwant %+v", got, m.Tests)
	}
}

func TestDiscoverGoTestsReadsOnlyTheGivenContent(t *testing.T) {
	t.Parallel()
	src := []byte("package p\n\nimport \"testing\"\n\n// TestHead checks the head.\nfunc TestHead(t *testing.T) {\n\tif 1 != 1 {\n\t\tt.Fatal(\"x\")\n\t}\n}\n")
	got, err := DiscoverGoTests("example.com/m/p", "no/such/dir/head_test.go", src)
	if err != nil {
		t.Fatal(err)
	}
	want := []Test{{
		ID: "go:example.com/m/p:TestHead", Language: "go", Package: "example.com/m/p", Name: "TestHead",
		File: "no/such/dir/head_test.go", RunnerID: "TestHead", Line: 6, Purpose: "TestHead checks the head.", Asserts: []string{"1 != 1"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DiscoverGoTests = %+v, want %+v", got, want)
	}
}

func TestDiscoverGoTestsRejectsUnparsableContent(t *testing.T) {
	t.Parallel()
	for name, src := range map[string]string{
		"syntax error": "package p\n\nfunc TestBroken( {\n",
		"empty":        "",
	} {
		tests, err := DiscoverGoTests("example.com/m/p", "p/p_test.go", []byte(src))
		if err == nil || tests != nil || !strings.Contains(err.Error(), "p/p_test.go") {
			t.Errorf("%s: DiscoverGoTests = %v, %v; want an error naming the file and no tests", name, tests, err)
		}
	}
}

func TestDiscoverGoSyntaxErrorFails(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"p/p_test.go": "package p\n\nfunc TestBroken( {\n"})
	g := &graph.Graph{Packages: map[string]*graph.Package{
		"example.com/m/p": {ID: "example.com/m/p", Language: "go", Dir: "p", TestFiles: []string{"p/p_test.go"}},
	}}
	if tests, err := discoverGo(dir, g); err == nil || tests != nil {
		t.Fatalf("unparsable test file must fail discovery, got %v %v", tests, err)
	}
	missing := &graph.Graph{Packages: map[string]*graph.Package{
		"example.com/m/p": {ID: "example.com/m/p", Language: "go", Dir: "p", TestFiles: []string{"p/gone_test.go"}},
	}}
	if tests, err := discoverGo(dir, missing); err == nil || tests != nil {
		t.Fatalf("missing test file must fail discovery, got %v %v", tests, err)
	}
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}
