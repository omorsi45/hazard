package graph

import (
	"errors"
	"reflect"
	"testing"
)

func pyTree() map[string]string {
	return map[string]string{
		"pkg/__init__.py":      "",
		"pkg/a.py":             "from . import b\nimport pkg.c\n",
		"pkg/b.py":             "from .c import thing\n",
		"pkg/c.py":             "import os\nimport numpy as np\nthing = 1\n",
		"tests/conftest.py":    "import pkg\n",
		"tests/test_a.py":      "import pkg.a\nfrom pkg import b\nfrom pkg import not_a_module\n",
		"tests/helper_test.py": "from pkg.c import thing\n",
	}
}

func TestPythonLoadModulesAndEdges(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, pyTree())
	g, err := LoadPython(dir, []string{"."})
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]struct {
		dir          string
		files, tests []string
		imports      []string
		testImports  []string
		external     []string
	}{
		"py:pkg":               {dir: "pkg", files: []string{"pkg/__init__.py"}},
		"py:pkg.a":             {dir: "pkg", files: []string{"pkg/a.py"}, imports: []string{"py:pkg", "py:pkg.b", "py:pkg.c"}},
		"py:pkg.b":             {dir: "pkg", files: []string{"pkg/b.py"}, imports: []string{"py:pkg", "py:pkg.c"}},
		"py:pkg.c":             {dir: "pkg", files: []string{"pkg/c.py"}, external: []string{"numpy", "os"}},
		"py:tests.conftest":    {dir: "tests", files: []string{"tests/conftest.py"}, imports: []string{"py:pkg"}},
		"py:tests.test_a":      {dir: "tests", tests: []string{"tests/test_a.py"}, testImports: []string{"py:pkg", "py:pkg.a", "py:pkg.b"}},
		"py:tests.helper_test": {dir: "tests", tests: []string{"tests/helper_test.py"}, testImports: []string{"py:pkg", "py:pkg.c"}},
	} {
		p := g.Packages[id]
		if p == nil {
			t.Fatalf("missing %s in %v", id, keys(g))
		}
		if p.Language != "python" || p.ID != id || p.Dir != want.dir || p.Dynamic {
			t.Errorf("%s: %+v", id, p)
		}
		if !reflect.DeepEqual(p.Files, want.files) || !reflect.DeepEqual(p.TestFiles, want.tests) {
			t.Errorf("%s: files=%v tests=%v, want %v %v", id, p.Files, p.TestFiles, want.files, want.tests)
		}
		if !reflect.DeepEqual(p.Imports, want.imports) || !reflect.DeepEqual(p.TestImports, want.testImports) || !reflect.DeepEqual(p.External, want.external) {
			t.Errorf("%s: imports=%v testImports=%v external=%v, want %v %v %v", id, p.Imports, p.TestImports, p.External, want.imports, want.testImports, want.external)
		}
	}
	if len(g.Packages) != 7 {
		t.Errorf("packages = %v", keys(g))
	}
	if id, ok := g.PackageOfFile("pkg/__init__.py"); !ok || id != "py:pkg" {
		t.Errorf("PackageOfFile(__init__) = %q %v", id, ok)
	}
	hops := g.ReverseHops([]string{"py:pkg.c"})
	if hops["py:tests.test_a"] != 2 || hops["py:tests.helper_test"] != 1 || hops["py:pkg.a"] != 1 {
		t.Errorf("hops = %v", hops)
	}
}

func TestPythonLoadSourceRoots(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"src/lib/__init__.py": "",
		"src/lib/m.py":        "x = 1\n",
		"tests/test_m.py":     "import lib.m\n",
	})
	g, err := LoadPython(dir, []string{".", "src"})
	if err != nil {
		t.Fatal(err)
	}
	if g.Packages["py:lib.m"] == nil || g.Packages["py:src.lib.m"] != nil {
		t.Fatalf("the longest root must name the module: %v", keys(g))
	}
	if got := g.Packages["py:tests.test_m"].TestImports; !reflect.DeepEqual(got, []string{"py:lib", "py:lib.m"}) {
		t.Errorf("test imports = %v", got)
	}
}

