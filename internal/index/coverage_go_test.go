package index

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/omorsi45/hazard/internal/graph"
)

func TestCoverageGoParseProfile(t *testing.T) {
	profile := "mode: set\n" +
		"example.com/m/a/a.go:3.19,4.11 1 1\n" +
		"example.com/m/a/a.go:5.3,5.11 1 0\n" +
		"example.com/m/b/b.go:7.2,9.3 2 4\n"
	got, err := parseCoverProfile(profile)
	want := []profileBlock{
		{File: "example.com/m/a/a.go", StartLine: 3, EndLine: 4, Count: 1},
		{File: "example.com/m/a/a.go", StartLine: 5, EndLine: 5, Count: 0},
		{File: "example.com/m/b/b.go", StartLine: 7, EndLine: 9, Count: 4},
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("parse = %+v, %v; want %+v", got, err, want)
	}
	if got, err := parseCoverProfile("mode: set\n"); err != nil || len(got) != 0 {
		t.Errorf("an empty profile has no blocks: %v %v", got, err)
	}
	for name, bad := range map[string]string{
		"no mode":       "example.com/m/a/a.go:3.19,4.11 1 1\n",
		"bad line":      "mode: set\nnot a block\n",
		"bad count":     "mode: set\nexample.com/m/a/a.go:3.19,4.11 1 x\n",
		"bad range":     "mode: set\nexample.com/m/a/a.go:3,4 1 1\n",
		"empty":         "",
		"windows colon": "mode: set\nC:\\x\\a.go:3.1,4.1 1 1 extra\n",
	} {
		if got, err := parseCoverProfile(bad); err == nil {
			t.Errorf("%s: accepted malformed profile: %v", name, got)
		}
	}
}

func TestCoverageGoModulePath(t *testing.T) {
	for name, tc := range map[string]struct {
		pkgs map[string]string // id -> dir
		want string
		ok   bool
	}{
		"root package":   {map[string]string{"example.com/m": ".", "example.com/m/a": "a"}, "example.com/m", true},
		"nested only":    {map[string]string{"example.com/m/a": "a", "example.com/m/internal/b": "internal/b"}, "example.com/m", true},
		"disagreement":   {map[string]string{"example.com/m/a": "a", "other.org/x": "x"}, "", false},
		"no go packages": {map[string]string{}, "", false},
	} {
		g := &graph.Graph{Packages: map[string]*graph.Package{}}
		for id, dir := range tc.pkgs {
			g.Packages[id] = &graph.Package{ID: id, Language: "go", Dir: dir}
		}
		g.Packages["py:x"] = &graph.Package{ID: "py:x", Language: "python", Dir: "."}
		got, ok := modulePathOf(g)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: modulePathOf = %q %v, want %q %v", name, got, ok, tc.want, tc.ok)
		}
	}
}

const coverageGoSource = "package a\n\nfunc A(x int) int {\n\tif x > 0 {\n\t\treturn 1\n\t}\n\treturn 2\n}\n"

func coverageFixture(extraTest string) map[string]string {
	return map[string]string{
		"go.mod": goFixtureMod,
		"a/a.go": coverageGoSource,
		"a/a_test.go": `package a

import "testing"

var seeded bool

func TestPos(t *testing.T) {
	if A(1) != 1 {
		t.Fatal("A(1)")
	}
}

// TestSeed only passes the shared state on, then skips.
func TestSeed(t *testing.T) {
	seeded = true
	t.Skip("state seeded for the next test")
}

// TestUsesSeed passes in a full run and fails when run alone.
func TestUsesSeed(t *testing.T) {
	if !seeded {
		t.Fatal("TestSeed did not run first")
	}
}
` + extraTest,
	}
}

func TestCoverageGoPerTestProfiles(t *testing.T) {
	t.Parallel()
	dir := gitRepo(t, coverageFixture(""))
	m, err := Build(context.Background(), dir, goOnly(), Options{Coverage: true})
	if err != nil {
		t.Fatal(err)
	}
	c := m.Coverage
	if c == nil {
		t.Fatalf("coverage missing, diagnostics: %v", m.Diagnostics)
	}
	if c.Granularity != "go-top-level-test" || c.HeadSHA != m.HeadSHA || c.ContentDigest != m.ContentDigest {
		t.Errorf("coverage header = %+v", c)
	}
	if want := map[string]string{"a/a.go": hexSHA(coverageGoSource)}; !reflect.DeepEqual(c.FileDigests, want) {
		t.Errorf("FileDigests = %v, want %v", c.FileDigests, want)
	}
	pos := c.Tests["go:example.com/m/a:TestPos"]
	if pos.Status != "covered" || !blocksCover(pos.Blocks, "a/a.go", 5) || blocksCover(pos.Blocks, "a/a.go", 7) {
		t.Errorf("TestPos = %+v: must cover the x>0 branch (line 5) and not line 7", pos)
	}
	uses := c.Tests["go:example.com/m/a:TestUsesSeed"]
	if uses.Status != "failed" || len(uses.Blocks) != 0 {
		t.Errorf("a test that fails when run alone must be failed with no blocks: %+v", uses)
	}
	seed := c.Tests["go:example.com/m/a:TestSeed"]
	if seed.Status != "not_run" || len(seed.Blocks) != 0 {
		t.Errorf("a skipped test has unknown coverage, never covered: %+v", seed)
	}
	if len(c.Tests) != 3 {
		t.Errorf("coverage tests = %v", c.Tests)
	}
	var baseline, perTest int
	for _, cmd := range m.Commands {
		joined := strings.Join(cmd.Argv, " ")
		switch {
		case len(cmd.Argv) >= 3 && cmd.Argv[0] == "go" && cmd.Argv[1] == "test" && !strings.Contains(joined, "-coverprofile"):
			baseline++
			if !strings.Contains(joined, "-count=1") || cmd.Exit != 0 {
				t.Errorf("baseline = %+v", cmd)
			}
		case strings.Contains(joined, "-coverprofile") && strings.Contains(joined, "-run ^TestPos$") && strings.Contains(joined, "-coverpkg=example.com/m/a") && strings.Contains(joined, "-count=1"):
			perTest++
		}
	}
	if baseline != 1 || perTest != 1 {
		t.Errorf("baseline=%d perTest=%d commands=%v", baseline, perTest, m.Commands)
	}
	if !m.Complete {
		t.Error("coverage collection must not change completeness of the inventory")
	}
}

func TestCoverageGoFailingBaselinePublishesWithoutCoverage(t *testing.T) {
	t.Parallel()
	dir := gitRepo(t, coverageFixture("\nfunc TestBroken(t *testing.T) { t.Fatal(\"always\") }\n"))
	m, err := Build(context.Background(), dir, goOnly(), Options{Coverage: true})
	if err != nil {
		t.Fatalf("a failing baseline must not fail the index: %v", err)
	}
	if m.Coverage != nil {
		t.Errorf("coverage must be nil when the baseline fails: %+v", m.Coverage)
	}
	var diag bool
	for _, d := range m.Diagnostics {
		diag = diag || strings.Contains(d, "coverage unavailable")
	}
	if !diag || len(m.Tests) != 4 {
		t.Errorf("diagnostics=%v tests=%d", m.Diagnostics, len(m.Tests))
	}
}

func blocksCover(blocks []Block, file string, line int) bool {
	for _, b := range blocks {
		if b.File == file && b.StartLine <= line && line <= b.EndLine {
			return true
		}
	}
	return false
}
