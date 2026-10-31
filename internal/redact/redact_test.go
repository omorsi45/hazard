package redact

import (
	"encoding/json"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/omorsi45/hazard/internal/change"
	"github.com/omorsi45/hazard/internal/config"
	"github.com/omorsi45/hazard/internal/impact"
)

const (
	sentinel = "SECRET_SENTINEL_42"
	literal  = "hunter2-literal-value"
)

func privacy(level string, hunkText bool) config.Privacy {
	return config.Privacy{Redact: level, SendHunkText: hunkText, SendSymbolNames: true, SendPaths: true, AllowExternalPredictor: true}
}

// sampleChange has one Go source file whose hunk text carries a sentinel in a
// comment, a string literal and a non-declared identifier, plus one config
// file outside every declaration.
func sampleChange() *change.ChangeSet {
	goText := "-\tif t.Expiry > now {\n" +
		"-\t\tgrace := 0\n" +
		"+\tif t.Expiry >= now {\n" +
		"+\t\tgrace := 30\n" +
		"+\t\tapiKey := \"" + literal + "\" // " + sentinel + "\n"
	return &change.ChangeSet{
		BaseSHA: "a1b2c3d4e5f6", HeadSHA: "e4f5a6b7c8d9",
		LinesAdded: 5, LinesRemoved: 2,
		Files: []change.ChangedFile{
			{
				Path: "internal/auth/token.go", Kind: change.KindSource, Status: "M", Language: "go",
				Package: "example.com/x/internal/auth",
				Hunks:   []change.Hunk{{OldStart: 10, OldLines: 2, NewStart: 10, NewLines: 3, Symbols: []string{"RefreshToken"}, Text: goText}},
			},
			{
				Path: "config/defaults.yaml", Kind: change.KindConfig, Status: "M",
				Hunks: []change.Hunk{{OldStart: 3, OldLines: 1, NewStart: 3, NewLines: 1, Symbols: []string{"__file__"}, Text: "-token_grace_seconds: 0\n+token_grace_seconds: 30\n"}},
			},
		},
	}
}

