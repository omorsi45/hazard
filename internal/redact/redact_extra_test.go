package redact

import (
	"strings"
	"testing"

	"github.com/omorsi45/hazard/internal/change"
)

func TestBuildChangeNoneHunkTextCarriesPositions(t *testing.T) {
	f := change.ChangedFile{Path: "a.go", Status: "M", Language: "go", Hunks: []change.Hunk{
		{OldStart: 3, OldLines: 1, NewStart: 3, NewLines: 1, Symbols: []string{"A"}, Text: "-x := 1\n+x := 2\n"},
		{OldStart: 9, OldLines: 0, NewStart: 10, NewLines: 1, Symbols: []string{"B"}},
		{OldStart: 20, OldLines: 1, NewStart: 21, NewLines: 0, Symbols: []string{"C"}, Text: "-y := 1"},
	}}
	got := buildChange(t, &change.ChangeSet{Files: []change.ChangedFile{f}}, privacy("none", true)).Files[0].HunkText
	want := "@@ -3,1 +3,1 @@\n-x := 1\n+x := 2\n@@ -20,1 +21,0 @@\n-y := 1\n"
	if got != want {
		t.Fatalf("hunk text = %q, want %q", got, want)
	}
}

func TestSummarySpansLines(t *testing.T) {
	cases := []struct {
		name, lang, text, want string
	}{
		{"block comment over lines", "go", "-/* one\n-two */ x := 1\n+/* three\n+four */ x := 1\n", "comment changed"},
		{"raw string over lines", "go", "-s := `a\n-b`\n+s := `a\n+c`\n", "literal changed"},
		{"triple quote over lines", "python", "-s = '''a\n-b'''\n+s = '''a\n+c'''\n", "literal changed"},
		{"python floor division is arithmetic", "python", "-x = a // b\n+x = a * b\n", "arithmetic operator changed"},
		{"go double slash is a comment", "go", "-x := a // b\n+x := a // c\n", "comment changed"},
		{"boolean constants are literals", "go", "-ok := true\n+ok := false\n", "literal changed"},
		{"empty lines only", "go", "-\n+\n+\n", "whitespace changed"},
		{"blank lines against code", "go", "-\n+x := 1\n", "lines added"},
		{"punctuation only", "go", "-f(a)\n+f(a);\n", "punctuation changed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := change.ChangedFile{Path: "x", Status: "M", Language: tc.lang, Hunks: []change.Hunk{{OldLines: 1, NewLines: 1, Symbols: []string{"F"}, Text: tc.text}}}
			got := buildChange(t, &change.ChangeSet{Files: []change.ChangedFile{f}}, privacy("structure", true)).Files[0].HunkSummary
			if want := "1 line changed in F: " + tc.want; got != want {
				t.Fatalf("summary = %q, want %q", got, want)
			}
		})
	}
}

func TestSummaryJoinsCategoriesAcrossHunks(t *testing.T) {
	f := change.ChangedFile{Path: "x", Status: "M", Language: "go", Hunks: []change.Hunk{
		{OldLines: 1, NewLines: 1, Symbols: []string{"A"}, Text: "-x := 1\n+x := 2\n"},
		{OldLines: 1, NewLines: 1, Symbols: []string{"B"}, Text: "-if a > b {\n+if a < b {\n"},
		{OldLines: 1, NewLines: 1, Symbols: []string{"A"}, Text: "-y := 1\n+y := 3\n"},
	}}
	got := buildChange(t, &change.ChangeSet{Files: []change.ChangedFile{f}}, privacy("structure", true)).Files[0].HunkSummary
	if want := "3 lines changed in A, B: comparison operator changed, literal changed"; got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
}

func TestBuildChangeAbbreviatesShortIDs(t *testing.T) {
	got := buildChange(t, &change.ChangeSet{BaseSHA: "abc", HeadSHA: "0123456789abcdef"}, privacy("structure", false))
	if got.Base != "abc" || got.Head != "0123456" {
		t.Fatalf("base %q head %q", got.Base, got.Head)
	}
	if js := mustJSON(t, got); !strings.Contains(js, `"files":[]`) {
		t.Fatalf("json = %s", js)
	}
}
