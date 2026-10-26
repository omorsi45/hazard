package change

import (
	"reflect"
	"strings"
	"testing"
)

const goSample = `// Package p docs.
package p

import "fmt"

// Foo does things.
func Foo() {
	fmt.Println()
}

type T struct{ x int }

func (t T) Val() int { return t.x }

// Ptr docs
func (t *T) Ptr() {
}

type G[K comparable] struct{}

func (g *G[K]) Get() {}

var X = 1

var (
	a    = 1
	b, c = 2, 3
)

const (
	Red = iota
	Green
	Blue
)

const K1, K2 = 1, 2

type (
	I interface{ M() }
	A = int
)

func init() {}
func init() {}
`

func TestSymbolsGo(t *testing.T) {
	got, err := GoSymbols([]byte(goSample))
	if err != nil {
		t.Fatalf("GoSymbols: %v", err)
	}
	want := []Interval{
		{"Foo", 6, 9},
		{"T", 11, 11},
		{"T.Val", 13, 13},
		{"(*T).Ptr", 15, 17},
		{"G", 19, 19},
		{"(*G).Get", 21, 21},
		{"X", 23, 23},
		{"a", 26, 26},
		{"b", 27, 27},
		{"c", 27, 27},
		// Implicit repetition (iota): every name spans the whole group, so a
		// change to one line is attributed to all of them.
		{"Red", 30, 34},
		{"Green", 30, 34},
		{"Blue", 30, 34},
		{"K1", 36, 36},
		{"K2", 36, 36},
		{"I", 39, 39},
		{"A", 40, 40},
		{"init", 43, 43},
		{"init", 44, 44},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("GoSymbols intervals\n got: %v\nwant: %v", got, want)
	}
}

func TestSymbolsGoCRLFHasSameLines(t *testing.T) {
	lf, err := GoSymbols([]byte(goSample))
	if err != nil {
		t.Fatal(err)
	}
	crlf, err := GoSymbols([]byte(strings.ReplaceAll(goSample, "\n", "\r\n")))
	if err != nil {
		t.Fatal(err)
	}
	if len(lf) == 0 || !reflect.DeepEqual(lf, crlf) {
		t.Errorf("CRLF intervals differ\n lf: %v\ncrlf: %v", lf, crlf)
	}
}

func TestSymbolsGoParseError(t *testing.T) {
	got, err := GoSymbols([]byte("package p\n\nfunc broken( {\n"))
	if err == nil {
		t.Fatalf("GoSymbols accepted broken source, intervals %v", got)
	}
}

func TestSymbolsGoNoDeclarations(t *testing.T) {
	got, err := GoSymbols([]byte("package p\n\nimport \"os\"\n"))
	if err != nil || len(got) != 0 {
		t.Errorf("GoSymbols = %v, %v; want no intervals and no error", got, err)
	}
}

const pySample = `import os

@decorator
@other(
    arg=1,
)
def deco(a,
         b):
    """doc
at col 0 line
"""
    x = "def not_a_func():"
    return x

# comment about K
class K:
    def method(self):
        pass

    # trailing comment in class

async def co():
    await x

def one(): return 1
if __name__ == "__main__":
    deco()

def tail(
a,
):
    return (
1
)
`

func TestSymbolsPython(t *testing.T) {
	got := PythonSymbols([]byte(pySample))
	want := []Interval{
		{"deco", 3, 13},
		{"K", 16, 18},
		{"co", 22, 23},
		{"one", 25, 25},
		{"tail", 29, 34},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("PythonSymbols intervals\n got: %v\nwant: %v", got, want)
	}
}

func TestSymbolsPythonCases(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []Interval
	}{
		{"empty", "", nil},
		{"only statements", "x = 1\nprint(x)\n", nil},
		{"keyword prefix is not a def", "defx = 1\nclass_name = 2\ndefine()\n", nil},
		{"commented def", "# def f():\n#     pass\n", nil},
		{"def inside a string", "s = '''\ndef f():\n    pass\nclass C:\n'''\n", nil},
		{"nested def is not top level", "if True:\n    def inner():\n        pass\n", nil},
		{"tab after def", "def\tf():\n    pass\n", []Interval{{"f", 1, 2}}},
		{"async with spaces", "async   def   g():\n    pass\n", []Interval{{"g", 1, 2}}},
		{"non-ascii name", "def héllo():\n    pass\n", []Interval{{"héllo", 1, 2}}},
		{"backslash continuation at column 0", "def bs():\n    x = 1 + \\\n2\n    return x\ny = 1\n", []Interval{{"bs", 1, 4}}},
		{"single-quoted string continued by backslash", "def s():\n    x = 'a\\\nb'\n    return x\n", []Interval{{"s", 1, 4}}},
		{"hash inside a string is not a comment", "def h():\n    x = '#'\n    y = (\n1\n)\n", []Interval{{"h", 1, 5}}},
		{"def without body newline at EOF", "def f():\n    pass", []Interval{{"f", 1, 2}}},
		{"crlf", "def f():\r\n    pass\r\n\r\ndef g():\r\n    pass\r\n", []Interval{{"f", 1, 2}, {"g", 4, 5}}},
		{"decorator then comment then def", "@d\n# note\ndef f():\n    pass\n", []Interval{{"f", 1, 4}}},
		{"decorator not carried to the next def", "@d\ndef f():\n    pass\nx = 1\ndef g():\n    pass\n", []Interval{{"f", 1, 3}, {"g", 5, 6}}},
		{"adjacent classes", "class A:\n    pass\nclass B:\n    pass\n", []Interval{{"A", 1, 2}, {"B", 3, 4}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := PythonSymbols([]byte(tc.src))
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("PythonSymbols(%q)\n got: %v\nwant: %v", tc.src, got, tc.want)
			}
		})
	}
}