// buildChange calls BuildChange and fails the test, instead of letting a
// later index panic, when the file count is not the one the input implies.
func buildChange(t *testing.T, ch *change.ChangeSet, p config.Privacy) Change {
	t.Helper()
	got := BuildChange(ch, p)
	want := 0
	if ch != nil {
		want = len(ch.Files)
	}
	if len(got.Files) != want {
		t.Fatalf("BuildChange returned %d files, want %d", len(got.Files), want)
	}
	return got
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestBuildChangeNone(t *testing.T) {
	got := buildChange(t, sampleChange(), privacy("none", false))
	if got.Base != "a1b2c3d" || got.Head != "e4f5a6b" || got.LinesAdded != 5 || got.LinesRemoved != 2 {
		t.Fatalf("header = %+v", got)
	}
	if len(got.Files) != 2 {
		t.Fatalf("files = %d, want 2", len(got.Files))
	}
	f := got.Files[0]
	if f.Path != "internal/auth/token.go" || f.Kind != "source" || f.Status != "M" || f.Package != "example.com/x/internal/auth" {
		t.Fatalf("file = %+v", f)
	}
	if !reflect.DeepEqual(f.Symbols, []string{"RefreshToken"}) {
		t.Fatalf("symbols = %v", f.Symbols)
	}
	if f.HunkText != "" {
		t.Fatalf("hunk text sent with send_hunk_text false: %q", f.HunkText)
	}
	if want := "3 lines changed in RefreshToken: comparison operator changed, literal changed"; f.HunkSummary != want {
		t.Fatalf("summary = %q, want %q", f.HunkSummary, want)
	}
}

func TestBuildChangeNoneSendsHunkTextOnlyWhenAsked(t *testing.T) {
	got := buildChange(t, sampleChange(), privacy("none", true))
	text := got.Files[0].HunkText
	if !strings.Contains(text, sentinel) || !strings.Contains(text, literal) {
		t.Fatalf("hunk text missing at none with send_hunk_text: %q", text)
	}
	if got.Files[1].HunkText == "" {
		t.Fatal("config file hunk text missing at none with send_hunk_text")
	}
}

func TestBuildChangeStructureNeverSendsSourceText(t *testing.T) {
	for _, sendText := range []bool{false, true} {
		got := buildChange(t, sampleChange(), privacy("structure", sendText))
		js := mustJSON(t, got)
		for _, banned := range []string{sentinel, literal, "apiKey", "grace", "Expiry", "now", "hunk_text", "token_grace_seconds"} {
			if strings.Contains(js, banned) {
				t.Fatalf("send_hunk_text=%v: structure payload leaks %q: %s", sendText, banned, js)
			}
		}
		for _, f := range got.Files {
			if f.HunkText != "" {
				t.Fatalf("hunk text at structure: %q", f.HunkText)
			}
		}
		if got.Files[0].Path != "internal/auth/token.go" || !reflect.DeepEqual(got.Files[0].Symbols, []string{"RefreshToken"}) {
			t.Fatalf("structure must keep paths and declared symbols: %+v", got.Files[0])
		}
	}
}

var tokenPattern = regexp.MustCompile(`^h_[0-9a-f]{16}$`)

func TestBuildChangeHashReplacesNames(t *testing.T) {
	got := buildChange(t, sampleChange(), privacy("hash", true))
	js := mustJSON(t, got)
	for _, banned := range []string{sentinel, literal, "token.go", "auth", "RefreshToken", "example.com", "defaults.yaml", "config/"} {
		if strings.Contains(js, banned) {
			t.Fatalf("hash payload leaks %q: %s", banned, js)
		}
	}
	f := got.Files[0]
	if !tokenPattern.MatchString(f.Path) || !tokenPattern.MatchString(f.Package) {
		t.Fatalf("path %q or package %q is not a token", f.Path, f.Package)
	}
	if len(f.Symbols) != 1 || !tokenPattern.MatchString(f.Symbols[0]) {
		t.Fatalf("symbols = %v", f.Symbols)
	}
	if got.Files[1].Symbols[0] != "__file__" {
		t.Fatalf("the synthetic symbol must stay readable: %v", got.Files[1].Symbols)
	}
	again := buildChange(t, sampleChange(), privacy("hash", false))
	if !reflect.DeepEqual(got.Files[0].Path, again.Files[0].Path) || got.Files[0].Path == got.Files[1].Path {
		t.Fatalf("tokens must be deterministic and distinct per input: %q %q %q", got.Files[0].Path, again.Files[0].Path, got.Files[1].Path)
	}
	if f.Kind != "source" || f.Status != "M" {
		t.Fatalf("kind and status are not names: %+v", f)
	}
}

func TestBuildChangeFailsClosedOnUnknownLevel(t *testing.T) {
	for _, level := range []string{"", "bogus", "None", "STRUCTURE"} {
		got := buildChange(t, sampleChange(), privacy(level, true))
		if !tokenPattern.MatchString(got.Files[0].Path) {
			t.Fatalf("level %q: path %q was not hashed", level, got.Files[0].Path)
		}
		if got.Files[0].HunkText != "" {
			t.Fatalf("level %q: hunk text sent", level)
		}
	}
}

func TestBuildChangePrivacyFlags(t *testing.T) {
	p := privacy("structure", false)
	p.SendPaths = false
	got := buildChange(t, sampleChange(), p)
	if !tokenPattern.MatchString(got.Files[0].Path) || !tokenPattern.MatchString(got.Files[0].Package) {
		t.Fatalf("send_paths=false must hash path and package: %+v", got.Files[0])
	}
	if !reflect.DeepEqual(got.Files[0].Symbols, []string{"RefreshToken"}) {
		t.Fatalf("symbols must stay readable: %v", got.Files[0].Symbols)
	}

	p = privacy("structure", false)
	p.SendSymbolNames = false
	got = buildChange(t, sampleChange(), p)
	js := mustJSON(t, got)
	if strings.Contains(js, "RefreshToken") {
		t.Fatalf("send_symbol_names=false leaks the symbol: %s", js)
	}
	if got.Files[0].Path != "internal/auth/token.go" {
		t.Fatalf("path must stay readable: %q", got.Files[0].Path)
	}
	if want := "3 lines changed in " + got.Files[0].Symbols[0] + ": comparison operator changed, literal changed"; got.Files[0].HunkSummary != want {
		t.Fatalf("summary = %q, want %q", got.Files[0].HunkSummary, want)
	}
}

func TestBuildChangeNilAndEmpty(t *testing.T) {
	got := buildChange(t, nil, privacy("structure", false))
	if got.Files == nil || len(got.Files) != 0 {
		t.Fatalf("nil change must give an empty, non-nil file list: %+v", got)
	}
	if js := mustJSON(t, got); !strings.Contains(js, `"files":[]`) {
		t.Fatalf("json = %s", js)
	}
}

func TestBuildChangeWireNames(t *testing.T) {
	got := buildChange(t, sampleChange(), privacy("none", true))
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(mustJSON(t, got)), &top); err != nil {
		t.Fatal(err)
	}
	assertKeys(t, "change", top, "base", "head", "lines_added", "lines_removed", "files")
	var files []map[string]json.RawMessage
	if err := json.Unmarshal(top["files"], &files); err != nil {
		t.Fatal(err)
	}
	assertKeys(t, "file", files[0], "path", "kind", "status", "package", "symbols", "hunk_summary", "hunk_text")
	assertKeys(t, "config file", files[1], "path", "kind", "status", "symbols", "hunk_summary", "hunk_text")

	test := BuildTest(sampleCandidate(), privacy("none", false))
	var td map[string]json.RawMessage
	if err := json.Unmarshal([]byte(mustJSON(t, test)), &td); err != nil {
		t.Fatal(err)
	}
	assertKeys(t, "test", td, "name", "package", "purpose", "asserts", "hops_from_change", "covers_changed_lines")
}

