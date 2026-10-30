package mutate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omorsi45/hazard/internal/graph"
	"github.com/omorsi45/hazard/internal/store"
)

// TestMain keeps the caller's git configuration (commit signing included) and
// the repository variables of a surrounding hook away from the fixture
// repositories and from the driver's own git calls.
func TestMain(m *testing.M) {
	for name := range scrubbedGitEnv {
		if err := os.Unsetenv(name); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	for name, value := range map[string]string{"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1"} {
		if err := os.Setenv(name, value); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	code := m.Run()
	if shared.dir != "" {
		if err := os.RemoveAll(shared.dir); err != nil {
			fmt.Fprintln(os.Stderr, err)
			code = 1
		}
	}
	os.Exit(code)
}

func testGraph() *graph.Graph {
	return &graph.Graph{Packages: map[string]*graph.Package{
		"m/calc":     {ID: "m/calc", Language: "go", Dir: "calc", Files: []string{"calc/calc.go"}, TestFiles: []string{"calc/calc_test.go"}},
		"m/app":      {ID: "m/app", Language: "go", Dir: "app", Files: []string{"app/app.go"}, Imports: []string{"m/calc"}},
		"m/far":      {ID: "m/far", Language: "go", Dir: "far", Files: []string{"far/far.go"}, Imports: []string{"m/app"}},
		"m/cfgcheck": {ID: "m/cfgcheck", Language: "go", Dir: "cfgcheck", Files: []string{"cfgcheck/cfgcheck.go"}},
	}}
}

func TestClassifyKillersGoFile(t *testing.T) {
	killers := []string{"go:m/app:TestRun", "go:m/calc:TestMax", "go:m/cfgcheck:TestLimit", "go:m/far:TestFar"}
	unreachable, beyond := classifyKillers(testGraph(), "calc/calc.go", killers, 1)
	if want := []string{"go:m/cfgcheck:TestLimit"}; !reflect.DeepEqual(unreachable, want) {
		t.Errorf("unreachable = %v, want %v", unreachable, want)
	}
	if want := []string{"go:m/far:TestFar"}; !reflect.DeepEqual(beyond, want) {
		t.Errorf("beyond = %v, want %v (2 reverse hops with MaxHops 1)", beyond, want)
	}
	// MaxHops 2 brings the far package inside the bound; hops == MaxHops is within it.
	_, beyond = classifyKillers(testGraph(), "calc/calc.go", killers, 2)
	if len(beyond) != 0 {
		t.Errorf("beyond with MaxHops 2 = %v, want none", beyond)
	}
}

func TestClassifyKillersNonGoFileIsAllUnreachable(t *testing.T) {
	killers := []string{"go:m/app:TestRun", "go:m/calc:TestMax"}
	unreachable, beyond := classifyKillers(testGraph(), "config/limits.yaml", killers, 3)
	if !reflect.DeepEqual(unreachable, killers) || len(beyond) != 0 {
		t.Errorf("unreachable=%v beyond=%v, want every killer unreachable", unreachable, beyond)
	}
}

func TestClassifyKillersUnknownPackageIsUnreachable(t *testing.T) {
	killers := []string{"go:m/app:TestRun"}
	unreachable, _ := classifyKillers(testGraph(), "elsewhere/x.go", killers, 3)
	if !reflect.DeepEqual(unreachable, killers) {
		t.Errorf("unreachable = %v, want %v", unreachable, killers)
	}
}

func TestCandidateMutantsSelection(t *testing.T) {
	files := map[string]string{
		"calc/calc.go":            "package calc\n\nfunc Max(a, b int) int {\n\tif a > b {\n\t\treturn a\n\t}\n\treturn b\n}\n",
		"calc/calc_test.go":       "package calc\n\nfunc helper(a, b int) bool { return a > b }\n",
		"config/limits.yaml":      "limit: 10\nname: abc\n",
		"README.md":               "limit: 5\n",
		"calc/testdata/broken.go": "package broken\nfunc (",
		"vendor/x/x.go":           "package x\nfunc F(a, b int) bool { return a > b }\n",
	}
	tracked := []string{"README.md", "calc/calc.go", "calc/calc_test.go", "calc/testdata/broken.go", "config/limits.yaml", "vendor/x/x.go"}
	read := func(rel string) ([]byte, error) {
		s, ok := files[rel]
		if !ok {
			return nil, errors.New("missing " + rel)
		}
		return []byte(s), nil
	}
	ids := func(ms []Mutant) (files []string, ops map[string]int) {
		ops = map[string]int{}
		seen := map[string]bool{}
		for _, m := range ms {
			if !seen[m.File] {
				seen[m.File] = true
				files = append(files, m.File)
			}
			ops[m.Operator]++
		}
		return files, ops
	}

	got, err := candidateMutants(tracked, []string{"**/*.go", "config/*.yaml", "*.md"}, []string{"cmp", "cfg"}, read)
	if err != nil {
		t.Fatal(err)
	}
	fs, ops := ids(got)
	if len(fs) != 2 {
		t.Fatalf("mutated files = %v, want calc/calc.go and config/limits.yaml only (no tests, testdata, vendor, markdown)", fs)
	}
	if ops["cmp"] == 0 || ops["cfg"] == 0 {
		t.Errorf("operator counts = %v, want cmp and cfg mutants", ops)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].ID >= got[i].ID {
			t.Fatalf("candidates are not ordered by ID at %d: %s then %s", i, got[i-1].ID, got[i].ID)
		}
	}

	// cfg not selected: the YAML file contributes nothing.
	got, err = candidateMutants(tracked, []string{"**/*.go", "config/*.yaml"}, []string{"cmp"}, read)
	if err != nil {
		t.Fatal(err)
	}
	if fs, ops = ids(got); len(fs) != 1 || fs[0] != "calc/calc.go" || ops["cfg"] != 0 {
		t.Errorf("cmp only: files=%v ops=%v", fs, ops)
	}

	// The glob restricts the files.
	got, err = candidateMutants(tracked, []string{"config/*.yaml"}, []string{"cmp", "cfg"}, read)
	if err != nil {
		t.Fatal(err)
	}
	if fs, _ = ids(got); len(fs) != 1 || fs[0] != "config/limits.yaml" {
		t.Errorf("config glob: files=%v", fs)
	}
}

func TestCandidateMutantsErrors(t *testing.T) {
	read := func(string) ([]byte, error) { return []byte("package p\nfunc ("), nil }
	if _, err := candidateMutants([]string{"a.go"}, []string{"*.go"}, nil, read); err == nil || !strings.Contains(err.Error(), "operator") {
		t.Errorf("empty operators: err = %v, want an error naming the operators", err)
	}
	if _, err := candidateMutants([]string{"a.go"}, []string{"*.go"}, []string{"cmp"}, read); err == nil {
		t.Errorf("an unparsable matched Go file must be an error, not silently skipped")
	}
	failing := func(string) ([]byte, error) { return nil, errors.New("boom") }
	if _, err := candidateMutants([]string{"a.go"}, []string{"*.go"}, []string{"cmp"}, failing); err == nil {
		t.Errorf("a read failure must be an error")
	}
}

func TestLimitCandidates(t *testing.T) {
	ms := []Mutant{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	if got := limitCandidates(ms, 2); len(got) != 2 || got[1].ID != "b" {
		t.Errorf("limit 2 = %v", got)
	}
	for _, n := range []int{0, -1, 3, 10} {
		if got := limitCandidates(ms, n); len(got) != 3 {
			t.Errorf("limit %d kept %d, want all 3", n, len(got))
		}
	}
}

// ---- the pure parts of the per-mutant procedure ----

func TestClassifyRun(t *testing.T) {
	tests := []struct {
		name string
		exit int
		run  testRun
		want string
	}{
		{"clean pass", 0, testRun{}, "survived"},
		{"a failing test", 1, testRun{Failed: []string{"go:m/a:TestA"}}, "killed"},
		{"timeout beats a failing test", 1, testRun{Failed: []string{"go:m/a:TestA"}, TimedOut: true}, "timeout"},
		{"a package that does not build", 1, testRun{BuildFailed: []string{"m/a"}}, "build_error"},
		{"build failure beats a failing test", 1, testRun{BuildFailed: []string{"m/a"}, Failed: []string{"go:m/b:TestB"}}, "build_error"},
		{"a package failed with no failing test", 1, testRun{PackagesFailedWithoutTest: []string{"m/a"}}, "unconfirmed"},
		{"a nonzero exit that explains nothing", 2, testRun{}, "unconfirmed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyRun(tt.exit, tt.run); got != tt.want {
				t.Errorf("classifyRun(%d, %+v) = %q, want %q", tt.exit, tt.run, got, tt.want)
			}
		})
	}
}