func TestPythonLoadSiblingImportWithoutPackage(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"tests/helpers.py":  "VALUE = 1\n",
		"tests/test_sib.py": "import helpers\n",
	})
	g, err := LoadPython(dir, []string{"."})
	if err != nil {
		t.Fatal(err)
	}
	if got := g.Packages["py:tests.test_sib"].TestImports; !reflect.DeepEqual(got, []string{"py:tests.helpers"}) {
		t.Errorf("sibling import of a directory without __init__.py lost: %v", got)
	}
}

func TestPythonLoadDynamicImport(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"app/__init__.py": "",
		"app/plugins.py":  "import importlib\n\ndef load(n):\n    return importlib.import_module(n)\n",
		"app/plain.py":    "VALUE = 1\n",
	})
	g, err := LoadPython(dir, []string{"."})
	if err != nil {
		t.Fatal(err)
	}
	if !g.Packages["py:app.plugins"].Dynamic || g.Packages["py:app.plain"].Dynamic || g.Packages["py:app"].Dynamic {
		t.Errorf("dynamic flags wrong: %+v %+v %+v", g.Packages["py:app.plugins"], g.Packages["py:app.plain"], g.Packages["py:app"])
	}
}

func TestPythonLoadUnresolvableRelativeImport(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"pkg/__init__.py": "",
		"pkg/ok.py":       "from . import sibling\n",
		"pkg/sibling.py":  "",
		"pkg/missing.py":  "from .nowhere import x\n",
		"pkg/toohigh.py":  "from .. import y\n",
		"loose.py":        "from . import z\n",
		"broken.py":       "x = foo(1,\n",
	})
	g, err := LoadPython(dir, []string{"."})
	var unresolved *UnresolvedError
	if !errors.As(err, &unresolved) || g == nil {
		t.Fatalf("want a graph with *UnresolvedError, got graph=%v err=%v", g, err)
	}
	for _, id := range []string{"py:pkg.missing", "py:pkg.toohigh", "py:loose", "py:broken"} {
		if _, bad := unresolved.Modules[id]; !bad || !g.Packages[id].Dynamic {
			t.Errorf("%s must be reported and marked unknown: %v dynamic=%v", id, unresolved.Modules, g.Packages[id].Dynamic)
		}
	}
	if _, bad := unresolved.Modules["py:pkg.ok"]; bad || g.Packages["py:pkg.ok"].Dynamic {
		t.Errorf("resolvable relative import flagged: %v", unresolved.Modules)
	}
	if !reflect.DeepEqual(g.Packages["py:pkg.ok"].Imports, []string{"py:pkg", "py:pkg.sibling"}) {
		t.Errorf("ok imports = %v", g.Packages["py:pkg.ok"].Imports)
	}
}

func TestPythonLoadSkipsEnvironmentDirectories(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"app/real.py":                         "x = 1\n",
		".venv/lib/site.py":                   "x = 1\n",
		"node_modules/pkg/tool.py":            "x = 1\n",
		"app/__pycache__/real.cpython-312.py": "x = 1\n",
		"envdir/pyvenv.cfg":                   "home = /usr\n",
		"envdir/lib/other.py":                 "x = 1\n",
		"vendor/lib/site-packages/dep.py":     "x = 1\n",
		".hazard/index/stray.py":              "x = 1\n",
	})
	g, err := LoadPython(dir, []string{"."})
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Packages) != 1 || g.Packages["py:app.real"] == nil {
		t.Fatalf("environment directories must be skipped, got %v", keys(g))
	}
}

func TestPythonLoadRootErrors(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"a.py": ""})
	for _, roots := range [][]string{{"missing"}, {".."}, {"a.py"}, {"/abs"}} {
		if g, err := LoadPython(dir, roots); err == nil || g != nil {
			t.Errorf("roots %v: want error, got graph=%v err=%v", roots, g, err)
		}
	}
	g, err := LoadPython(dir, nil)
	if err != nil || g.Packages["py:a"] == nil {
		t.Errorf("no roots must default to the repository root: %v %v", g, err)
	}
}
