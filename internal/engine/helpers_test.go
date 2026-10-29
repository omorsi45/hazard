package engine

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omorsi45/hazard/internal/config"
	"github.com/omorsi45/hazard/internal/index"
)

// TestMain removes repository-redirecting variables a surrounding git hook may
// have exported and ignores the user and system git configuration, so the
// fixture repositories behave the same on every machine.
func TestMain(m *testing.M) {
	for _, k := range []string{
		"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR",
		"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_PREFIX",
	} {
		if err := os.Unsetenv(k); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	for k, v := range map[string]string{"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1"} {
		if err := os.Setenv(k, v); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
}

// diamondFiles is a small Go module: core is imported by left and right, both
// are imported by top, and solo is unrelated.
func diamondFiles() map[string]string {
	pkg := func(name, imports, body string) string {
		return "package " + name + "\n\n" + imports + body
	}
	test := func(pkg, fn, call string) string {
		return "package " + pkg + "\n\nimport \"testing\"\n\nfunc " + fn + "(t *testing.T) {\n\tif " + call + " != 1 {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n"
	}
	return map[string]string{
		"go.mod":              "module example.com/m\n\ngo 1.21\n",
		"hazard.yml":          "version: 1\nlanguages:\n  python:\n    enabled: false\n",
		".gitignore":          ".hazard/\n",
		"README.md":           "# readme\n",
		"core/core.go":        pkg("core", "", "func Core() int { return 1 }\n"),
		"core/core_test.go":   test("core", "TestCore", "Core()"),
		"left/left.go":        pkg("left", "import \"example.com/m/core\"\n\n", "func Left() int { return core.Core() }\n"),
		"left/left_test.go":   test("left", "TestLeft", "Left()"),
		"right/right.go":      pkg("right", "import \"example.com/m/core\"\n\n", "func Right() int { return core.Core() }\n"),
		"right/right_test.go": test("right", "TestRight", "Right()"),
		"top/top.go":          pkg("top", "import (\n\t\"example.com/m/left\"\n\t\"example.com/m/right\"\n)\n\n", "func Top() int { return left.Left() + right.Right() - 1 }\n"),
		"top/top_test.go":     test("top", "TestTop", "Top()"),
		"solo/solo.go":        pkg("solo", "", "func Solo() int { return 1 }\n"),
		"solo/solo_test.go":   test("solo", "TestSolo", "Solo()"),
	}
}

const (
	idCore = "go:example.com/m/core:TestCore"
	idLeft = "go:example.com/m/left:TestLeft"
	idRght = "go:example.com/m/right:TestRight"
	idTop  = "go:example.com/m/top:TestTop"
	idSolo = "go:example.com/m/solo:TestSolo"
)

var diamondIDs = []string{idCore, idLeft, idRght, idSolo, idTop}

// fixture is a committed git repository on branch main.
type fixture struct {
	dir string
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(stdout.String())
}

func writeIn(t *testing.T, dir string, files map[string]string) {
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

func commitAll(t *testing.T, dir, msg string) string {
	t.Helper()
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "-m", msg)
	return gitIn(t, dir, "rev-parse", "HEAD")
}

// newFixture commits the diamond module, with overrides replacing or adding
// files, and returns the repository. No index exists yet.
func newFixture(t *testing.T, overrides map[string]string) *fixture {
	t.Helper()
	files := diamondFiles()
	for k, v := range overrides {
		files[k] = v
	}
	return newFixtureFiles(t, files)
}

// newFixtureFiles commits exactly files into a new repository on main.
func newFixtureFiles(t *testing.T, files map[string]string) *fixture {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeIn(t, dir, files)
	gitIn(t, dir, "init", "-q", "-b", "main")
	commitAll(t, dir, "init")
	return &fixture{dir: dir}
}

// indexNow builds and publishes the index for the current working tree.
func (f *fixture) indexNow(t *testing.T, coverage bool) {
	t.Helper()
	cfg, err := config.Load(filepath.Join(f.dir, "hazard.yml"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := index.Build(context.Background(), f.dir, cfg, index.Options{Coverage: coverage})
	if err != nil {
		t.Fatal(err)
	}
	if err := index.Publish(f.dir, m); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) write(t *testing.T, files map[string]string) {
	t.Helper()
	writeIn(t, f.dir, files)
}

func (f *fixture) git(t *testing.T, args ...string) string {
	t.Helper()
	return gitIn(t, f.dir, args...)
}

// fileURL is the file:// URL of a local directory, usable with a shallow clone.
func fileURL(dir string) string {
	p := filepath.ToSlash(dir)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return (&url.URL{Scheme: "file", Path: p}).String()
}

func selectOK(t *testing.T, opts Options) Decision {
	t.Helper()
	d, err := Select(context.Background(), opts)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	return d
}

func ids(sel []Selected) []string {
	out := make([]string, len(sel))
	for i, s := range sel {
		out[i] = s.TestID
	}
	return out
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