func TestIntersectKillers(t *testing.T) {
	a := []string{"go:m/a:TestA", "go:m/b:TestB", "go:m/c:TestC"}
	b := []string{"go:m/b:TestB", "go:m/c:TestC", "go:m/d:TestD"}
	if got, want := intersectKillers(a, b), []string{"go:m/b:TestB", "go:m/c:TestC"}; !reflect.DeepEqual(got, want) {
		t.Errorf("intersection = %v, want %v", got, want)
	}
	if got := intersectKillers(a, nil); len(got) != 0 {
		t.Errorf("intersection with nothing = %v, want none", got)
	}
	if got := intersectKillers(a, []string{"go:m/a:TestB"}); len(got) != 0 {
		t.Errorf("the same test name in another package must not match: %v", got)
	}
}

func TestRerunPlan(t *testing.T) {
	pkgs, pattern := rerunPlan([]string{"go:m/calc:TestC", "go:m/app:TestB", "go:m/calc:TestA", "go:m/calc:TestA"})
	if want := []string{"m/app", "m/calc"}; !reflect.DeepEqual(pkgs, want) {
		t.Errorf("packages = %v, want %v", pkgs, want)
	}
	if want := "^(TestA|TestB|TestC)$"; pattern != want {
		t.Errorf("pattern = %q, want %q", pattern, want)
	}
}

// ---- fixtures ----

const fixtureModule = "example.com/mutfix"

var (
	pkgCalc     = fixtureModule + "/calc"
	pkgApp      = fixtureModule + "/app"
	pkgCfgcheck = fixtureModule + "/cfgcheck"
)

// calcSources are the functions of package calc. Each yields exactly the
// mutants named in its comment for the operators the tests select.
var calcSources = map[string]string{
	// cmp: == to !=, killed by calc and app tests.
	"fix": "package calc\n\n// Fix replaces a zero value by one.\nfunc Fix(p *int) {\n\tif *p == 0 {\n\t\t*p = 1\n\t}\n}\n",
	// cmp: > to >=, which changes nothing observable: survives.
	"settle": "package calc\n\n// Settle caps *p at ten.\nfunc Settle(p *int) {\n\tif *p > 10 {\n\t\t*p = 10\n\t}\n}\n",
	// ret: err to nil leaves err unused, so the package no longer compiles.
	"check": "package calc\n\nimport \"strconv\"\n\n// Check reports whether s is a decimal number.\nfunc Check(s string) error {\n\t_, err := strconv.Atoi(s)\n\treturn err\n}\n",
	// cmp: > to >= never ends for an unsigned n, so the test times out.
	"count": "package calc\n\nvar floor uint\n\n// Countdown counts the decrements needed to reach the floor.\nfunc Countdown(n uint) (c int) {\n\tfor n > floor {\n\t\tn--\n\t\tc++\n\t}\n\treturn\n}\n",
	// ret: true to false, killed by TestArmed once and then never again.
	"armed": "package calc\n\n// Armed reports whether the calculator is armed.\nfunc Armed() bool { return true }\n",
}

var calcTests = map[string]string{
	"fix": `func TestFix(t *testing.T) {
	zero, five := 0, 5
	Fix(&zero)
	Fix(&five)
	if zero != 1 || five != 5 {
		t.Fatalf("Fix: %d %d", zero, five)
	}
}`,
	"settle": `func TestSettle(t *testing.T) {
	for in, want := range map[int]int{11: 10, 10: 10, 3: 3} {
		p := in
		Settle(&p)
		if p != want {
			t.Fatalf("Settle(%d) = %d, want %d", in, p, want)
		}
	}
}`,
	"check": `func TestCheck(t *testing.T) {
	if Check("12") != nil || Check("x") == nil {
		t.Fatal("Check")
	}
}`,
	"count": `func TestCountdown(t *testing.T) {
	if got := Countdown(3); got != 3 {
		t.Fatalf("Countdown(3) = %d", got)
	}
}`,
	// TestArmed fails the first time the calculator is disarmed and passes
	// afterwards: it fails only while the marker file is absent, and creates it.
	"armed": `func TestArmed(t *testing.T) {
	if Armed() {
		return
	}
	marker := filepath.Join(os.TempDir(), "MARKER")
	if _, err := os.Stat(marker); err != nil {
		if werr := os.WriteFile(marker, []byte("x"), 0o644); werr != nil {
			t.Fatal(werr)
		}
		t.Fatal("disarmed: the first run fails")
	}
}`,
}

// calcFixture is a module whose package calc holds the named functions and
// their tests.
func calcFixture(marker string, names ...string) map[string]string {
	var test strings.Builder
	test.WriteString("package calc\n\nimport (\n")
	if slices.Contains(names, "armed") {
		test.WriteString("\t\"os\"\n\t\"path/filepath\"\n")
	}
	test.WriteString("\t\"testing\"\n)\n")
	files := map[string]string{"go.mod": "module " + fixtureModule + "\n\ngo 1.26\n"}
	for _, name := range names {
		files["calc/"+name+".go"] = calcSources[name]
		test.WriteString("\n" + strings.ReplaceAll(calcTests[name], "MARKER", marker) + "\n")
	}
	files["calc/calc_test.go"] = test.String()
	return files
}

// fullFixture adds package app, which imports calc, and package cfgcheck,
// whose test reads ../config/limits.yaml with no Go import of it.
func fullFixture(marker string, names ...string) map[string]string {
	files := calcFixture(marker, names...)
	files["app/app.go"] = "package app\n\nimport \"" + pkgCalc + "\"\n\n// Normalize returns x, or one when x is zero.\nfunc Normalize(x int) int {\n\tcalc.Fix(&x)\n\treturn x\n}\n"
	files["app/app_test.go"] = "package app\n\nimport \"testing\"\n\nfunc TestNormalize(t *testing.T) {\n\tif Normalize(0) != 1 || Normalize(4) != 4 {\n\t\tt.Fatal(\"Normalize\")\n\t}\n}\n"
	files["cfgcheck/cfgcheck.go"] = "package cfgcheck\n"
	files["cfgcheck/cfgcheck_test.go"] = `package cfgcheck

import (
	"os"
	"strings"
	"testing"
)

func TestLimits(t *testing.T) {
	data, err := os.ReadFile("../config/limits.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "strict: true\n") {
		t.Fatalf("limits.yaml = %q", data)
	}
}
`
	files["config/limits.yaml"] = "strict: true\n"
	return files
}

// sharedMarker names, inside os.TempDir, the marker file that TestArmed uses.
var sharedMarker = fmt.Sprintf("mutfix-armed-%d", os.Getpid())

// sharedFiles is the one fixture module every test of this package mutates.
func sharedFiles() map[string]string {
	return fullFixture(sharedMarker, "fix", "settle", "check", "count", "armed")
}

var shared struct {
	once sync.Once
	dir  string
	err  error
}

// sharedRepo returns a repository holding sharedFiles, created once for the
// whole test binary. Run never writes into its RepoDir, so tests can share it.
func sharedRepo(t *testing.T) string {
	t.Helper()
	shared.once.Do(func() {
		dir, err := os.MkdirTemp("", "mutfix-repo-*")
		if err != nil {
			shared.err = err
			return
		}
		shared.dir = dir
		shared.err = initRepo(dir, sharedFiles())
	})
	if shared.err != nil {
		t.Fatalf("shared fixture: %v", shared.err)
	}
	return shared.dir
}

// armedMarker removes the marker file TestArmed uses now and when the test
// ends, and returns its path.
func armedMarker(t *testing.T) string {
	t.Helper()
	path := filepath.Join(os.TempDir(), sharedMarker)
	remove := func() {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("remove the marker: %v", err)
		}
	}
	remove()
	t.Cleanup(remove)
	return path
}

func gitRun(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, errb.String())
	}
	return strings.TrimSpace(out.String()), nil
}

