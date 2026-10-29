package index

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestPythonTestNames(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		src  string
		want []string
	}{
		{"empty module", "", nil},
		{"no tests", "import os\n\nVALUE = 1\n\ndef helper():\n    pass\n", nil},
		{"top-level functions", "def test_a():\n    pass\n\ndef test_b():\n    pass\n", []string{"test_a", "test_b"}},
		{"name prefix is test, case sensitive", "def test():\n    pass\ndef testing_x():\n    pass\ndef Test_upper():\n    pass\ndef tes():\n    pass\ndef atest():\n    pass\n", []string{"test", "testing_x"}},
		{"async function", "async def test_async():\n    pass\n", []string{"test_async"}},
		{"spaces around the parameter list", "def  test_a  (x):\n    pass\n", []string{"test_a"}},
		{"one-line body", "def test_a(): pass\n", []string{"test_a"}},
		{"parametrized and decorated", "import pytest\n\n@pytest.mark.parametrize(\n    'v',\n    [1, 2],\n)\ndef test_p(v):\n    assert v\n", []string{"test_p"}},
		{"class methods", "class TestC:\n    def test_a(self):\n        pass\n    def helper(self):\n        pass\n    def test_b(self):\n        pass\n", []string{"TestC::test_a", "TestC::test_b"}},
		{"class prefix is Test, case sensitive", "class TestC:\n    def test_a(self):\n        pass\nclass Testing:\n    def test_b(self):\n        pass\nclass Test:\n    def test_c(self):\n        pass\nclass Helper:\n    def test_d(self):\n        pass\nclass testlower:\n    def test_e(self):\n        pass\n", []string{"TestC::test_a", "Testing::test_b", "Test::test_c"}},
		{"class with a base class", "class TestC(Base, object):\n    def test_a(self):\n        pass\n", []string{"TestC::test_a"}},
		{"class with an __init__ is not collected", "class TestC:\n    def __init__(self):\n        pass\n    def test_a(self):\n        pass\n\ndef test_after():\n    pass\n", []string{"test_after"}},
		{"__init__ after the methods", "class TestC:\n    def test_a(self):\n        pass\n    def __init__(self):\n        pass\n", nil},
		{"class without __init__ next to one with", "class TestA:\n    def __init__(self):\n        pass\n    def test_a(self):\n        pass\nclass TestB:\n    def test_b(self):\n        pass\n", []string{"TestB::test_b"}},
		{"async method and decorators", "class TestC:\n    @staticmethod\n    def test_s():\n        pass\n    async def test_async(self):\n        pass\n", []string{"TestC::test_s", "TestC::test_async"}},
		{"nested helper defs are not tests", "def test_a():\n    def test_inner():\n        pass\n    class TestInner:\n        def test_x(self):\n            pass\n", []string{"test_a"}},
		{"nested class inside a test class is not listed", "class TestC:\n    def test_a(self):\n        pass\n    class TestInner:\n        def test_x(self):\n            pass\n    def test_b(self):\n        pass\n", []string{"TestC::test_a", "TestC::test_b"}},
		{"in source order with the class between functions", "def test_a():\n    pass\nclass TestC:\n    def test_m(self):\n        pass\ndef test_z():\n    pass\n", []string{"test_a", "TestC::test_m", "test_z"}},
		{"redefinition is listed once", "def test_a():\n    pass\ndef test_a():\n    pass\nclass TestC:\n    def test_m(self):\n        pass\n    def test_m(self):\n        pass\n", []string{"test_a", "TestC::test_m"}},
		{"comments and strings are not code", "# def test_commented(): pass\nx = 'def test_in_string(): pass'\ndoc = \"\"\"\ndef test_in_triple():\n    pass\nclass TestInTriple:\n    def test_m(self):\n        pass\n\"\"\"\ndef test_real():\n    '''def test_docstring(): pass'''\n", []string{"test_real"}},
		{"method docstring with a def", "class TestC:\n    def test_a(self):\n        \"\"\"\n        def test_hidden(self): pass\n        \"\"\"\n", []string{"TestC::test_a"}},
		{"non-ASCII names", "def test_caf\u00e9():\n    pass\nclass Test\u00c9:\n    def test_\u00fc(self):\n        pass\n", []string{"test_caf\u00e9", "Test\u00c9::test_\u00fc"}},
		{"crlf line endings", "def test_a():\r\n    pass\r\nclass TestC:\r\n    def test_m(self):\r\n        pass\r\n", []string{"test_a", "TestC::test_m"}},
		{"leading byte order mark", "\xef\xbb\xbfdef test_a():\n    pass\n", []string{"test_a"}},
		{"tab indented methods", "class TestC:\n\tdef test_a(self):\n\t\tpass\n", []string{"TestC::test_a"}},
		{"multi-line signature", "def test_a(\n    x,\n    y=1,\n):\n    pass\n", []string{"test_a"}},
		{"assignment is not a definition", "test_x = 1\nclass TestC:\n    test_y = 2\n", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := PythonTestNames([]byte(tc.src))
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("PythonTestNames(%q) = %q, want %q", tc.src, got, tc.want)
			}
		})
	}
}

// TestPythonTestNamesAgreesWithPytest checks the names against a real pytest
// collection of the same module.
func TestPythonTestNamesAgreesWithPytest(t *testing.T) {
	t.Parallel()
	python := testPython(t, "pytest")
	const src = `import pytest


def test_a():
    pass


def helper():
    pass


@pytest.mark.parametrize("v", [1, "a::b"])
def test_p(v):
    assert v


class TestC:
    def test_m(self):
        pass

    def helper(self):
        pass


class TestWithInit:
    def __init__(self):
        pass

    def test_hidden(self):
        pass


class Helper:
    def test_not_collected(self):
        pass
`
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"tests/test_x.py": src})
	collected, err := discoverPython(context.Background(), dir, python, []string{"tests"})
	if err != nil {
		t.Fatal(err)
	}
	pytestNames := map[string]bool{}
	for _, test := range collected.Tests {
		// py:tests.test_x:test_p[1] and py:tests.test_x.TestC:test_m
		pkg := strings.TrimPrefix(test.Package, "tests.test_x")
		name := test.Name
		if i := strings.IndexByte(name, '['); i >= 0 {
			name = name[:i]
		}
		if class := strings.TrimPrefix(pkg, "."); class != "" {
			name = class + "::" + name
		}
		pytestNames[name] = true
	}
	got := map[string]bool{}
	for _, name := range PythonTestNames([]byte(src)) {
		got[name] = true
	}
	if !reflect.DeepEqual(got, pytestNames) {
		t.Fatalf("PythonTestNames = %v, pytest collected %v", got, pytestNames)
	}
}
