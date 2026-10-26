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
	for _, src := range []string{
		"import importlib\nmod = importlib.import_module('x')\n",
		"from importlib import import_module\nm = import_module(name)\n",
		"m = __import__('x')\n",
		"m = importlib . import_module (\n    'x')\n",
	} {
		_, dynamic, ok := scanPython([]byte(src))
		if !ok || !dynamic {
			t.Errorf("dynamic import not recorded for %q (dynamic=%v ok=%v)", src, dynamic, ok)
		}
	}
	for _, src := range []string{
		"# importlib.import_module('x')\n",
		"s = 'importlib.import_module(x)'\n",
		"x = my_import_modules_total\n",
		"import importlib\n",
	} {
		_, dynamic, ok := scanPython([]byte(src))
		if !ok || dynamic {
			t.Errorf("false dynamic import for %q (dynamic=%v ok=%v)", src, dynamic, ok)
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