// initRepo commits files into a fresh repository at dir.
func initRepo(dir string, files map[string]string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if _, err := gitRun(dir, "init", "-q", "-b", "main"); err != nil {
		return err
	}
	for rel, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			return err
		}
	}
	if _, err := gitRun(dir, "-c", "core.autocrlf=false", "add", "-A"); err != nil {
		return err
	}
	_, err := gitRun(dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "fixture")
	return err
}

// newRepo commits files into a fresh repository and returns its directory.
func newRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	if err := initRepo(dir, files); err != nil {
		t.Fatal(err)
	}
	return dir
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitRun(dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// hashTreeErr returns the sha256 of every file under dir outside .git.
func hashTreeErr(dir string) (map[string]string, error) {
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(dir, p)
		if relErr != nil {
			return relErr
		}
		if d.IsDir() {
			if rel == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		out[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:])
		return nil
	})
	return out, err
}

func hashTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out, err := hashTreeErr(dir)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// runGuard is the Log of a run. At every "applied" line, which the driver logs
// after a mutant is written and before it is built, it checks that the
// repository still holds its original bytes.
type runGuard struct {
	mu        sync.Mutex
	repo      string
	before    map[string]string
	applied   int
	problems  []string
	lines     []string
	onApplied func()
	onLine    func(line string)
}

func newRunGuard(t *testing.T, repo string) *runGuard {
	t.Helper()
	return &runGuard{repo: repo, before: hashTree(t, repo)}
}

func (g *runGuard) Write(p []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	line := strings.TrimRight(string(p), "\n")
	g.lines = append(g.lines, line)
	if g.onLine != nil {
		g.onLine(line)
	}
	if !strings.Contains(line, " applied") {
		return len(p), nil
	}
	g.applied++
	if now, err := hashTreeErr(g.repo); err != nil {
		g.problems = append(g.problems, "hash the repository: "+err.Error())
	} else if !reflect.DeepEqual(now, g.before) {
		g.problems = append(g.problems, "the repository changed while a mutant was applied: "+line)
	}
	if g.onApplied != nil {
		g.onApplied()
	}
	return len(p), nil
}

// slotRoot returns the temporary root the run reported.
func (g *runGuard) slotRoot(t *testing.T) string {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, line := range g.lines {
		if root, ok := strings.CutPrefix(line, "slot root: "); ok {
			return root
		}
	}
	t.Fatalf("the run never reported its slot root; log: %q", g.lines)
	return ""
}

func (g *runGuard) logText() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return strings.Join(g.lines, "\n")
}

// finish asserts what must hold after every run, successful or not: the
// repository is byte for byte what it was, no mutant was ever applied to it,
// and the temporary root is gone.
func (g *runGuard) finish(t *testing.T) {
	t.Helper()
	g.mu.Lock()
	problems := slices.Clone(g.problems)
	g.mu.Unlock()
	for _, p := range problems {
		t.Error(p)
	}
	if after := hashTree(t, g.repo); !reflect.DeepEqual(g.before, after) {
		t.Errorf("the repository changed: before %v after %v", g.before, after)
	}
	if root := g.slotRoot(t); root != "" {
		if _, err := os.Stat(root); err == nil {
			t.Errorf("the temporary root %s was left behind", root)
		}
	}
}

func fixtureOptions(repo, outDir string, files, ops []string) Options {
	return Options{
		RepoDir: repo, Base: "HEAD", Files: files, Operators: ops,
		Timeout: 300 * time.Second, Concurrency: 1, Packages: []string{"./..."}, MaxHops: 3,
		Out: filepath.Join(outDir, "mutants.jsonl"), DBPath: filepath.Join(outDir, "hazard.db"),
	}
}

func requireNoOutputs(t *testing.T, opts Options) {
	t.Helper()
	for _, p := range []string{opts.Out, opts.Out + ".partial", opts.Out + ".baseline.json", opts.Out + ".baseline.json.tmp", opts.DBPath} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s exists after a refused run", p)
		}
	}
}

type storedMutants struct {
	status map[string]string   // mutant id -> status
	kills  map[string][]string // mutant id -> killer test ids
}

func readStored(t *testing.T, dbPath string) storedMutants {
	t.Helper()
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	}()
	got := storedMutants{status: map[string]string{}, kills: map[string][]string{}}
	rows, err := st.DB().Query(`SELECT id, status FROM mutants`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, status string
		if err := rows.Scan(&id, &status); err != nil {
			t.Fatal(err)
		}
		got.status[id] = status
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	rows, err = st.DB().Query(`SELECT mutant_id, test_id FROM mutant_kills ORDER BY mutant_id, test_id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, test string
		if err := rows.Scan(&id, &test); err != nil {
			t.Fatal(err)
		}
		got.kills[id] = append(got.kills[id], test)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	return got
}

// requirePublished asserts that the JSONL file, the baseline durations file and
// the database hold exactly what the summary reports: every record, the
// per-test baseline, and the stored statuses and kills of all but the
// unconfirmed mutants.
func requirePublished(t *testing.T, opts Options, sum Summary) {
	t.Helper()
	if got := readJSONL(t, opts.Out); !reflect.DeepEqual(got, sum.Records) {
		t.Errorf("JSONL records differ from the summary:\n got %+v\nwant %+v", got, sum.Records)
	}
	if _, err := os.Stat(opts.Out + ".partial"); err == nil {
		t.Errorf("the partial file survives a completed run")
	}
	requireBaselineFile(t, opts, sum)
	stored := readStored(t, opts.DBPath)
	wantStatus, wantKills := map[string]string{}, map[string][]string{}
	for _, r := range sum.Records {
		if r.Status == "unconfirmed" {
			continue
		}
		wantStatus[r.ID] = r.Status
		if r.Status == "killed" {
			wantKills[r.ID] = r.Killers
		}
	}
	if !reflect.DeepEqual(stored.status, wantStatus) || !reflect.DeepEqual(stored.kills, wantKills) {
		t.Errorf("database differs from the summary:\n status %v want %v\n kills %v want %v", stored.status, wantStatus, stored.kills, wantKills)
	}
}

// requireBaselineFile asserts that <Out>.baseline.json holds the base SHA and
// exactly the summary's per-test baseline, which must not be empty: every
// fixture suite has tests.
func requireBaselineFile(t *testing.T, opts Options, sum Summary) {
	t.Helper()
	raw, err := os.ReadFile(opts.Out + ".baseline.json")
	if err != nil {
		t.Errorf("the baseline durations file: %v", err)
		return
	}
	var got struct {
		BaseSHA string           `json:"base_sha"`
		Tests   map[string]int64 `json:"tests"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Errorf("the baseline durations file is not JSON: %q: %v", raw, err)
		return
	}
	if got.BaseSHA != sum.BaseSHA || len(sum.BaselineMS) == 0 || !reflect.DeepEqual(got.Tests, sum.BaselineMS) {
		t.Errorf("baseline file = base %q tests %v; want base %q tests %v (non-empty)", got.BaseSHA, got.Tests, sum.BaseSHA, sum.BaselineMS)
	}
	if _, err := os.Stat(opts.Out + ".baseline.json.tmp"); err == nil {
		t.Errorf("the temporary baseline file survives a completed run")
	}
}

// ---- writing and restoring a mutant ----

func TestWriteMutantAppliesTheMutantToTheBaseBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.go")
	base := []byte("package a\n\nvar x = 1\n")
	if err := os.WriteFile(path, base, 0o644); err != nil {
		t.Fatal(err)
	}
	m := Mutant{ID: "m1", File: "a.go", Original: "1", Mutated: "2", Offset: bytes.IndexByte(base, '1'), Length: 1}
	if err := writeMutant(path, base, m); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "package a\n\nvar x = 2\n" {
		t.Errorf("file = %q, want the mutated bytes", got)
	}
}

func TestWriteMutantRefusesAFileThatIsNotTheBase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.go")
	base := []byte("package a\n\nvar x = 1\n")
	dirty := []byte("package a\n\nvar x = 3\n")
	if err := os.WriteFile(path, dirty, 0o644); err != nil {
		t.Fatal(err)
	}
	m := Mutant{ID: "m1", File: "a.go", Original: "1", Mutated: "2", Offset: bytes.IndexByte(base, '1'), Length: 1}
	err := writeMutant(path, base, m)
	if err == nil || !strings.Contains(err.Error(), "base") {
		t.Fatalf("writeMutant on a dirty file: err = %v, want an error naming the base", err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, dirty) {
		t.Errorf("a refused write changed the file to %q", got)
	}
}

