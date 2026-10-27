package graph

import (
	"reflect"
	"strings"
	"testing"
)

func TestPythonScanImportForms(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		want []pyImport
	}{
		{"plain", "import os\n", []pyImport{{Module: "os"}}},
		{"dotted", "import a.b.c\n", []pyImport{{Module: "a.b.c"}}},
		{"comma list", "import a.b, c ,d\n", []pyImport{{Module: "a.b"}, {Module: "c"}, {Module: "d"}}},
		{"alias", "import a.b as x, c as y\n", []pyImport{{Module: "a.b"}, {Module: "c"}}},
		{"from", "from a.b import c, d\n", []pyImport{{From: true, Module: "a.b", Names: []string{"c", "d"}}}},
		{"from alias", "from a import b as c, d as e\n", []pyImport{{From: true, Module: "a", Names: []string{"b", "d"}}}},
		{"star", "from a import *\n", []pyImport{{From: true, Module: "a", Names: []string{"*"}}}},
		{"parenthesized", "from x import (\n    a,\n    b as c,  # trailing comment\n    d,\n)\n", []pyImport{{From: true, Module: "x", Names: []string{"a", "b", "d"}}}},
		{"backslash", "from x import a, \\\n    b\n", []pyImport{{From: true, Module: "x", Names: []string{"a", "b"}}}},
		{"relative current", "from . import x\n", []pyImport{{From: true, Level: 1, Names: []string{"x"}}}},
		{"relative module", "from .m import y\n", []pyImport{{From: true, Level: 1, Module: "m", Names: []string{"y"}}}},
		{"relative parent", "from ..m.n import y, z\n", []pyImport{{From: true, Level: 2, Module: "m.n", Names: []string{"y", "z"}}}},
		{"relative triple dot", "from ... import q\n", []pyImport{{From: true, Level: 3, Names: []string{"q"}}}},
		{"semicolon", "import a; import b\n", []pyImport{{Module: "a"}, {Module: "b"}}},
		{"indented in function", "def f():\n    import lazy.mod\n    from lazy import other\n", []pyImport{{Module: "lazy.mod"}, {From: true, Module: "lazy", Names: []string{"other"}}}},
		{"try except", "try:\n    import fast\nexcept ImportError:\n    import slow\n", []pyImport{{Module: "fast"}, {Module: "slow"}}},
		{"one-line compound", "try: import a\nexcept ImportError: import b\nif x: from c import d\n", []pyImport{{Module: "a"}, {Module: "b"}, {From: true, Module: "c", Names: []string{"d"}}}},
		{"crlf", "import a\r\nfrom b import c\r\n", []pyImport{{Module: "a"}, {From: true, Module: "b", Names: []string{"c"}}}},
		{"spaced dots", "from a . b import c\n", []pyImport{{From: true, Module: "a.b", Names: []string{"c"}}}},
		{"no trailing newline", "import last", []pyImport{{Module: "last"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, dynamic, ok := scanPython([]byte(tc.src))
			if !ok || dynamic || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("scan = %+v dynamic=%v ok=%v, want %+v", got, dynamic, ok, tc.want)
			}
		})
	}
}

func TestPythonScanSkipsCommentsAndStrings(t *testing.T) {
	src := strings.Join([]string{
		`# import commented`,
		`x = "import in_string"`,
		`y = 'from nope import thing'`,
		`doc = """`,
		`import in_triple`,
		`from also import nothing`,
		`"""`,
		`z = '''multi`,
		`import another'''`,
		`s = "escaped \" import quote"  # import trailing`,
		`r = f"{x} import fstring"`,
		`def f():`,
		`    """Docstring mentioning import docstring_mod."""`,
		`import real_one`,
		``,
	}, "\n")
	got, dynamic, ok := scanPython([]byte(src))
	want := []pyImport{{Module: "real_one"}}
	if !ok || dynamic || !reflect.DeepEqual(got, want) {
		t.Fatalf("scan = %+v dynamic=%v ok=%v, want %+v", got, dynamic, ok, want)
	}
}

func TestPythonScanDynamicImports(t *testing.T) {
	for name, src := range map[string]string{
		"importlib attribute":        "import importlib\nmod = importlib.import_module('x')\n",
		"from import":                "from importlib import import_module\nm = import_module(name)\n",
		"dunder import":              "m = __import__('x')\n",
		"spaced attribute":           "m = importlib . import_module (\n    'x')\n",
		"bare import_module":         "m = import_module('x')\n",
		"bare dunder import":         "def load(n):\n    return __import__(n)\n",
		"module alias":               "import importlib as il\nm = il.import_module(name)\n",
		"function alias":             "from importlib import import_module as im\nm = im(name)\n",
		"function alias spaced call": "from importlib import import_module as im\nm = im (name)\n",
		"function alias in a list":   "from importlib import util, import_module as im\nm = im(name)\n",
		"function alias parenthesis": "from importlib import (\n    util,\n    import_module as im,  # comment\n)\nm = im(name)\n",
		"function alias used first":  "def load(n):\n    return im(n)\n\nfrom importlib import import_module as im\n",
		"function alias one-liner":   "from importlib import import_module as im; m = im('x')\n",
		"function alias in a try":    "try: from importlib import import_module as im\nexcept ImportError: raise\nm = im('x')\n",
		"function alias in a def":    "def load(n):\n    from importlib import import_module as im\n    return im(n)\n",
	} {
		_, dynamic, ok := scanPython([]byte(src))
		if !ok || !dynamic {
			t.Errorf("%s: dynamic import not recorded for %q (dynamic=%v ok=%v)", name, src, dynamic, ok)
		}
	}
	for _, src := range []string{
		"# importlib.import_module('x')\n",
		"s = 'importlib.import_module(x)'\n",
		"x = my_import_modules_total\n",
		"import importlib\n",
		"from importlib import import_module as im\n",
		"from importlib import util as im\nm = im(name)\n",
		"from other import import_module as im\nm = im(name)\n",
		"def im(n):\n    return n\n",
		"import importlib as il\nvalue = il.machinery\n",
	} {
		_, dynamic, ok := scanPython([]byte(src))
		if !ok || dynamic {
			t.Errorf("false dynamic import for %q (dynamic=%v ok=%v)", src, dynamic, ok)
		}
	}
}