func assertKeys(t *testing.T, what string, m map[string]json.RawMessage, want ...string) {
	t.Helper()
	var got []string
	for k := range m {
		got = append(got, k)
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s keys = %v, want %v", what, got, want)
	}
}

func TestSummaryShapes(t *testing.T) {
	hunk := func(old, new int, symbols []string, text string) []change.Hunk {
		return []change.Hunk{{OldStart: 1, OldLines: old, NewStart: 1, NewLines: new, Symbols: symbols, Text: text}}
	}
	cases := []struct {
		name string
		file change.ChangedFile
		want string
	}{
		{"added", change.ChangedFile{Path: "a.go", Status: "A", Language: "go", Hunks: hunk(0, 4, []string{"__file__"}, "")}, "file added, 4 lines"},
		{"deleted", change.ChangedFile{Path: "a.go", Status: "D", Language: "go", Hunks: hunk(6, 0, []string{"Old"}, "")}, "file deleted, 6 lines"},
		{"renamed only", change.ChangedFile{Path: "b.go", OldPath: "a.go", Status: "R", Language: "go"}, "file renamed"},
		{"renamed and edited", change.ChangedFile{Path: "b.go", OldPath: "a.go", Status: "R", Language: "go", Hunks: hunk(1, 1, []string{"Foo"}, "")}, "file renamed; 1 line changed in Foo"},
		{"binary", change.ChangedFile{Path: "a.png", Status: "M", Binary: true}, "binary file modified"},
		{"binary added", change.ChangedFile{Path: "a.png", Status: "A", Binary: true}, "binary file added"},
		{"no hunks", change.ChangedFile{Path: "a.go", Status: "M", Language: "go"}, "file modified without line changes"},
		{"outside declarations", change.ChangedFile{Path: "a.go", Status: "M", Language: "go", Hunks: hunk(2, 2, []string{"__file__"}, "")}, "2 lines changed outside any declaration"},
		{"many symbols", change.ChangedFile{Path: "a.go", Status: "M", Language: "go", Hunks: hunk(1, 1, []string{"A", "B", "C", "D", "E", "F", "G"}, "")}, "1 line changed in A, B, C, D, E and 2 more"},
		{"duplicate symbols across hunks", change.ChangedFile{Path: "a.go", Status: "M", Language: "go", Hunks: []change.Hunk{
			{OldLines: 1, NewLines: 1, Symbols: []string{"A"}}, {OldLines: 2, NewLines: 1, Symbols: []string{"A", "B"}}}}, "3 lines changed in A, B"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildChange(t, &change.ChangeSet{Files: []change.ChangedFile{tc.file}}, privacy("structure", false))
			if got.Files[0].HunkSummary != tc.want {
				t.Fatalf("summary = %q, want %q", got.Files[0].HunkSummary, tc.want)
			}
		})
	}
}