func TestRestoreFileWritesTheBaseAndVerifiesIt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	base := []byte("package a\n\nvar x = 1\n")
	if err := os.WriteFile(path, []byte("package a\n\nvar x = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := restoreFile(path, base); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, base) {
		t.Errorf("file = %q, want the base bytes", got)
	}
	// A path that cannot hold the file is an error, never a silent success.
	blocked := filepath.Join(dir, "blocked.go")
	if err := os.Mkdir(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := restoreFile(blocked, base); err == nil {
		t.Errorf("restoring onto a directory must fail")
	}
}

// ---- baseline durations ----

func TestBaselineElapsedReadsTheTopLevelPassEvents(t *testing.T) {
	stdout := jsonLines(
		`go: downloading something`,
		`{"Action":"start","Package":"m/calc"}`,
		`{"Action":"run","Package":"m/calc","Test":"TestAdd"}`,
		`{"Action":"output","Package":"m/calc","Test":"TestAdd","Output":"=== RUN   TestAdd\n"}`,
		`{"Action":"pass","Package":"m/calc","Test":"TestAdd/neg","Elapsed":9}`,
		`{"Action":"pass","Package":"m/calc","Test":"TestAdd","Elapsed":0.25}`,
		`{"Action":"pass","Package":"m/calc","Test":"TestFast","Elapsed":0}`,
		`{"Action":"skip","Package":"m/calc","Test":"TestSkipped","Elapsed":0.5}`,
		`{"Action":"pass","Package":"m/calc","Elapsed":3}`,
		`{"Action":"pass","Package":"m/app","Test":"TestAdd","Elapsed":1.5}`,
	)
	got, err := baselineElapsed([]byte(stdout))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{
		"go:m/calc:TestAdd":  0.25,
		"go:m/calc:TestFast": 0, // go test reports 0.00 for a fast test: a measurement, not a gap
		"go:m/app:TestAdd":   1.5,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("elapsed = %v, want %v", got, want)
	}
}

func TestBaselineElapsedNeverInventsADuration(t *testing.T) {
	tests := []struct {
		name, stdout, wantInErr string
	}{
		{"a pass event without an elapsed field", jsonLines(
			`{"Action":"pass","Package":"m/calc","Test":"TestAdd","Elapsed":0.1}`,
			`{"Action":"pass","Package":"m/calc","Test":"TestSub"}`,
		), "go:m/calc:TestSub"},
		{"a negative elapsed time", jsonLines(
			`{"Action":"pass","Package":"m/calc","Test":"TestAdd","Elapsed":-0.1}`,
		), "go:m/calc:TestAdd"},
		{"an event that is not JSON", jsonLines(
			`{"Action":"pass","Package":"m/calc","Test":"TestAdd","Elap`,
		), "malformed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := baselineElapsed([]byte(tt.stdout))
			if err == nil || !strings.Contains(err.Error(), tt.wantInErr) {
				t.Fatalf("baselineElapsed = %v, %v; want an error containing %q", got, err, tt.wantInErr)
			}
		})
	}
	// A subtest has no key of its own, so a subtest without a time is no gap.
	if _, err := baselineElapsed([]byte(jsonLines(`{"Action":"pass","Package":"m/calc","Test":"TestAdd/neg"}`))); err != nil {
		t.Errorf("a subtest without an elapsed field: %v, want none", err)
	}
}

func TestMedianMSIsTheMeanOfTheTwoRunsRoundedHalfUp(t *testing.T) {
	tests := []struct {
		name          string
		first, second float64
		want          int64
	}{
		{"two equal runs", 0.1, 0.1, 100},
		{"an even sum of milliseconds", 0.010, 0.020, 15},
		{"an odd sum rounds up", 0.010, 0.013, 12},
		{"1.5 rounds to 2", 0.001, 0.002, 2},
		{"2.5 rounds to 3, not to the even 2", 0.002, 0.003, 3},
		{"half a millisecond rounds to 1", 0.0005, 0.0005, 1},
		{"a quarter of a millisecond rounds to 0", 0.0005, 0, 0},
		{"zero is a measurement", 0, 0, 0},
		{"a long test", 12.5, 12.75, 12625},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := medianMS(map[string]float64{"go:m/a:TestA": tt.first}, map[string]float64{"go:m/a:TestA": tt.second})
			if err != nil {
				t.Fatal(err)
			}
			if want := map[string]int64{"go:m/a:TestA": tt.want}; !reflect.DeepEqual(got, want) {
				t.Errorf("medianMS(%v, %v) = %v, want %v", tt.first, tt.second, got, want)
			}
		})
	}
}

// Elapsed times arrive as decimal text, so a mean of exactly k+0.5 ms must round
// to k+1 whatever the binary form of the two inputs.
func TestMedianMSRoundsEveryHalfMillisecondUp(t *testing.T) {
	seconds := func(micros int) float64 {
		v, err := strconv.ParseFloat(fmt.Sprintf("%d.%06d", micros/1_000_000, micros%1_000_000), 64)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for k := 0; k < 20000; k++ {
		same := seconds(k*1000 + 500)
		pair := [2]float64{seconds(k * 1000), seconds((k + 1) * 1000)}
		for _, runs := range [][2]float64{{same, same}, pair} {
			got, err := medianMS(map[string]float64{"go:m/a:TestA": runs[0]}, map[string]float64{"go:m/a:TestA": runs[1]})
			if err != nil {
				t.Fatal(err)
			}
			if got["go:m/a:TestA"] != int64(k+1) {
				t.Fatalf("medianMS(%v, %v) = %d, want %d", runs[0], runs[1], got["go:m/a:TestA"], k+1)
			}
		}
	}
}

// The baseline must be the same suite twice: a test seen in one run only would
// have a duration that is not a median of anything.
func TestMedianMSRefusesTestsOfOnlyOneRun(t *testing.T) {
	both := map[string]float64{"go:m/a:TestA": 0.1}
	extra := map[string]float64{"go:m/a:TestA": 0.1, "go:m/b:TestB": 0.2, "go:m/a:TestAA": 0.3}
	for name, runs := range map[string][2]map[string]float64{
		"only in the first run":  {extra, both},
		"only in the second run": {both, extra},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := medianMS(runs[0], runs[1])
			// Several tests differ: the error names the first by ID, so it is stable.
			if err == nil || !strings.Contains(err.Error(), "baseline runs disagree on the test set: go:m/a:TestAA") {
				t.Fatalf("medianMS = %v, %v; want the disagreement on go:m/a:TestAA", got, err)
			}
			if got != nil {
				t.Errorf("medianMS returned %v with its error", got)
			}
		})
	}
}

func TestMedianMSOfTwoEmptyRunsIsAnEmptyMap(t *testing.T) {
	got, err := medianMS(map[string]float64{}, map[string]float64{})
	if err != nil || got == nil || len(got) != 0 {
		t.Errorf("medianMS of nothing = %#v, %v; want an empty, non-nil map", got, err)
	}
}

// ---- a stand-in for the go command ----

// fakeCall is one go invocation the driver made.
type fakeCall struct {
	Dir  string
	Args []string
	// Kind is baseline (nothing applied, -count=1), build, test or rerun (-run).
	Kind string
	// Mutated is the repository-relative file that differs from the base in
	// Dir when the call starts, or "".
	Mutated string
	// Deadline is the time left on the context deadline, or 0 without one.
	Deadline time.Duration
}

// fakeGo answers the driver's go invocations from a script, deciding by the
// slot's contents which mutant is applied. It is safe for concurrent use.
type fakeGo struct {
	t      *testing.T
	base   map[string]string // repository-relative path -> original contents
	script func(fakeCall) (cmdResult, error)
	mu     sync.Mutex
	calls  []fakeCall
}

