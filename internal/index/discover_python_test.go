package index

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestDiscoverPythonParseCollect(t *testing.T) {
	for _, tc := range []struct {
		name, out string
		want      []string
		wantErr   string
	}{
		{"nodes and summary", "tests/test_x.py::test_a\ntests/test_x.py::TestC::test_b\n\n2 tests collected in 0.05s\n", []string{"tests/test_x.py::test_a", "tests/test_x.py::TestC::test_b"}, ""},
		{"crlf and params", "tests/test_x.py::test_p[a::b]\r\ntests/test_x.py::test_p[x y]\r\n\r\n2 tests collected in 0.05s\r\n", []string{"tests/test_x.py::test_p[a::b]", "tests/test_x.py::test_p[x y]"}, ""},
		{"nothing collected", "\nno tests collected in 0.01s\n", nil, ""},
		{"stops at the first blank line", "tests/test_x.py::test_a\n\n=== warnings summary ===\ntests/test_x.py::test_w\n1 tests collected in 0.01s\n", []string{"tests/test_x.py::test_a"}, ""},
		{"leading blank line", "\ntests/test_x.py::test_a\n\n1 test collected in 0.01s\n", []string{"tests/test_x.py::test_a"}, ""},
		{"collection error lines are not tests", "tests/test_x.py::test_a\n\n= ERRORS =\nERROR tests/test_bad.py::boom\n", []string{"tests/test_x.py::test_a"}, ""},
		{"per-file counts from addopts", "tests/test_x.py: 4\n\n", nil, "per-file counts"},
		{"summary disagrees with nodes", "tests/test_x.py::test_a\n\n3 tests collected in 0.01s\n", nil, "collected 3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCollect(tc.out)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parseCollect = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

const discoverPyTests = `import pytest


def test_a():
    """Checks A."""
    assert 1 == 1, "one"


@pytest.mark.parametrize("v", [1, "a::b"])
def test_p(v):
    assert v


class TestC:
    def test_b(self):
        assert 2 > 1
`

func TestDiscoverPythonCollectsAndMapsIdentities(t *testing.T) {
	python := testPython(t, "pytest")
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"tests/test_x.py": discoverPyTests})
	got, err := discoverPython(context.Background(), dir, python, []string{"tests"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Incomplete || len(got.Diagnostics) != 0 {
		t.Fatalf("unexpected diagnostics: %+v", got)
	}
	byID := map[string]Test{}
	for _, tc := range got.Tests {
		byID[tc.ID] = tc
	}
	want := map[string]Test{
		"py:tests.test_x:test_a":       {ID: "py:tests.test_x:test_a", Package: "tests.test_x", Name: "test_a", RunnerID: "tests/test_x.py::test_a", Line: 4, Purpose: "Checks A.", Asserts: []string{"1 == 1"}},
		"py:tests.test_x:test_p[1]":    {ID: "py:tests.test_x:test_p[1]", Package: "tests.test_x", Name: "test_p[1]", RunnerID: "tests/test_x.py::test_p[1]", Line: 10, Purpose: "test_p", Asserts: []string{"v"}},
		"py:tests.test_x:test_p[a::b]": {ID: "py:tests.test_x:test_p[a::b]", Package: "tests.test_x", Name: "test_p[a::b]", RunnerID: "tests/test_x.py::test_p[a::b]", Line: 10, Purpose: "test_p", Asserts: []string{"v"}},
		"py:tests.test_x.TestC:test_b": {ID: "py:tests.test_x.TestC:test_b", Package: "tests.test_x.TestC", Name: "test_b", RunnerID: "tests/test_x.py::TestC::test_b", Line: 15, Purpose: "test_b", Asserts: []string{"2 > 1"}},
	}
	if len(byID) != len(want) {
		t.Fatalf("tests = %+v", got.Tests)
	}
	for id, w := range want {
		w.Language, w.File = "python", "tests/test_x.py"
		if !reflect.DeepEqual(byID[id], w) {
			t.Errorf("%s = %+v, want %+v", id, byID[id], w)
		}
	}
	if len(got.Commands) != 1 || got.Commands[0].Exit != 0 || got.Commands[0].Dir != dir {
		t.Fatalf("commands = %+v", got.Commands)
	}
	argv := got.Commands[0].Argv
	if len(argv) < 8 || !reflect.DeepEqual(argv[1:], []string{"-m", "pytest", "--collect-only", "-q", "-p", "no:cacheprovider", "tests"}) {
		t.Errorf("argv = %q", argv)
	}
}

func TestDiscoverPythonNoTestsIsNotAnError(t *testing.T) {
	python := testPython(t, "pytest")
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"tests/helper.py": "VALUE = 1\n"})
	got, err := discoverPython(context.Background(), dir, python, []string{"tests"})
	if err != nil || len(got.Tests) != 0 || len(got.Commands) != 1 || got.Commands[0].Exit != 5 {
		t.Fatalf("exit 5 (no tests collected) must yield an empty inventory: %+v %v", got, err)
	}
}

func TestDiscoverPythonFailuresAreErrors(t *testing.T) {
	python := testPython(t, "pytest")
	for name, files := range map[string]map[string]string{
		"collection error":  {"tests/test_bad.py": "import module_that_does_not_exist_xyz\n"},
		"per-file counts":   {"tests/test_x.py": "def test_a():\n    pass\n", "pytest.ini": "[pytest]\naddopts = -q\n"},
		"unknown test path": {"other/test_x.py": "def test_a():\n    pass\n"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeFiles(t, dir, files)
			paths := []string{"tests"}
			if name == "unknown test path" {
				paths = []string{"missing_dir"}
			}
			if got, err := discoverPython(context.Background(), dir, python, paths); err == nil {
				t.Fatalf("discovery failure must be an error, got %+v", got)
			}
		})
	}
}

func TestDiscoverPythonCanceledContext(t *testing.T) {
	python := testPython(t, "pytest")
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"tests/test_x.py": "def test_a():\n    pass\n"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := discoverPython(ctx, dir, python, []string{"tests"}); err == nil {
		t.Fatal("canceled context must fail discovery")
	}
}