func TestSummaryCategories(t *testing.T) {
	cases := []struct {
		name, lang, text, want string
	}{
		{"comparison", "go", "-if a > b {\n+if a >= b {\n", "comparison operator changed"},
		{"logical", "go", "-if a && b {\n+if a || b {\n", "logical operator changed"},
		{"arithmetic", "go", "-x = a + b\n+x = a - b\n", "arithmetic operator changed"},
		{"literal number", "go", "-x = 1\n+x = 2\n", "literal changed"},
		{"literal string", "go", "-x = \"" + literal + "\"\n+x = \"other\"\n", "literal changed"},
		{"identifier", "go", "-x = alpha\n+x = beta\n", "identifier changed"},
		{"control flow", "go", "-if a {\n+for a {\n", "control flow changed"},
		{"python comparison and literal", "python", "-    if x == 1:\n+    if x != 2:  # " + sentinel + "\n", "comparison operator changed, literal changed"},
		{"python and to or", "python", "-    if a and b:\n+    if a or b:\n", "logical operator changed"},
		{"comment only", "go", "-// old " + sentinel + "\n+// new\n", "comment changed"},
		{"python comment only", "python", "-# old\n+# new " + sentinel + "\n", "comment changed"},
		{"block comment only", "go", "-/* old */\n+/* new " + sentinel + " */\n", "comment changed"},
		{"lines added", "go", "+x := 1\n+y := 2\n", "lines added"},
		{"lines removed", "go", "-x := 1\n", "lines removed"},
		{"whitespace only", "go", "-x:=1\n+x := 1\n", "whitespace changed"},
		{"reordered", "go", "-a + b\n+b + a\n", "code reordered"},
		{"no text", "go", "", ""},
		{"unprefixed text is ignored", "go", "x := \"" + literal + "\"\n", ""},
		{"raw string with sentinel", "go", "-x := `" + sentinel + "`\n+x := `other`\n", "literal changed"},
		{"python triple quote", "python", "-x = '''" + sentinel + "'''\n+x = '''other'''\n", "literal changed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := change.ChangedFile{Path: "x", Status: "M", Language: tc.lang, Hunks: []change.Hunk{{OldLines: 1, NewLines: 1, Symbols: []string{"F"}, Text: tc.text}}}
			got := buildChange(t, &change.ChangeSet{Files: []change.ChangedFile{f}}, privacy("structure", true)).Files[0].HunkSummary
			want := "1 line changed in F"
			if tc.want != "" {
				want += ": " + tc.want
			}
			if got != want {
				t.Fatalf("summary = %q, want %q", got, want)
			}
			for _, banned := range []string{sentinel, literal, "alpha", "beta"} {
				if strings.Contains(got, banned) {
					t.Fatalf("summary leaks %q: %q", banned, got)
				}
			}
		})
	}
}