func (f *fakeGo) run(ctx context.Context, dir string, args ...string) (cmdResult, error) {
	if err := ctx.Err(); err != nil {
		return cmdResult{}, err
	}
	c := fakeCall{Dir: dir, Args: slices.Clone(args)}
	if dl, ok := ctx.Deadline(); ok {
		c.Deadline = time.Until(dl)
	}
	var changed []string
	for rel, want := range f.base {
		got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil || string(got) != want {
			changed = append(changed, rel)
		}
	}
	slices.Sort(changed)
	switch len(changed) {
	case 0:
	case 1:
		c.Mutated = changed[0]
	default:
		f.t.Errorf("slot %s holds several mutants at once: %v", dir, changed)
	}
	switch {
	case args[0] == "build":
		c.Kind = "build"
	case slices.Contains(args, "-run"):
		c.Kind = "rerun"
	case c.Mutated == "":
		c.Kind = "baseline"
	default:
		c.Kind = "test"
	}
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()
	if c.Kind == "baseline" {
		time.Sleep(2 * time.Millisecond) // a baseline always takes measurable time
	}
	return f.script(c)
}

func (f *fakeGo) callsFor(mutated string) []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeCall
	for _, c := range f.calls {
		if c.Mutated == mutated {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeGo) all() []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeGo) tools() tools {
	return tools{goCmd: f.run, loadGraph: func(context.Context, string, []string, []string) (*graph.Graph, error) {
		return fixtureGraph(), nil
	}}
}

// fixtureGraph is the package graph of the shared fixture module.
func fixtureGraph() *graph.Graph {
	return &graph.Graph{Packages: map[string]*graph.Package{
		pkgCalc:     {ID: pkgCalc, Language: "go", Dir: "calc", Files: []string{"calc/fix.go", "calc/settle.go", "calc/check.go", "calc/count.go", "calc/armed.go"}, TestFiles: []string{"calc/calc_test.go"}},
		pkgApp:      {ID: pkgApp, Language: "go", Dir: "app", Files: []string{"app/app.go"}, TestFiles: []string{"app/app_test.go"}, Imports: []string{pkgCalc}},
		pkgCfgcheck: {ID: pkgCfgcheck, Language: "go", Dir: "cfgcheck", Files: []string{"cfgcheck/cfgcheck.go"}, TestFiles: []string{"cfgcheck/cfgcheck_test.go"}},
	}}
}

func passRun() cmdResult {
	return cmdResult{Stdout: []byte(jsonLines(
		`{"Action":"pass","Package":"`+pkgCalc+`","Test":"TestFix","Elapsed":0.1}`,
		`{"Action":"pass","Package":"`+pkgCalc+`","Elapsed":0.2}`,
	))}
}

// failRun reports the named top-level tests of each package as failed.
func failRun(fails map[string][]string) cmdResult {
	var ls []string
	for _, pkg := range slices.Sorted(maps.Keys(fails)) {
		for _, name := range fails[pkg] {
			ls = append(ls, fmt.Sprintf(`{"Action":"fail","Package":%q,"Test":%q,"Elapsed":0.1}`, pkg, name))
		}
		ls = append(ls, fmt.Sprintf(`{"Action":"fail","Package":%q,"Elapsed":0.2}`, pkg))
	}
	return cmdResult{Stdout: []byte(jsonLines(ls...)), Exit: 1}
}

// panicRun reports the panic the test binary prints when -timeout expires.
func panicRun(pkg, test string) cmdResult {
	return cmdResult{Exit: 1, Stdout: []byte(jsonLines(
		fmt.Sprintf(`{"Action":"output","Package":%q,"Test":%q,"Output":"panic: test timed out after 20s\n"}`, pkg, test),
		fmt.Sprintf(`{"Action":"fail","Package":%q,"Test":%q,"Elapsed":20}`, pkg, test),
		fmt.Sprintf(`{"Action":"fail","Package":%q,"Elapsed":20}`, pkg),
	))}
}

// fixtureScript plays the suite of the shared fixture against each mutant of
// the six selected below.
func fixtureScript(c fakeCall) (cmdResult, error) {
	if c.Kind == "baseline" {
		return passRun(), nil
	}
	if c.Kind == "build" && c.Mutated != "calc/check.go" {
		return cmdResult{}, nil
	}
	switch c.Mutated {
	case "calc/fix.go": // killed by calc and, through the import, by app
		return failRun(map[string][]string{pkgApp: {"TestNormalize"}, pkgCalc: {"TestFix"}}), nil
	case "calc/settle.go": // equivalent: nothing fails
		return passRun(), nil
	case "calc/check.go": // does not compile
		return cmdResult{Exit: 1, Stderr: []byte("calc/check.go:8:2: declared and not used: err")}, nil
	case "calc/count.go": // never ends
		return panicRun(pkgCalc, "TestCountdown"), nil
	case "calc/armed.go": // fails once, then passes
		if c.Kind == "rerun" {
			return passRun(), nil
		}
		return failRun(map[string][]string{pkgCalc: {"TestArmed"}}), nil
	case "config/limits.yaml": // app's failure does not come back, cfgcheck's does
		if c.Kind == "rerun" {
			return failRun(map[string][]string{pkgCfgcheck: {"TestLimits"}}), nil
		}
		return failRun(map[string][]string{pkgApp: {"TestNormalize"}, pkgCfgcheck: {"TestLimits"}}), nil
	}
	return cmdResult{}, fmt.Errorf("unscripted call %+v", c)
}

func newFakeGo(t *testing.T, script func(fakeCall) (cmdResult, error)) *fakeGo {
	t.Helper()
	return &fakeGo{t: t, base: sharedFiles(), script: script}
}

// ---- the procedure around the go command, without launching it ----

// fixtureWant is what a run must report for one mutant of the shared fixture.
type fixtureWant struct {
	operator, status             string
	killers, unreachable, beyond []string
}

// fixtureWants lists the six mutants of calc/*.go and config/*.yaml under the
// cmp, ret and cfg operators and the outcome of each, whether the go command is
// scripted or real: a killed mutant that a dependent package also sees, a
// configuration mutant that only a package with no import path to it kills, a
// survivor, a build failure, a timeout and a killer that fails only once.
// Packages are one hop from calc, so the run must use MaxHops 0.
func fixtureWants() map[string]fixtureWant {
	tid := func(pkg, name string) string { return "go:" + pkg + ":" + name }
	return map[string]fixtureWant{
		"calc/fix.go":        {"cmp", "killed", []string{tid(pkgApp, "TestNormalize"), tid(pkgCalc, "TestFix")}, nil, []string{tid(pkgApp, "TestNormalize")}},
		"calc/settle.go":     {"cmp", "survived", nil, nil, nil},
		"calc/check.go":      {"ret", "build_error", nil, nil, nil},
		"calc/count.go":      {"cmp", "timeout", nil, nil, nil},
		"calc/armed.go":      {"ret", "unconfirmed", nil, nil, nil},
		"config/limits.yaml": {"cfg", "killed", []string{tid(pkgCfgcheck, "TestLimits")}, []string{tid(pkgCfgcheck, "TestLimits")}, nil},
	}
}

// requireOutcome asserts that the summary reports exactly the wanted mutants:
// one record each, and the counts those records imply.
func requireOutcome(t *testing.T, sum Summary, sha string, wants map[string]fixtureWant) {
	t.Helper()
	byStatus, byOperator := map[string]int{}, map[string]int{}
	var unreachable, fully, beyond int
	for _, w := range wants {
		byStatus[w.status]++
		byOperator[w.operator]++
		if len(w.unreachable) > 0 {
			unreachable++
			if len(w.unreachable) == len(w.killers) {
				fully++
			}
		}
		if len(w.beyond) > 0 {
			beyond++
		}
	}
	n := len(wants)
	if sum.BaseSHA != sha || sum.Candidates != n || sum.Executed != n || sum.BaselineSeconds <= 0 {
		t.Errorf("header = base %q candidates %d executed %d baseline %.4fs; want %q %d %d >0", sum.BaseSHA, sum.Candidates, sum.Executed, sum.BaselineSeconds, sha, n, n)
	}
	if !reflect.DeepEqual(sum.ByStatus, byStatus) {
		t.Errorf("ByStatus = %v, want %v", sum.ByStatus, byStatus)
	}
	if !reflect.DeepEqual(sum.ByOperator, byOperator) {
		t.Errorf("ByOperator = %v, want %v", sum.ByOperator, byOperator)
	}
	if sum.Usable != byStatus["killed"] || sum.UnreachableMutants != unreachable || sum.FullyUnreachableMutants != fully || sum.BeyondMaxHopsMutants != beyond {
		t.Errorf("usable %d unreachable %d fully %d beyond %d; want %d %d %d %d", sum.Usable, sum.UnreachableMutants, sum.FullyUnreachableMutants, sum.BeyondMaxHopsMutants, byStatus["killed"], unreachable, fully, beyond)
	}
	if len(sum.Records) != n {
		t.Fatalf("records = %d, want %d: %+v", len(sum.Records), n, sum.Records)
	}
	for _, r := range sum.Records {
		w, ok := wants[r.File]
		if !ok {
			t.Errorf("unexpected record for %s: %+v", r.File, r)
			continue
		}
		if r.Status != w.status || r.Operator != w.operator || r.BaseSHA != sha {
			t.Errorf("%s: status %q operator %q base %q, want %q %q %q", r.File, r.Status, r.Operator, r.BaseSHA, w.status, w.operator, sha)
		}
		for name, pair := range map[string][2][]string{
			"killers":     {r.Killers, w.killers},
			"unreachable": {r.UnreachableKillers, w.unreachable},
			"beyond":      {r.BeyondMaxHopsKillers, w.beyond},
		} {
			if len(pair[0]) != len(pair[1]) || (len(pair[1]) > 0 && !reflect.DeepEqual(pair[0], pair[1])) {
				t.Errorf("%s: %s = %v, want %v", r.File, name, pair[0], pair[1])
			}
		}
		if r.ID == "" || r.Original == r.Mutated || r.Line < 1 {
			t.Errorf("%s: incomplete record %+v", r.File, r)
		}
	}
}

