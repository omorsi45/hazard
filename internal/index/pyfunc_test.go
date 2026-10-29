package index

import (
	"reflect"
	"testing"
)

const pyFuncSource = `"""Module docstring."""
import pytest


def helper():
    assert False


def test_plain():
    """Checks the plain path.

    More detail that is not the purpose.
    """
    value = compute()
    assert value == 1, "value must be one"
    assert (value
            != 2)  # trailing comment
    if value:
        assert value is not None
    assert value == 1, "value must be one"


async def test_async():
    '''Async docstring.'''
    assert await thing()


def test_nodoc():
    assert a, b
    assert "x, y" in text, "message"


class TestC:
    """Class docstring."""

    def test_b(self):
        """Method docstring."""
        assert self.x == 1

    def test_dup(self):
        assert 1

    def test_dup(self):
        """Second wins."""
        assert 2

    class TestInner:
        def test_deep(self):
            r"""Raw docstring."""
            assert deep


def test_many():
    assert 1 == 1
    assert 2 == 2
    assert 3 == 3
    assert 4 == 4
    assert 5 == 5
    assert 6 == 6


def test_long():
    assert name == "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"


def test_header(
    a,
    b=1,
):
    pass
`

func TestLexPythonStripsLeadingBOM(t *testing.T) {
	src := "\xef\xbb\xbfdef test_first():\n    \"\"\"Checks the first line.\"\"\"\n    assert 1 == 1\n"
	got, found := findPyFunc(lexPython([]byte(src)), nil, "test_first")
	want := pyFunc{Line: 1, Purpose: "Checks the first line.", Asserts: []string{"1 == 1"}}
	if !found || !reflect.DeepEqual(got, want) {
		t.Fatalf("a function on line 1 after a byte order mark: findPyFunc = %+v, %v; want %+v", got, found, want)
	}
}

func TestDiscoverPythonFunctionScan(t *testing.T) {
	stmts := lexPython([]byte(pyFuncSource))
	long := `name == "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"`
	for _, tc := range []struct {
		name    string
		classes []string
		fn      string
		want    pyFunc
		found   bool
	}{
		{"plain", nil, "test_plain", pyFunc{Line: 9, Purpose: "Checks the plain path.", Asserts: []string{"value == 1", "(value != 2)", "value is not None"}}, true},
		{"async", nil, "test_async", pyFunc{Line: 23, Purpose: "Async docstring.", Asserts: []string{"await thing()"}}, true},
		{"no doc and comma split", nil, "test_nodoc", pyFunc{Line: 28, Asserts: []string{"a", `"x, y" in text`}}, true},
		{"method", []string{"TestC"}, "test_b", pyFunc{Line: 36, Purpose: "Method docstring.", Asserts: []string{"self.x == 1"}}, true},
		{"last definition wins", []string{"TestC"}, "test_dup", pyFunc{Line: 43, Purpose: "Second wins.", Asserts: []string{"2"}}, true},
		{"nested class", []string{"TestC", "TestInner"}, "test_deep", pyFunc{Line: 48, Purpose: "Raw docstring.", Asserts: []string{"deep"}}, true},
		{"five assertion cap", nil, "test_many", pyFunc{Line: 53, Asserts: []string{"1 == 1", "2 == 2", "3 == 3", "4 == 4", "5 == 5"}}, true},
		{"truncation", nil, "test_long", pyFunc{Line: 62, Asserts: []string{long[:80]}}, true},
		{"multi-line header", nil, "test_header", pyFunc{Line: 66}, true},
		{"module function is not a method", []string{"TestC"}, "test_plain", pyFunc{}, false},
		{"method is not module level", nil, "test_b", pyFunc{}, false},
		{"missing class", []string{"Nope"}, "test_b", pyFunc{}, false},
		{"missing function", nil, "test_absent", pyFunc{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, found := findPyFunc(stmts, tc.classes, tc.fn)
			if found != tc.found || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("findPyFunc = %+v, %v; want %+v, %v", got, found, tc.want, tc.found)
			}
		})
	}
}