// TestSummaryNeverCopiesText feeds adversarial hunk text and checks the
// summary is built from fixed phrases, counts and the declared symbol only.
func TestSummaryNeverCopiesText(t *testing.T) {
	texts := []string{
		"+" + sentinel + "\n",
		"-password = \"" + sentinel + "\"\n+password = \"" + literal + "\"\n",
		"+/* " + sentinel + "\n+" + literal + " */\n",
		"+\"unterminated " + sentinel + "\n",
		"+`raw " + sentinel + "\n+" + literal + "`\n",
		"+'''" + sentinel + "\n",
		"-\u00e9\u4e16" + sentinel + " == 0x" + literal + "\n+" + sentinel + " != 1e9\n",
		"\\ No newline at end of file\n+" + sentinel + "\n",
	}
	for _, lang := range []string{"go", "python", ""} {
		for i, text := range texts {
			f := change.ChangedFile{Path: "p", Status: "M", Language: lang, Hunks: []change.Hunk{{OldLines: 3, NewLines: 2, Symbols: []string{"Declared"}, Text: text}}}
			got := buildChange(t, &change.ChangeSet{Files: []change.ChangedFile{f}}, privacy("structure", true)).Files[0].HunkSummary
			if !strings.HasPrefix(got, "3 lines changed in Declared") {
				t.Fatalf("lang %q text %d: summary = %q", lang, i, got)
			}
			rest := strings.TrimPrefix(got, "3 lines changed in Declared")
			for _, banned := range []string{"SECRET", "hunter2", "password", "unterminated", "raw", "\u00e9", "\u4e16"} {
				if strings.Contains(rest, banned) {
					t.Fatalf("lang %q text %d: summary leaks %q: %q", lang, i, banned, got)
				}
			}
		}
	}
}

func sampleCandidate() impact.Candidate {
	return impact.Candidate{
		TestID: "go:m/auth:TestRefreshNearExpiry", TestName: "TestRefreshNearExpiry", Package: "m/auth",
		Purpose: "refresh with literal " + sentinel,
		Asserts: []string{"session.Valid == \"" + literal + "\"", "err == nil"},
		Features: impact.Features{
			Hops: 2, Reachable: true, CoverageKnown: true, CoverageOverlap: 0.5,
			LinesAdded: 6, LinesRemoved: 2,
		},
	}
}

func TestBuildTestNone(t *testing.T) {
	got := BuildTest(sampleCandidate(), privacy("none", false))
	want := Test{
		Name: "TestRefreshNearExpiry", Package: "m/auth",
		Purpose:        "refresh with literal " + sentinel,
		Asserts:        []string{"session.Valid == \"" + literal + "\"", "err == nil"},
		HopsFromChange: 2, CoversChangedLines: 4,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("test = %+v, want %+v", got, want)
	}
}

func TestBuildTestStructureOmitsPurposeAndAsserts(t *testing.T) {
	got := BuildTest(sampleCandidate(), privacy("structure", true))
	if got.Purpose != "refresh near expiry" {
		t.Fatalf("purpose = %q, want the name split into words", got.Purpose)
	}
	if len(got.Asserts) != 0 {
		t.Fatalf("asserts at structure: %v", got.Asserts)
	}
	js := mustJSON(t, got)
	for _, banned := range []string{sentinel, literal, "session.Valid"} {
		if strings.Contains(js, banned) {
			t.Fatalf("structure descriptor leaks %q: %s", banned, js)
		}
	}
	if got.Name != "TestRefreshNearExpiry" || got.Package != "m/auth" || got.HopsFromChange != 2 {
		t.Fatalf("test = %+v", got)
	}
}