func TestRunProcedureWithScriptedGo(t *testing.T) {
	repo := sharedRepo(t)
	sha := gitIn(t, repo, "rev-parse", "HEAD")
	opts := fixtureOptions(repo, t.TempDir(), []string{"calc/*.go", "config/*.yaml"}, []string{"cmp", "ret", "cfg"})
	opts.Concurrency = 3
	opts.MaxHops = 0 // app imports calc one hop away: beyond a bound of zero
	opts.Packages = []string{"./calc", "./app", "./cfgcheck"}
	opts.Tags = []string{"slow", "x"}
	opts.Timeout = 5 * time.Minute
	guard := newRunGuard(t, repo)
	opts.Log = guard
	fake := newFakeGo(t, fixtureScript)
	sum, err := run(context.Background(), opts, fake.tools())
	if err != nil {
		t.Fatalf("run: %v\nlog:\n%s", err, guard.logText())
	}
	guard.finish(t)
	if guard.applied != 6 {
		t.Errorf("applied %d mutants, want 6", guard.applied)
	}
	if log := guard.logText(); !strings.Contains(log, "executing 6 of 6 mutants on 3 slots") {
		t.Errorf("slot count not honored; log:\n%s", log)
	}

	requireOutcome(t, sum, sha, fixtureWants())
	requirePublished(t, opts, sum)
	// Both scripted baseline runs report TestFix at 0.1s: 100ms.
	if want := map[string]int64{"go:" + pkgCalc + ":TestFix": 100}; !reflect.DeepEqual(sum.BaselineMS, want) {
		t.Errorf("BaselineMS = %v, want %v", sum.BaselineMS, want)
	}

	// The exact go command lines.
	pkgs := []string{"./calc", "./app", "./cfgcheck"}
	cat := func(parts ...[]string) []string { return slices.Concat(parts...) }
	root := guard.slotRoot(t)
	baseline := fake.callsFor("")
	if len(baseline) != 2 {
		t.Fatalf("baseline ran %d times, want twice: %+v", len(baseline), baseline)
	}
	for _, c := range baseline {
		if want := cat([]string{"test", "-json", "-tags", "slow,x", "-count=1"}, pkgs); !reflect.DeepEqual(c.Args, want) || c.Dir != filepath.Join(root, "slot0") {
			t.Errorf("baseline call = %+v, want %v in slot0", c, want)
		}
		if c.Deadline != 0 {
			t.Errorf("the baseline has a deadline of %v, want none", c.Deadline)
		}
	}
	build := cat([]string{"build", "-tags", "slow,x"}, pkgs)
	test := cat([]string{"test", "-json", "-tags", "slow,x", "-timeout", "5m0s"}, pkgs)
	kinds := func(file string) (out []string) {
		for _, c := range fake.callsFor(file) {
			out = append(out, c.Kind)
		}
		return out
	}
	for file, wantKinds := range map[string][]string{
		"calc/fix.go":        {"build", "test", "rerun"},
		"calc/settle.go":     {"build", "test"},
		"calc/check.go":      {"build"},
		"calc/count.go":      {"build", "test"},
		"calc/armed.go":      {"build", "test", "rerun"},
		"config/limits.yaml": {"build", "test", "rerun"},
	} {
		if got := kinds(file); !reflect.DeepEqual(got, wantKinds) {
			t.Errorf("%s: go calls %v, want %v", file, got, wantKinds)
		}
	}
	for _, c := range fake.all() {
		if c.Kind == "baseline" {
			continue
		}
		if !strings.HasPrefix(c.Dir, root) || c.Dir == repo {
			t.Errorf("a go command ran outside the slots: %+v", c)
		}
		switch c.Kind {
		case "build":
			if !reflect.DeepEqual(c.Args, build) || c.Deadline != 0 {
				t.Errorf("build call = %+v, want %v", c, build)
			}
		case "test":
			if !reflect.DeepEqual(c.Args, test) {
				t.Errorf("test call = %+v, want %v", c, test)
			}
		}
		if c.Kind == "test" || c.Kind == "rerun" {
			if c.Deadline <= 5*time.Minute || c.Deadline > 5*time.Minute+30*time.Second {
				t.Errorf("%s call deadline %v, want within (5m, 5m30s]: %+v", c.Kind, c.Deadline, c)
			}
		}
	}
	rerun := func(file string) []string { return fake.callsFor(file)[2].Args }
	if want := []string{"test", "-json", "-tags", "slow,x", "-count=1", "-timeout", "5m0s", "-run", "^(TestFix|TestNormalize)$", pkgApp, pkgCalc}; !reflect.DeepEqual(rerun("calc/fix.go"), want) {
		t.Errorf("rerun of calc/fix.go = %v, want %v", rerun("calc/fix.go"), want)
	}
	if want := []string{"test", "-json", "-tags", "slow,x", "-count=1", "-timeout", "5m0s", "-run", "^(TestLimits|TestNormalize)$", pkgApp, pkgCfgcheck}; !reflect.DeepEqual(rerun("config/limits.yaml"), want) {
		t.Errorf("rerun of config/limits.yaml = %v, want %v", rerun("config/limits.yaml"), want)
	}
}

// A mutant that fails to restore leaves a slot that is no longer the base. The
// run stops with an error instead of testing the next mutant against it.
func TestRunStopsWhenAMutantCannotBeRestored(t *testing.T) {
	repo := sharedRepo(t)
	opts := fixtureOptions(repo, t.TempDir(), []string{"calc/fix.go", "calc/settle.go"}, []string{"cmp"})
	guard := newRunGuard(t, repo)
	opts.Log = guard
	fake := newFakeGo(t, func(c fakeCall) (cmdResult, error) {
		if c.Kind == "test" {
			// Replace the mutated file by a directory: it can no longer be written.
			path := filepath.Join(c.Dir, filepath.FromSlash(c.Mutated))
			if err := os.Remove(path); err != nil {
				return cmdResult{}, err
			}
			if err := os.Mkdir(path, 0o755); err != nil {
				return cmdResult{}, err
			}
		}
		return passRun(), nil
	})
	_, err := run(context.Background(), opts, fake.tools())
	if err == nil || !strings.Contains(err.Error(), "restore") {
		t.Fatalf("run error = %v, want a restore failure", err)
	}
	guard.finish(t)
	if guard.applied != 1 {
		t.Errorf("applied %d mutants, want the run to stop after the first", guard.applied)
	}
	if _, err := os.Stat(opts.Out); err == nil {
		t.Errorf("the final output exists after an aborted run")
	}
	if _, err := os.Stat(opts.Out + ".baseline.json"); err == nil {
		t.Errorf("the baseline durations file exists after an aborted run")
	}
	if _, err := os.Stat(opts.DBPath); err == nil {
		t.Errorf("the database exists after an aborted run")
	}
}