// scanRecovering calls scanPython and reports a panic as a test failure, so
// that one bad input does not hide the verdict on the others.
func scanRecovering(t *testing.T, name string, src string) (imports []pyImport, dynamic, ok bool) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("%s: scanPython panicked on %q: %v", name, src, r)
		}
	}()
	return scanPython([]byte(src))
}

func TestPythonScanNonASCIIAliases(t *testing.T) {
	const bind = "from importlib import import_module as "
	for _, tc := range []struct {
		name string
		src  string
		want bool // dynamic
	}{
		// Source that is not valid UTF-8 (a Latin-1 file with a coding cookie)
		// cannot be matched by identifier, so the module is dynamic.
		{"latin-1 alias", bind + "\xe9\nm = \xe9('x')\n", true},
		{"latin-1 alias never called", bind + "\xe9\n", true},
		{"latin-1 module alias", "import importlib as \xe9\nm = \xe9.import_module(n)\n", true},
		{"latin-1 bytes in a comment", "# caf\xe9\nx = 1\n", true},
		{"latin-1 bytes in a string", "x = 'caf\xe9'\n", true},
		{"truncated utf-8 sequence", "x = 1\n# \xe2\x82\n", true},
		// Valid non-ASCII identifiers.
		{"accented alias", bind + "\u00e9\nm = \u00e9('x')\n", true},
		{"accented alias, spaced call", bind + "\u00e9\nm = \u00e9 ('x')\n", true},
		{"cyrillic alias", bind + "\u0438\u043c\nm = \u0438\u043c(n)\n", true},
		{"cjk alias", bind + "\u5bfc\u5165\nm = \u5bfc\u5165(n)\n", true},
		{"alias with digit and underscore", bind + "_\u00e92\nm = _\u00e92(n)\n", true},
		{"decomposed alias", bind + "e\u0301\nm = e\u0301(n)\n", true},
		{"accented alias used before binding", "def load(n):\n    return \u00e9(n)\n" + bind + "\u00e9\n", true},
		{"accented alias in a parenthesized list", "from importlib import (\n    util,\n    import_module as \u00e9,\n)\nm = \u00e9(n)\n", true},
		{"accented module alias", "import importlib as \u00e9\nm = \u00e9.import_module(n)\n", true},
		{"accented module alias, spaced attribute", "import importlib as \u00e9\nm = \u00e9 . import_module (n)\n", true},
		// Identifier boundaries use Unicode letters, digits and underscore.
		{"accented alias never called", bind + "\u00e9\nx = 1\n", false},
		{"accented alias is only an attribute name prefix", bind + "\u00e9\nx = \u00e9\u00e9(n)\n", false},
		{"ascii alias inside a longer accented name", bind + "im\nm = \u00e9im(n)\n", false},
		{"ascii alias before an accented letter", bind + "im\nm = im\u00e9(n)\n", false},
		{"ascii alias before a digit", bind + "im\nm = im2(n)\n", false},
		{"ascii alias after an underscore", bind + "im\nm = _im(n)\n", false},
		{"valid utf-8 elsewhere", "x = '\u00e9'  # \u00e9\ny = 1\n", false},
	} {
		_, dynamic, ok := scanRecovering(t, tc.name, tc.src)
		if !ok || dynamic != tc.want {
			t.Errorf("%s: dynamic = %v ok = %v, want dynamic %v for %q", tc.name, dynamic, ok, tc.want, tc.src)
		}
	}
}

func TestPythonScanInvalidUTF8KeepsImports(t *testing.T) {
	src := "import a\n# caf\xe9\nfrom b import c\n"
	got, dynamic, ok := scanRecovering(t, "latin-1 comment", src)
	want := []pyImport{{Module: "a"}, {From: true, Module: "b", Names: []string{"c"}}}
	if !ok || !dynamic || !reflect.DeepEqual(got, want) {
		t.Errorf("scan = %+v dynamic=%v ok=%v, want %+v dynamic=true ok=true", got, dynamic, ok, want)
	}
}

func TestPythonScanStripsLeadingBOM(t *testing.T) {
	const bom = "\xef\xbb\xbf"
	for _, tc := range []struct {
		name string
		src  string
		want []pyImport
	}{
		{"import", bom + "import a\nimport b\n", []pyImport{{Module: "a"}, {Module: "b"}}},
		{"from import", bom + "from a import b\n", []pyImport{{From: true, Module: "a", Names: []string{"b"}}}},
		{"crlf", bom + "import a\r\nimport b\r\n", []pyImport{{Module: "a"}, {Module: "b"}}},
	} {
		got, dynamic, ok := scanPython([]byte(tc.src))
		if !ok || dynamic || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: scan = %+v dynamic=%v ok=%v, want %+v", tc.name, got, dynamic, ok, tc.want)
		}
	}
}

func TestPythonScanUnreliableSource(t *testing.T) {
	for name, src := range map[string]string{
		"unbalanced bracket":       "x = foo(1,\nimport a\n",
		"unterminated triple":      "x = '''never closed\nimport a\n",
		"unterminated import list": "from x import (a,\n",
	} {
		if _, _, ok := scanPython([]byte(src)); ok {
			t.Errorf("%s: unreliable source reported as ok", name)
		}
	}
}