func TestBuildTestHash(t *testing.T) {
	got := BuildTest(sampleCandidate(), privacy("hash", true))
	js := mustJSON(t, got)
	for _, banned := range []string{sentinel, literal, "Refresh", "auth"} {
		if strings.Contains(js, banned) {
			t.Fatalf("hash descriptor leaks %q: %s", banned, js)
		}
	}
	if !tokenPattern.MatchString(got.Name) || !tokenPattern.MatchString(got.Package) || got.Purpose != "" || len(got.Asserts) != 0 {
		t.Fatalf("test = %+v", got)
	}
}

func TestBuildTestFailsClosed(t *testing.T) {
	got := BuildTest(sampleCandidate(), privacy("bogus", true))
	if !tokenPattern.MatchString(got.Name) || got.Purpose != "" || len(got.Asserts) != 0 {
		t.Fatalf("unknown level must hash: %+v", got)
	}
}

func TestBuildTestSymbolFlagHidesPurposeEvenAtNone(t *testing.T) {
	p := privacy("none", false)
	p.SendSymbolNames = false
	got := BuildTest(sampleCandidate(), p)
	if !tokenPattern.MatchString(got.Name) || got.Purpose != "" || len(got.Asserts) != 0 {
		t.Fatalf("purpose and asserts name symbols and must be withheld: %+v", got)
	}
	if got.Package != "m/auth" {
		t.Fatalf("package = %q", got.Package)
	}
	p = privacy("structure", false)
	p.SendPaths = false
	if got := BuildTest(sampleCandidate(), p); !tokenPattern.MatchString(got.Package) {
		t.Fatalf("send_paths=false must hash the package: %q", got.Package)
	}
}

func TestBuildTestHopsAndCoverage(t *testing.T) {
	c := sampleCandidate()
	c.Features.Reachable = false
	c.Features.Hops = 0
	if got := BuildTest(c, privacy("none", false)); got.HopsFromChange != 99 {
		t.Fatalf("unreachable hops = %d, want 99", got.HopsFromChange)
	}
	c = sampleCandidate()
	c.Features.CoverageKnown = false
	c.Features.CoverageOverlap = 0.9
	if got := BuildTest(c, privacy("none", false)); got.CoversChangedLines != 0 {
		t.Fatalf("unknown coverage must not claim covered lines: %d", got.CoversChangedLines)
	}
	c = sampleCandidate()
	c.Features.CoverageOverlap = 1
	if got := BuildTest(c, privacy("none", false)); got.CoversChangedLines != 8 {
		t.Fatalf("full overlap covers = %d, want 8", got.CoversChangedLines)
	}
}

func TestBuildTestParameterLiteralsAreCutAtStructure(t *testing.T) {
	c := sampleCandidate()
	c.TestName = "test_login[" + sentinel + "-x]"
	got := BuildTest(c, privacy("structure", false))
	if got.Name != "test_login" || got.Purpose != "login" {
		t.Fatalf("test = %+v", got)
	}
	if js := mustJSON(t, got); strings.Contains(js, sentinel) {
		t.Fatalf("parameter literal leaks: %s", js)
	}
	if got := BuildTest(c, privacy("none", false)); got.Name != c.TestName {
		t.Fatalf("none keeps the name: %q", got.Name)
	}
}

func TestWords(t *testing.T) {
	cases := map[string]string{
		"TestRefreshNearExpiry":    "refresh near expiry",
		"test_refresh_near_expiry": "refresh near expiry",
		"TestHTTPServer2Start":     "http server 2 start",
		"Test":                     "test",
		"TestXYZ":                  "xyz",
		"BenchmarkFoo":             "benchmark foo",
		"":                         "",
		"test_x[1-a]":              "x",
		"TestX/sub_case":           "x",
	}
	for in, want := range cases {
		if got := words(in); got != want {
			t.Errorf("words(%q) = %q, want %q", in, got, want)
		}
	}
}
