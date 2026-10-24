package index

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/omorsi45/hazard/internal/config"
	_ "modernc.org/sqlite"
)

func TestCoveragePythonDecodeNumbits(t *testing.T) {
	for name, tc := range map[string]struct {
		in   []byte
		want []int
	}{
		"empty":         {nil, nil},
		"bit order":     {[]byte{0b00000110, 0b00000001}, []int{1, 2, 8}},
		"high bit":      {[]byte{0b10000000}, []int{7}},
		"line 17":       {[]byte{0, 0, 0b00000010}, []int{17}},
		"all of a byte": {[]byte{0xff}, []int{0, 1, 2, 3, 4, 5, 6, 7}},
	} {
		if got := decodeNumbits(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: decodeNumbits(%v) = %v, want %v", name, tc.in, got, tc.want)
		}
	}
}

// numbits encodes lines like coverage.py: bit i of byte j is line j*8+i.
func numbits(lines ...int) []byte {
	var out []byte
	for _, l := range lines {
		for len(out) <= l/8 {
			out = append(out, 0)
		}
		out[l/8] |= 1 << (l % 8)
	}
	return out
}

func writeCoverageDB(t *testing.T, path string, files []string, contexts []string, rows [][3]any) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		`CREATE TABLE file (id integer primary key, path text, unique (path))`,
		`CREATE TABLE context (id integer primary key, context text, unique (context))`,
		`CREATE TABLE line_bits (file_id integer, context_id integer, numbits blob, unique (file_id, context_id))`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	for i, f := range files {
		if _, err := db.Exec(`INSERT INTO file VALUES (?, ?)`, i+1, f); err != nil {
			t.Fatal(err)
		}
	}
	for i, c := range contexts {
		if _, err := db.Exec(`INSERT INTO context VALUES (?, ?)`, i+1, c); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range rows {
		if _, err := db.Exec(`INSERT INTO line_bits VALUES (?, ?, ?)`, r[0], r[1], r[2]); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCoveragePythonReadDBAndAttribute(t *testing.T) {
	dir := t.TempDir()
	root := filepath.ToSlash(dir)
	db := filepath.Join(dir, "cov.db")
	writeCoverageDB(t, db,
		[]string{"pkg/calc.py", `tests\test_calc.py`, root + "/pkg/abs.py", "/elsewhere/site-packages/dep.py"},
		[]string{"", "tests.test_calc.test_add", "tests.test_calc.TestC.test_both", "weird.test_helper"},
		[][3]any{
			{1, 1, numbits(1, 3, 6)}, // calc.py lines run outside any test (import time)
			{2, 1, numbits(1, 2)},    // test file import time
			{1, 2, numbits(4)},       // test_add runs calc line 4
			{1, 3, numbits(4, 5, 7)}, // test_both runs 4,5,7
			{1, 4, numbits(9)},       // an unmapped context
			{3, 2, numbits(2)},       // absolute path inside the repo
			{4, 2, numbits(1)},       // outside the repository
		})
	data, err := readCoverageDB(db, dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := data["tests.test_calc.test_add"]["pkg/calc.py"]; !reflect.DeepEqual(got, []int{4}) {
		t.Errorf("test_add lines = %v", got)
	}
	if got := data["tests.test_calc.test_add"]["pkg/abs.py"]; !reflect.DeepEqual(got, []int{2}) {
		t.Errorf("absolute paths inside the repository must become relative: %v", data["tests.test_calc.test_add"])
	}
	if got := data[""]["tests/test_calc.py"]; !reflect.DeepEqual(got, []int{1, 2}) {
		t.Errorf("backslashes must become slashes: %v", data[""])
	}
	for ctx, files := range data {
		for f := range files {
			if strings.Contains(f, "site-packages") {
				t.Errorf("file outside the repository kept in %q: %s", ctx, f)
			}
		}
	}
	tests := []Test{
		{ID: "py:tests.test_calc:test_add", Package: "tests.test_calc", Name: "test_add"},
		{ID: "py:tests.test_calc:test_p[1]", Package: "tests.test_calc", Name: "test_p[1]"},
		{ID: "py:tests.test_calc:test_p[2]", Package: "tests.test_calc", Name: "test_p[2]"},
		{ID: "py:tests.test_calc.TestC:test_both", Package: "tests.test_calc.TestC", Name: "test_both"},
		{ID: "py:tests.test_calc:test_skipped", Package: "tests.test_calc", Name: "test_skipped"},
	}
	cov, diags := attributePython(data, tests)
	add := cov["py:tests.test_calc:test_add"]
	if add.Status != "covered" || !blocksCover(add.Blocks, "pkg/calc.py", 4) || blocksCover(add.Blocks, "pkg/calc.py", 5) || blocksCover(add.Blocks, "pkg/calc.py", 7) {
		t.Errorf("test_add = %+v", add)
	}
	for _, line := range []int{1, 3, 6, 9} {
		if !blocksCover(add.Blocks, "pkg/calc.py", line) {
			t.Errorf("line %d ran outside any test (import time, fixtures, unmapped context) and must count for every test: %+v", line, add.Blocks)
		}
	}
	both := cov["py:tests.test_calc.TestC:test_both"]
	if both.Status != "covered" || !blocksCover(both.Blocks, "pkg/calc.py", 7) || !blocksCover(both.Blocks, "pkg/calc.py", 5) {
		t.Errorf("test_both = %+v", both)
	}
	if skipped := cov["py:tests.test_calc:test_skipped"]; skipped.Status != "not_run" || len(skipped.Blocks) != 0 {
		t.Errorf("a test without a context is unknown, never covered: %+v", skipped)
	}
	if cov["py:tests.test_calc:test_p[1]"].Status != "not_run" {
		t.Errorf("parametrized test without context: %+v", cov["py:tests.test_calc:test_p[1]"])
	}
	var unmapped bool
	for _, d := range diags {
		unmapped = unmapped || strings.Contains(d, "weird.test_helper")
	}
	if !unmapped {
		t.Errorf("unmapped contexts must be reported: %v", diags)
	}
	for _, b := range add.Blocks {
		if b.File == "pkg/calc.py" && b.StartLine == 4 && b.EndLine != 4 {
			t.Errorf("single lines must stay single-line blocks: %+v", b)
		}
	}
}

func TestCoveragePythonParametrizedTestsShareAContext(t *testing.T) {
	data := pyLines{"tests.test_calc.test_p": {"pkg/calc.py": {4}}}
	tests := []Test{
		{ID: "py:tests.test_calc:test_p[1]", Package: "tests.test_calc", Name: "test_p[1]"},
		{ID: "py:tests.test_calc:test_p[2]", Package: "tests.test_calc", Name: "test_p[2]"},
	}
	cov, _ := attributePython(data, tests)
	for _, test := range tests {
		if got := cov[test.ID]; got.Status != "covered" || !blocksCover(got.Blocks, "pkg/calc.py", 4) {
			t.Errorf("%s = %+v", test.ID, got)
		}
	}
	// A context carrying a longer module path (pytest rootdir inserted a package prefix) still maps.
	data = pyLines{"src.tests.test_calc.test_p": {"pkg/calc.py": {4}}}
	cov, _ = attributePython(data, tests)
	if cov[tests[0].ID].Status != "covered" {
		t.Errorf("longer module prefix = %+v", cov[tests[0].ID])
	}
}

const coveragePyCalc = "LIMIT = 10\n\n\ndef add(a, b):\n    return a + b\n\n\ndef mul(a, b):\n    return a * b\n"

func coveragePyFixture(extra string) map[string]string {
	return map[string]string{
		"pkg/__init__.py":    "",
		"pkg/calc.py":        coveragePyCalc,
		"tests/__init__.py":  "",
		"tests/test_calc.py": "import pytest\n\nfrom pkg.calc import add, mul\n\n\ndef test_add():\n    assert add(1, 2) == 3\n\n\ndef test_mul():\n    assert mul(2, 3) == 6\n\n\n@pytest.mark.parametrize(\"n\", [1, 2])\ndef test_param(n):\n    assert add(n, 0) == n\n\n\n@pytest.mark.skip(reason=\"never runs\")\ndef test_skipped():\n    assert mul(1, 1) == 1\n\n\nclass TestC:\n    def test_both(self):\n        assert add(1, 1) + mul(1, 1) == 3\n" + extra,
	}
}

func pyCoverageConfig() config.Config {
	cfg := config.Defaults()
	cfg.Languages.Go.Enabled = false
	return cfg
}

func TestCoveragePythonContextsFromCoveragePy(t *testing.T) {
	t.Parallel()
	python := testPython(t, "pytest", "coverage")
	dir := gitRepo(t, coveragePyFixture(""))
	m, err := Build(context.Background(), dir, pyCoverageConfig(), Options{Coverage: true, Python: python})
	if err != nil {
		t.Fatal(err)
	}
	c := m.Coverage
	if c == nil {
		t.Fatalf("coverage missing, diagnostics: %v", m.Diagnostics)
	}
	if c.Granularity != "python-test-context" || c.HeadSHA != m.HeadSHA || c.ContentDigest != m.ContentDigest {
		t.Errorf("coverage header = %+v", c)
	}
	if c.FileDigests["pkg/calc.py"] != hexSHA(coveragePyCalc) {
		t.Errorf("FileDigests = %v", c.FileDigests)
	}
	add := c.Tests["py:tests.test_calc:test_add"]
	mul := c.Tests["py:tests.test_calc:test_mul"]
	both := c.Tests["py:tests.test_calc.TestC:test_both"]
	if add.Status != "covered" || !blocksCover(add.Blocks, "pkg/calc.py", 5) || blocksCover(add.Blocks, "pkg/calc.py", 9) {
		t.Errorf("test_add = %+v: must run add's body (line 5) and not mul's (line 9)", add)
	}
	if mul.Status != "covered" || !blocksCover(mul.Blocks, "pkg/calc.py", 9) || blocksCover(mul.Blocks, "pkg/calc.py", 5) {
		t.Errorf("test_mul = %+v", mul)
	}
	if !blocksCover(add.Blocks, "pkg/calc.py", 1) || !blocksCover(mul.Blocks, "pkg/calc.py", 4) {
		t.Errorf("import-time lines (LIMIT, def add) must count for every test: %+v", add.Blocks)
	}
	if both.Status != "covered" || !blocksCover(both.Blocks, "pkg/calc.py", 5) || !blocksCover(both.Blocks, "pkg/calc.py", 9) {
		t.Errorf("test_both = %+v", both)
	}
	for _, id := range []string{"py:tests.test_calc:test_param[1]", "py:tests.test_calc:test_param[2]"} {
		if got := c.Tests[id]; got.Status != "covered" || !blocksCover(got.Blocks, "pkg/calc.py", 5) {
			t.Errorf("%s = %+v", id, got)
		}
	}
	if skipped := c.Tests["py:tests.test_calc:test_skipped"]; skipped.Status != "not_run" || len(skipped.Blocks) != 0 {
		t.Errorf("a skipped test is unknown: %+v", skipped)
	}
	if m.Tools["coverage"] == "" || m.Tools["pytest"] == "" || m.Tools["python"] == "" {
		t.Errorf("tools = %v", m.Tools)
	}
	var sawCoverage bool
	for _, cmd := range m.Commands {
		joined := strings.Join(cmd.Argv, " ")
		if strings.Contains(joined, "-m coverage run --rcfile=") && strings.Contains(joined, "-m pytest -q -p no:cacheprovider tests") {
			sawCoverage = cmd.Exit == 0
		}
	}
	if !sawCoverage {
		t.Errorf("coverage run not recorded: %+v", m.Commands)
	}
}

func TestCoveragePythonFailingSuitePublishesWithoutCoverage(t *testing.T) {
	t.Parallel()
	python := testPython(t, "pytest", "coverage")
	dir := gitRepo(t, coveragePyFixture("\n\ndef test_broken():\n    assert False\n"))
	m, err := Build(context.Background(), dir, pyCoverageConfig(), Options{Coverage: true, Python: python})
	if err != nil {
		t.Fatalf("a failing suite must not fail the index: %v", err)
	}
	var diag bool
	for _, d := range m.Diagnostics {
		diag = diag || strings.Contains(d, "python coverage unavailable")
	}
	if m.Coverage != nil || !diag || len(m.Tests) == 0 {
		t.Errorf("coverage=%v diagnostics=%v tests=%d", m.Coverage, m.Diagnostics, len(m.Tests))
	}
}