// The deadline the driver sets (Timeout plus a grace period) ending a go test
// is a timeout of the mutant, not a failure of the run.
func TestRunTreatsAnExpiredDeadlineAsATimeout(t *testing.T) {
	repo := sharedRepo(t)
	opts := fixtureOptions(repo, t.TempDir(), []string{"calc/count.go"}, []string{"cmp"})
	opts.Timeout = 20 * time.Second
	guard := newRunGuard(t, repo)
	opts.Log = guard
	fake := newFakeGo(t, func(c fakeCall) (cmdResult, error) {
		if c.Kind == "test" {
			return cmdResult{}, context.DeadlineExceeded
		}
		return passRun(), nil
	})
	sum, err := run(context.Background(), opts, fake.tools())
	if err != nil {
		t.Fatalf("run: %v\nlog:\n%s", err, guard.logText())
	}
	guard.finish(t)
	if len(sum.Records) != 1 || sum.Records[0].Status != "timeout" || len(sum.Records[0].Killers) != 0 {
		t.Fatalf("records = %+v, want one timeout record with no killers", sum.Records)
	}
	if stored := readStored(t, opts.DBPath); len(stored.status) != 1 || stored.status[sum.Records[0].ID] != "timeout" {
		t.Errorf("database holds %+v, want the timeout row", stored)
	}
}

// A baseline that is green once and red the next time is not green: the second
// run is what exposes a flaky suite before any mutant is blamed on it.
func TestRunRefusesBaselineThatTurnsRed(t *testing.T) {
	repo := sharedRepo(t)
	opts := fixtureOptions(repo, t.TempDir(), []string{"calc/fix.go"}, []string{"cmp"})
	guard := newRunGuard(t, repo)
	opts.Log = guard
	baselines := 0
	fake := newFakeGo(t, func(c fakeCall) (cmdResult, error) {
		if c.Kind == "baseline" {
			baselines++
			if baselines == 2 {
				return failRun(map[string][]string{pkgCalc: {"TestFix"}}), nil
			}
		}
		return passRun(), nil
	})
	_, err := run(context.Background(), opts, fake.tools())
	if err == nil || !strings.Contains(err.Error(), "baseline is not green") || !strings.Contains(err.Error(), "run 2") {
		t.Fatalf("run error = %v, want %q from run 2", err, "baseline is not green")
	}
	if guard.applied != 0 || len(fake.all()) != 2 {
		t.Errorf("a mutant ran against a baseline that turned red: applied %d, go calls %d", guard.applied, len(fake.all()))
	}
	requireNoOutputs(t, opts)
	guard.finish(t)
}

// Each way the baseline can be red refuses the run before a mutant is applied.
func TestRunRefusesEveryKindOfRedBaseline(t *testing.T) {
	cases := map[string]cmdResult{
		"a failing test":         failRun(map[string][]string{pkgCalc: {"TestFix"}}),
		"a nonzero exit":         {Exit: 2, Stderr: []byte("boom")},
		"a package that panics":  {Exit: 1, Stdout: []byte(jsonLines(`{"Action":"fail","Package":"` + pkgCalc + `","Elapsed":1}`))},
		"a test timeout":         panicRun(pkgCalc, "TestFix"),
		"a build failure":        {Exit: 1, Stdout: []byte(jsonLines(`{"ImportPath":"` + pkgCalc + ` [` + pkgCalc + `.test]","Action":"build-fail"}`))},
		"output that is cut off": {Stdout: []byte("{\"Action\":\"pass\",\"Pack")},
	}
	for name, red := range cases {
		t.Run(name, func(t *testing.T) {
			repo := sharedRepo(t)
			opts := fixtureOptions(repo, t.TempDir(), []string{"calc/fix.go"}, []string{"cmp"})
			guard := newRunGuard(t, repo)
			opts.Log = guard
			fake := newFakeGo(t, func(fakeCall) (cmdResult, error) { return red, nil })
			_, err := run(context.Background(), opts, fake.tools())
			if err == nil || !strings.Contains(err.Error(), "baseline is not green") || !strings.Contains(err.Error(), "run 1") {
				t.Fatalf("run error = %v, want %q from run 1", err, "baseline is not green")
			}
			if guard.applied != 0 || len(fake.all()) != 1 {
				t.Errorf("applied %d mutants and %d go calls, want none and one", guard.applied, len(fake.all()))
			}
			requireNoOutputs(t, opts)
			guard.finish(t)
		})
	}
}

// The baseline durations are, per top-level test, the median of the two
// baseline runs in milliseconds, rounded half up. Subtests have no entry, a
// test that go test timed at 0 does, and the file next to the JSONL holds the
// same map sorted by key.
func TestRunRecordsTheMedianBaselineDurations(t *testing.T) {
	repo := sharedRepo(t)
	sha := gitIn(t, repo, "rev-parse", "HEAD")
	opts := fixtureOptions(repo, t.TempDir(), []string{"calc/settle.go"}, []string{"cmp"})
	guard := newRunGuard(t, repo)
	opts.Log = guard
	pass := func(pkg, test string, elapsed float64) string {
		return fmt.Sprintf(`{"Action":"pass","Package":%q,"Test":%q,"Elapsed":%v}`, pkg, test, elapsed)
	}
	baselineRuns := []string{
		jsonLines(
			pass(pkgCalc, "TestFix", 0.010),
			pass(pkgCalc, "TestFix/sub", 7),
			pass(pkgCalc, "TestFast", 0),
			pass(pkgApp, "TestNormalize", 2.5),
			`{"Action":"pass","Package":"`+pkgCalc+`","Elapsed":3}`,
		),
		jsonLines(
			pass(pkgApp, "TestNormalize", 2.75),
			pass(pkgCalc, "TestFast", 0),
			pass(pkgCalc, "TestFix/sub", 8),
			pass(pkgCalc, "TestFix", 0.013),
			`{"Action":"pass","Package":"`+pkgCalc+`","Elapsed":4}`,
		),
	}
	baselines := 0
	fake := newFakeGo(t, func(c fakeCall) (cmdResult, error) {
		if c.Kind == "baseline" {
			baselines++
			return cmdResult{Stdout: []byte(baselineRuns[baselines-1])}, nil
		}
		return passRun(), nil
	})
	sum, err := run(context.Background(), opts, fake.tools())
	if err != nil {
		t.Fatalf("run: %v\nlog:\n%s", err, guard.logText())
	}
	guard.finish(t)
	want := map[string]int64{
		"go:" + pkgApp + ":TestNormalize": 2625, // (2500 + 2750) / 2
		"go:" + pkgCalc + ":TestFast":     0,
		"go:" + pkgCalc + ":TestFix":      12, // (10 + 13) / 2 = 11.5
	}
	if !reflect.DeepEqual(sum.BaselineMS, want) {
		t.Errorf("BaselineMS = %v, want %v", sum.BaselineMS, want)
	}
	requirePublished(t, opts, sum)
	raw, err := os.ReadFile(opts.Out + ".baseline.json")
	if err != nil {
		t.Fatal(err)
	}
	wantFile := fmt.Sprintf(`{"base_sha":%q,"tests":{%q:2625,%q:0,%q:12}}`+"\n", sha,
		"go:"+pkgApp+":TestNormalize", "go:"+pkgCalc+":TestFast", "go:"+pkgCalc+":TestFix")
	if string(raw) != wantFile {
		t.Errorf("baseline file = %q\nwant            %q", raw, wantFile)
	}
}

// The baseline must be the same suite twice: a test that only one run reported
// has no median, so the run stops before any mutant is applied.
func TestRunRefusesBaselinesThatDisagreeOnTheTestSet(t *testing.T) {
	repo := sharedRepo(t)
	opts := fixtureOptions(repo, t.TempDir(), []string{"calc/fix.go"}, []string{"cmp"})
	guard := newRunGuard(t, repo)
	opts.Log = guard
	baselines := 0
	fake := newFakeGo(t, func(c fakeCall) (cmdResult, error) {
		if c.Kind == "baseline" {
			baselines++
			if baselines == 2 {
				return cmdResult{Stdout: []byte(jsonLines(
					`{"Action":"pass","Package":"` + pkgCalc + `","Test":"TestFix","Elapsed":0.1}`,
				))}, nil
			}
			return cmdResult{Stdout: []byte(jsonLines(
				`{"Action":"pass","Package":"`+pkgCalc+`","Test":"TestFix","Elapsed":0.1}`,
				`{"Action":"pass","Package":"`+pkgCalc+`","Test":"TestOnce","Elapsed":0.1}`,
			))}, nil
		}
		return passRun(), nil
	})
	_, err := run(context.Background(), opts, fake.tools())
	want := "baseline runs disagree on the test set: go:" + pkgCalc + ":TestOnce"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("run error = %v, want %q", err, want)
	}
	if guard.applied != 0 || len(fake.all()) != 2 {
		t.Errorf("applied %d mutants and %d go calls, want none and the two baseline runs", guard.applied, len(fake.all()))
	}
	requireNoOutputs(t, opts)
	guard.finish(t)
}

