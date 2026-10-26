package mutate

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// lines joins source lines; the nth entry is on line n+1.
func lines(l ...string) string { return strings.Join(l, "\n") + "\n" }

// cfgSpots describes the cfg mutants replacing orig, found as within inside ctx.
func cfgSpots(ctx, within, orig string, line int, muts ...string) []spot {
	var out []spot
	for _, m := range muts {
		out = append(out, spot{ctx: ctx, within: within, op: "cfg", orig: orig, mut: m, line: line})
	}
	return out
}

func join(groups ...[]spot) []spot {
	var out []spot
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

// checkConfig asserts that ConfigMutants returns exactly the described spots
// and that every mutant is consistent and applies cleanly.
func checkConfig(t *testing.T, file, src string, spots []spot) []Mutant {
	t.Helper()
	got := ConfigMutants(file, []byte(src))
	var want, have []view
	for _, s := range spots {
		want = append(want, view{locate(t, src, s.ctx, s.within, s.nth), s.op, s.orig, s.mut, s.line})
	}
	sortViews(want)
	for _, m := range got {
		have = append(have, view{m.Offset, m.Operator, m.Original, m.Mutated, m.Line})
	}
	if !reflect.DeepEqual(have, want) {
		t.Fatalf("mutants differ\ngot:\n%swant:\n%s", viewList(have), viewList(want))
	}
	wantFile := strings.ReplaceAll(file, `\`, "/")
	for _, m := range got {
		if m.File != wantFile {
			t.Errorf("%v: File = %q, want %q", m, m.File, wantFile)
		}
		if m.ID != makeID(wantFile, m.Offset, m.Operator, m.Mutated) {
			t.Errorf("%v: ID = %q does not match its components", m, m.ID)
		}
		if m.Length != len(m.Original) || src[m.Offset:m.Offset+m.Length] != m.Original {
			t.Errorf("%v: span does not hold Original", m)
		}
		out, err := Apply([]byte(src), m)
		if err != nil {
			t.Errorf("%v: Apply: %v", m, err)
			continue
		}
		if string(out[:m.Offset]) != src[:m.Offset] || string(out[m.Offset+len(m.Mutated):]) != src[m.Offset+m.Length:] {
			t.Errorf("%v: Apply changed bytes outside the value", m)
		}
	}
	return got
}

func TestConfigYAML(t *testing.T) {
	src := lines(
		/* 1 */ "name: demo",
		/* 2 */ "port: 8080",
		/* 3 */ "ratio: 1.5",
		/* 4 */ "debug: true",
		/* 5 */ "enabled: False",
		/* 6 */ `title: "Hello"`,
		/* 7 */ "alias: 'single'",
		/* 8 */ `empty: ""`,
		/* 9 */ "nothing: null",
		/* 10 */ "tilde: ~",
		/* 11 */ "# comment: 5",
		/* 12 */ "list:",
		/* 13 */ "  - a",
		/* 14 */ "  - 5",
		/* 15 */ "  - key: 7",
		/* 16 */ "nested:",
		/* 17 */ "  deep: 3",
		/* 18 */ "  inline: [1, 2]",
		/* 19 */ "  map: {a: 1}",
		/* 20 */ "text: |",
		/* 21 */ "  not: 5",
		/* 22 */ "  more: true",
		/* 23 */ "after: 9",
		/* 24 */ "folded: >-",
		/* 25 */ "  x: 1",
		/* 26 */ "last: yes # comment",
		/* 27 */ `quoted: "a # b" # c`,
		/* 28 */ "url: http://host:80/path",
		/* 29 */ "neg: -3",
		/* 30 */ "---",
		/* 31 */ "multi: [",
		/* 32 */ "  1,",
		/* 33 */ "  a: 2",
		/* 34 */ "]",
		/* 35 */ "end: 1",
	)
	checkConfig(t, "conf/app.yaml", src, join(
		cfgSpots("name: demo", "demo", "demo", 1, `""`),
		cfgSpots("port: 8080", "8080", "8080", 2, "8081", "0"),
		cfgSpots("ratio: 1.5", "1.5", "1.5", 3, "3.0"),
		cfgSpots("debug: true", "true", "true", 4, "false"),
		cfgSpots("enabled: False", "False", "False", 5, "True"),
		cfgSpots(`title: "Hello"`, `"Hello"`, `"Hello"`, 6, `""`),
		cfgSpots("alias: 'single'", "'single'", "'single'", 7, `""`),
		cfgSpots("  deep: 3", "3", "3", 17, "4", "0"),
		cfgSpots("after: 9", "9", "9", 23, "10", "0"),
		cfgSpots("last: yes", "yes", "yes", 26, `""`),
		cfgSpots(`quoted: "a # b"`, `"a # b"`, `"a # b"`, 27, `""`),
		cfgSpots("url: http://host:80/path", "http://host:80/path", "http://host:80/path", 28, `""`),
		cfgSpots("neg: -3", "-3", "-3", 29, "-2", "0"),
		cfgSpots("end: 1", "1", "1", 35, "2", "0"),
	))
}

func TestConfigYAMLVariants(t *testing.T) {
	t.Run("both extensions", func(t *testing.T) {
		for _, f := range []string{"a.yaml", "a.yml", "A.YML", `dir\a.yaml`} {
			checkConfig(t, f, "a: 1\n", cfgSpots("a: 1", "1", "1", 1, "2", "0"))
		}
	})
	t.Run("zero has no zero mutant", func(t *testing.T) {
		checkConfig(t, "a.yaml", "a: 0\n", cfgSpots("a: 0", "0", "0", 1, "1"))
	})
	t.Run("floats", func(t *testing.T) {
		checkConfig(t, "a.yaml", lines("a: 1e3", "b: -1.5", "c: .5", "d: 1.", "e: 0.0", "f: 1e308", "g: 2.5e-3"), join(
			cfgSpots("a: 1e3", "1e3", "1e3", 1, "2000.0"),
			cfgSpots("b: -1.5", "-1.5", "-1.5", 2, "-3.0"),
			cfgSpots("c: .5", ".5", ".5", 3, "1.0"),
			cfgSpots("d: 1.", "1.", "1.", 4, "2.0"),
			cfgSpots("g: 2.5e-3", "2.5e-3", "2.5e-3", 7, "0.005"),
		))
	})
	t.Run("boolean case is preserved", func(t *testing.T) {
		checkConfig(t, "a.yaml", lines("a: true", "b: True", "c: TRUE", "d: false", "e: False", "f: FALSE"), join(
			cfgSpots("a: true", "true", "true", 1, "false"),
			cfgSpots("b: True", "True", "True", 2, "False"),
			cfgSpots("c: TRUE", "TRUE", "TRUE", 3, "FALSE"),
			cfgSpots("d: false", "false", "false", 4, "true"),
			cfgSpots("e: False", "False", "False", 5, "True"),
			cfgSpots("f: FALSE", "FALSE", "FALSE", 6, "TRUE"),
		))
	})
	t.Run("keys are never mutated", func(t *testing.T) {
		checkConfig(t, "a.yaml", lines("1: 2", "true: false", `"k": 'v'`), join(
			cfgSpots("1: 2", "2", "2", 1, "3", "0"),
			cfgSpots("true: false", "false", "false", 2, "true"),
			cfgSpots(`"k": 'v'`, "'v'", "'v'", 3, `""`),
		))
	})
	t.Run("hash without a preceding space belongs to the value", func(t *testing.T) {
		checkConfig(t, "a.yaml", "a: b#c\n", cfgSpots("a: b#c", "b#c", "b#c", 1, `""`))
	})
	t.Run("anchors aliases tags and special floats", func(t *testing.T) {
		src := lines("a: &x 5", "b: *x", "c: !!str 5", "d: .inf", "e: -.INF", "f: .NaN", "g: Null", "h: NULL")
		checkConfig(t, "a.yaml", src, nil)
	})
	t.Run("document markers directives and bare keys", func(t *testing.T) {
		checkConfig(t, "a.yaml", lines("%YAML 1.2", "---", "key:", "...", "- 3", "-", "just text", "? complex"), nil)
	})
	t.Run("multi-line quoted scalar", func(t *testing.T) {
		src := lines(`a: "first`, "  b: 5", `  end" `, "c: 1")
		checkConfig(t, "a.yaml", src, cfgSpots("c: 1", "1", "1", 4, "2", "0"))
	})
	t.Run("carriage returns are not part of the value", func(t *testing.T) {
		src := "a: 1\r\nb: true\r\nc: text   \r\nd: \"q\"\r\n"
		checkConfig(t, "a.yaml", src, join(
			cfgSpots("a: 1", "1", "1", 1, "2", "0"),
			cfgSpots("b: true", "true", "true", 2, "false"),
			cfgSpots("c: text", "text", "text", 3, `""`),
			cfgSpots(`d: "q"`, `"q"`, `"q"`, 4, `""`),
		))
	})
	t.Run("no trailing newline", func(t *testing.T) {
		checkConfig(t, "a.yaml", "a: 1", cfgSpots("a: 1", "1", "1", 1, "2", "0"))
	})
}

func TestConfigTOML(t *testing.T) {
	src := lines(
		/* 1 */ "# settings",
		/* 2 */ `title = "TOML Example"`,
		/* 3 */ "count = 5",
		/* 4 */ "pi = 3.14",
		/* 5 */ "enabled = true",
		/* 6 */ "when = 1979-05-27",
		/* 7 */ "raw = 'literal'",
		/* 8 */ `empty = ""`,
		/* 9 */ "hex = 0xFF",
		/* 10 */ "big = 1_000",
		/* 11 */ "",
		/* 12 */ "[server]",
		/* 13 */ `host = "localhost"   # trailing comment`,
		/* 14 */ "ports = [8000, 8001]",
		/* 15 */ "multi = [",
		/* 16 */ "  1,",
		/* 17 */ "  2,",
		/* 18 */ "]",
		/* 19 */ `doc = """`,
		/* 20 */ "line = 5",
		/* 21 */ `"""`,
		/* 22 */ "inline = { a = 1 }",
		/* 23 */ "[[items]]",
		/* 24 */ "id = 7",
		/* 25 */ `"quoted key" = false`,
	)
	checkConfig(t, "Cargo.toml", src, join(
		cfgSpots(`title = "TOML Example"`, `"TOML Example"`, `"TOML Example"`, 2, `""`),
		cfgSpots("count = 5", "5", "5", 3, "6", "0"),
		cfgSpots("pi = 3.14", "3.14", "3.14", 4, "6.28"),
		cfgSpots("enabled = true", "true", "true", 5, "false"),
		cfgSpots("raw = 'literal'", "'literal'", "'literal'", 7, `""`),
		cfgSpots(`host = "localhost"`, `"localhost"`, `"localhost"`, 13, `""`),
		cfgSpots("id = 7", "7", "7", 24, "8", "0"),
		cfgSpots(`"quoted key" = false`, "false", "false", 25, "true"),
	))
}

func TestConfigTOMLSingleLineMultilineString(t *testing.T) {
	checkConfig(t, "a.toml", lines(`a = """one line"""`, "b = 2"), cfgSpots("b = 2", "2", "2", 2, "3", "0"))
}

func TestConfigINI(t *testing.T) {
	src := lines(
		/* 1 */ "; comment",
		/* 2 */ "# another",
		/* 3 */ "[database]",
		/* 4 */ "host = localhost",
		/* 5 */ "port = 5432",
		/* 6 */ "timeout: 30",
		/* 7 */ "ratio = 0.25",
		/* 8 */ "ssl = off",
		/* 9 */ "debug = TRUE",
		/* 10 */ `name = "quoted"   ; trailing`,
		/* 11 */ "empty =",
		/* 12 */ "novalue",
		/* 13 */ "url = http://example.com:8080/x",
		/* 14 */ "[section two]",
		/* 15 */ "key=1",
	)
	checkConfig(t, "php.ini", src, join(
		cfgSpots("host = localhost", "localhost", "localhost", 4, `""`),
		cfgSpots("port = 5432", "5432", "5432", 5, "5433", "0"),
		cfgSpots("timeout: 30", "30", "30", 6, "31", "0"),
		cfgSpots("ratio = 0.25", "0.25", "0.25", 7, "0.5"),
		cfgSpots("ssl = off", "off", "off", 8, `""`),
		cfgSpots("debug = TRUE", "TRUE", "TRUE", 9, "FALSE"),
		cfgSpots(`name = "quoted"`, `"quoted"`, `"quoted"`, 10, `""`),
		cfgSpots("url = http://example.com:8080/x", "http://example.com:8080/x", "http://example.com:8080/x", 13, `""`),
		cfgSpots("key=1", "1", "1", 15, "2", "0"),
	))
}

func TestConfigEnv(t *testing.T) {
	src := lines(
		/* 1 */ "# comment",
		/* 2 */ "APP_ENV=production",
		/* 3 */ "PORT=3000",
		/* 4 */ "export DEBUG=true",
		/* 5 */ `SECRET="s3cr3t value"`,
		/* 6 */ "EMPTY=",
		/* 7 */ `QUOTED_EMPTY=""`,
		/* 8 */ "RATIO=0.5",
		/* 9 */ "NAME=hello world # note",
		/* 10 */ "not a key=1",
		/* 11 */ "KEY.WITH-DOT=7",
		/* 12 */ `MULTI="line one`,
		/* 13 */ "PORT=99",
		/* 14 */ `end"`,
		/* 15 */ "AFTER=2",
	)
	for _, f := range []string{".env", "prod.env", "cfg/.env"} {
		checkConfig(t, f, src, join(
			cfgSpots("APP_ENV=production", "production", "production", 2, `""`),
			cfgSpots("PORT=3000", "3000", "3000", 3, "3001", "0"),
			cfgSpots("export DEBUG=true", "true", "true", 4, "false"),
			cfgSpots(`SECRET="s3cr3t value"`, `"s3cr3t value"`, `"s3cr3t value"`, 5, `""`),
			cfgSpots("RATIO=0.5", "0.5", "0.5", 8, "1.0"),
			cfgSpots("NAME=hello world", "hello world", "hello world", 9, `""`),
			cfgSpots("KEY.WITH-DOT=7", "7", "7", 11, "8", "0"),
			cfgSpots("AFTER=2", "2", "2", 15, "3", "0"),
		))
	}
}

func TestConfigProperties(t *testing.T) {
	src := lines(
		/* 1 */ "# comment",
		/* 2 */ "! another",
		/* 3 */ "app.name=demo",
		/* 4 */ "app.port = 8080",
		/* 5 */ "app.url: http://x/y",
		/* 6 */ "app.flag=true",
		/* 7 */ "app.ratio=2.5",
		/* 8 */ "empty=",
		/* 9 */ `long.value=abc \`,
		/* 10 */ "  def=5",
		/* 11 */ "after=1",
		/* 12 */ `esc\=key=7`,
		/* 13 */ "nosep",
		/* 14 */ "hash=a # b",
		/* 15 */ `k=""`,
		/* 16 */ `k = ""`,
		/* 17 */ `k: ""`,
	)
	// Lines 15 to 17 hold a value that already is two double quote characters.
	// Properties has no quoting, so the value would be "emptied" to the same
	// bytes: no mutant may be returned for them.
	checkConfig(t, "app.properties", src, join(
		cfgSpots("app.name=demo", "demo", "demo", 3, `""`),
		cfgSpots("app.port = 8080", "8080", "8080", 4, "8081", "0"),
		cfgSpots("app.url: http://x/y", "http://x/y", "http://x/y", 5, `""`),
		cfgSpots("app.flag=true", "true", "true", 6, "false"),
		cfgSpots("app.ratio=2.5", "2.5", "2.5", 7, "5.0"),
		cfgSpots("after=1", "1", "1", 11, "2", "0"),
		cfgSpots(`esc\=key=7`, "7", "7", 12, "8", "0"),
		cfgSpots("hash=a # b", "a # b", "a # b", 14, `""`),
	))
}

func TestConfigJSON(t *testing.T) {
	src := lines(
		/* 1 */ "{",
		/* 2 */ `  "name": "demo",`,
		/* 3 */ `  "port": 8080,`,
		/* 4 */ `  "ratio": 1.5,`,
		/* 5 */ `  "debug": true,`,
		/* 6 */ `  "nothing": null,`,
		/* 7 */ `  "empty": "",`,
		/* 8 */ `  "list": [1, 2],`,
		/* 9 */ `  "multi": [`,
		/* 10 */ `    "a",`,
		/* 11 */ `    {`,
		/* 12 */ `      "inner": 5`,
		/* 13 */ `    }`,
		/* 14 */ `  ],`,
		/* 15 */ `  "nested": {"x": 1},`,
		/* 16 */ `  "neg": -2,`,
		/* 17 */ `  "esc": "say \"hi\": ok",`,
		/* 18 */ `  "last": false`,
		/* 19 */ "}",
	)
	checkConfig(t, "package.json", src, join(
		cfgSpots(`"name": "demo"`, `"demo"`, `"demo"`, 2, `""`),
		cfgSpots(`"port": 8080`, "8080", "8080", 3, "8081", "0"),
		cfgSpots(`"ratio": 1.5`, "1.5", "1.5", 4, "3.0"),
		cfgSpots(`"debug": true`, "true", "true", 5, "false"),
		cfgSpots(`"inner": 5`, "5", "5", 12, "6", "0"),
		cfgSpots(`"neg": -2`, "-2", "-2", 16, "-1", "0"),
		cfgSpots(`"esc": "say \"hi\": ok"`, `"say \"hi\": ok"`, `"say \"hi\": ok"`, 17, `""`),
		cfgSpots(`"last": false`, "false", "false", 18, "true"),
	))
}

func TestConfigIgnoresOtherFiles(t *testing.T) {
	for _, f := range []string{"main.go", "README.md", "notes.txt", "Makefile", "a.json5", "a.yaml.bak", "env", "", "a.yaml/"} {
		if got := ConfigMutants(f, []byte("a: 1\nb = 2\nc=3\n")); got != nil {
			t.Errorf("ConfigMutants(%q) = %v, want none", f, got)
		}
	}
	if got := ConfigMutants("a.yaml", nil); got != nil {
		t.Errorf("empty file: got %v, want none", got)
	}
	if got := ConfigMutants("a.yaml", []byte("\n\n   \n")); got != nil {
		t.Errorf("blank file: got %v, want none", got)
	}
}

func TestConfigDeterministicAndUnique(t *testing.T) {
	// The properties input holds values that are already two double quotes:
	// they must not yield a mutant that replaces the bytes with themselves.
	for _, in := range []struct{ file, src string }{
		{"a.yaml", lines("a: 1", "b: true", "c: text", "d: 2.5")},
		{"a.properties", lines("a=1", "b=true", `c=""`, "d=text", "e = 2.5")},
	} {
		t.Run(in.file, func(t *testing.T) { checkDeterministicAndUnique(t, in.file, []byte(in.src)) })
	}
}

func checkDeterministicAndUnique(t *testing.T, file string, src []byte) {
	t.Helper()
	first := ConfigMutants(file, src)
	if len(first) == 0 {
		t.Fatal("expected mutants")
	}
	for i := 0; i < 5; i++ {
		if again := ConfigMutants(file, src); !reflect.DeepEqual(first, again) {
			t.Fatalf("run %d differs from the first run", i)
		}
	}
	seen := map[string]bool{}
	for i, m := range first {
		if seen[m.ID] {
			t.Errorf("duplicate ID %s", m.ID)
		}
		seen[m.ID] = true
		if m.Operator != "cfg" {
			t.Errorf("%v: operator %q, want cfg", m, m.Operator)
		}
		if m.Original == m.Mutated {
			t.Errorf("%v: replaces %q with itself", m, m.Original)
		}
		if i > 0 && (first[i-1].Offset > m.Offset || (first[i-1].Offset == m.Offset && first[i-1].Mutated > m.Mutated)) {
			t.Errorf("mutants %d and %d are out of order", i-1, i)
		}
	}
}

// TestRepositoryConfig mutates every configuration file in the repository and
// checks that each mutant applies and, for json, still decodes.
func TestRepositoryConfig(t *testing.T) {
	root := repoRoot(t)
	idFile := map[string]string{}
	files, total := 0, 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); p != root && strings.HasPrefix(name, ".") && name != ".github" {
				return filepath.SkipDir // .git, .hazard and other tool directories
			}
			return nil
		}
		if cfgFormatOf(p) == cfgNone {
			return nil
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		files++
		muts := ConfigMutants(rel, src)
		if again := ConfigMutants(rel, src); !reflect.DeepEqual(muts, again) {
			t.Errorf("%s: a second run gives a different answer", rel)
		}
		validJSON := strings.HasSuffix(rel, ".json") && json.Valid(src)
		for _, m := range muts {
			total++
			if other, dup := idFile[m.ID]; dup {
				t.Errorf("%s: mutant ID %s already used by %s", rel, m.ID, other)
			}
			idFile[m.ID] = rel
			if m.Original == m.Mutated {
				t.Errorf("%s: mutant %s replaces %q with itself at line %d", rel, m.ID, m.Original, m.Line)
			}
			out, err := Apply(src, m)
			if err != nil {
				t.Errorf("%s: Apply %+v: %v", rel, m, err)
				continue
			}
			if validJSON && !json.Valid(out) {
				t.Errorf("%s: mutant %s (%q -> %q at line %d) breaks the json", rel, m.ID, m.Original, m.Mutated, m.Line)
			}
		}
		t.Logf("%-40s %4d cfg mutants", rel, len(muts))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files == 0 {
		t.Fatal("no configuration files found in the repository")
	}
	t.Logf("total %d cfg mutants in %d files", total, files)
}