// A duration is never invented: a test that passed without an elapsed time in
// either run stops the run, and the error names the test.
func TestRunRefusesABaselineTestWithoutAnElapsedTime(t *testing.T) {
	for _, bad := range []int{1, 2} {
		t.Run(fmt.Sprintf("run %d", bad), func(t *testing.T) {
			repo := sharedRepo(t)
			opts := fixtureOptions(repo, t.TempDir(), []string{"calc/fix.go"}, []string{"cmp"})
			guard := newRunGuard(t, repo)
			opts.Log = guard
			baselines := 0
			fake := newFakeGo(t, func(c fakeCall) (cmdResult, error) {
				if c.Kind != "baseline" {
					return passRun(), nil
				}
				baselines++
				if baselines == bad {
					return cmdResult{Stdout: []byte(jsonLines(`{"Action":"pass","Package":"` + pkgCalc + `","Test":"TestFix"}`))}, nil
				}
				return passRun(), nil
			})
			_, err := run(context.Background(), opts, fake.tools())
			if err == nil || !strings.Contains(err.Error(), "go:"+pkgCalc+":TestFix") || !strings.Contains(err.Error(), fmt.Sprintf("run %d", bad)) {
				t.Fatalf("run error = %v, want one naming go:%s:TestFix and run %d", err, pkgCalc, bad)
			}
			if guard.applied != 0 {
				t.Errorf("applied %d mutants after a baseline without durations", guard.applied)
			}
			requireNoOutputs(t, opts)
			guard.finish(t)
		})
	}
}

// Cancelling in the middle of the second mutant stops the run with the
// context's error, leaves the repository untouched, keeps the first mutant's
// record in the partial file as evidence, publishes nothing, and removes the
// slots.
func TestRunCanceledInTheSecondMutantKeepsTheFirstAsEvidence(t *testing.T) {
	repo := sharedRepo(t)
	opts := fixtureOptions(repo, t.TempDir(), []string{"calc/*.go"}, []string{"cmp", "ret"})
	opts.N = 2
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	guard := newRunGuard(t, repo)
	guard.onApplied = func() {
		if guard.applied == 2 {
			cancel()
		}
	}
	opts.Log = guard
	fake := newFakeGo(t, fixtureScript)
	_, err := run(ctx, opts, fake.tools())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("run error = %v, want context.Canceled", err)
	}
	guard.finish(t)
	if guard.applied != 2 {
		t.Errorf("applied %d mutants, want exactly the two that N allows", guard.applied)
	}
	if log := guard.logText(); !strings.Contains(log, "executing 2 of 5 mutants on 1 slots") {
		t.Errorf("N was not honored; log:\n%s", log)
	}
	if _, err := os.Stat(opts.Out); err == nil {
		t.Errorf("the final output exists after an interrupted run")
	}
	if _, err := os.Stat(opts.Out + ".baseline.json"); err == nil {
		t.Errorf("the baseline durations file exists after an interrupted run")
	}
	if _, err := os.Stat(opts.DBPath); err == nil {
		t.Errorf("the database exists after an interrupted run")
	}
	partial := readJSONL(t, opts.Out+".partial")
	if len(partial) != 1 {
		t.Fatalf("partial file holds %d records, want the one mutant that finished: %+v", len(partial), partial)
	}
	if r := partial[0]; r.ID == "" || r.Status == "" || r.BaseSHA == "" {
		t.Errorf("partial record is incomplete: %+v", r)
	}
}

// ---- the real go command ----

func TestRunRefusesRedBaseline(t *testing.T) {
	files := calcFixture("", "fix")
	files["calc/red_test.go"] = "package calc\n\nimport \"testing\"\n\nfunc TestRed(t *testing.T) { t.Fatal(\"red\") }\n"
	repo := newRepo(t, files)
	opts := fixtureOptions(repo, t.TempDir(), []string{"calc/fix.go"}, []string{"cmp"})
	guard := newRunGuard(t, repo)
	opts.Log = guard
	sum, err := Run(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "baseline is not green") {
		t.Fatalf("Run = %+v, %v; want an error containing %q", sum, err, "baseline is not green")
	}
	if sum.Executed != 0 || len(sum.Records) != 0 || guard.applied != 0 {
		t.Errorf("a mutant ran against a red baseline: %+v (applied %d)", sum, guard.applied)
	}
	requireNoOutputs(t, opts)
	guard.finish(t)
}

// Cancelling while the first baseline run is in progress ends the go command,
// removes the slots and leaves the repository and the output paths untouched.
func TestRunCanceledDuringTheBaselineLeavesNothingBehind(t *testing.T) {
	repo := sharedRepo(t)
	opts := fixtureOptions(repo, t.TempDir(), []string{"calc/fix.go"}, []string{"cmp"})
	opts.Packages = []string{"./calc"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	guard := newRunGuard(t, repo)
	guard.onLine = func(line string) {
		if strings.Contains(line, "baseline run 1 of 2 starting") {
			time.AfterFunc(time.Second, cancel)
		}
	}
	opts.Log = guard
	_, err := Run(ctx, opts)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
	guard.finish(t)
	if guard.applied != 0 {
		t.Errorf("applied %d mutants before the baseline finished", guard.applied)
	}
	requireNoOutputs(t, opts)
}

// The real go toolchain over the shared fixture. Its launches dominate the cost
// of this package's tests on this host, so one run covers every outcome that does
// not need a hang, and every run repeats the baseline twice and the package graph
// load. The Timeout is long: a go test that merely runs slowly under load must
// not be mistaken for a hang.
func TestRunEndToEnd(t *testing.T) {
	marker := armedMarker(t)
	repo := sharedRepo(t)
	sha := gitIn(t, repo, "rev-parse", "HEAD")
	files := []string{"calc/fix.go", "calc/settle.go", "calc/check.go", "calc/armed.go", "config/limits.yaml"}
	opts := fixtureOptions(repo, t.TempDir(), files, []string{"cmp", "ret", "cfg"})
	opts.Concurrency = 4
	opts.MaxHops = 0 // app imports calc one hop away: beyond a bound of zero
	opts.Timeout = 15 * time.Minute
	guard := newRunGuard(t, repo)
	opts.Log = guard
	sum, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run: %v\nlog:\n%s", err, guard.logText())
	}
	t.Logf("run log:\n%s", guard.logText())
	guard.finish(t)
	if guard.applied != 5 {
		t.Errorf("applied %d mutants, want 5", guard.applied)
	}
	if log := guard.logText(); !strings.Contains(log, "executing 5 of 5 mutants on 4 slots") {
		t.Errorf("slot count not honored; log:\n%s", log)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("the marker was never created, so TestArmed did not fail in the first run: %v", err)
	}
	wants := fixtureWants()
	delete(wants, "calc/count.go") // the hang has a run of its own
	requireOutcome(t, sum, sha, wants)
	// An unconfirmed kill is not a label: it never reaches the database.
	requirePublished(t, opts, sum)
}

// A mutant that never ends is a timeout, not a failure of the run, and has no
// killers. A short Timeout makes the hang cheap: whether the test binary's own
// timeout or the driver's deadline ends it first, the status is the same.
func TestRunTimeoutMutant(t *testing.T) {
	repo := sharedRepo(t)
	sha := gitIn(t, repo, "rev-parse", "HEAD")
	opts := fixtureOptions(repo, t.TempDir(), []string{"calc/count.go"}, []string{"cmp"})
	opts.Packages = []string{"./calc"}
	opts.Timeout = 20 * time.Second
	guard := newRunGuard(t, repo)
	opts.Log = guard
	sum, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run: %v\nlog:\n%s", err, guard.logText())
	}
	t.Logf("run log:\n%s", guard.logText())
	guard.finish(t)
	requireOutcome(t, sum, sha, map[string]fixtureWant{"calc/count.go": fixtureWants()["calc/count.go"]})
	requirePublished(t, opts, sum)
}
