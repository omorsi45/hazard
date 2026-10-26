package change

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseHunkHeader(t *testing.T) {
	t.Parallel()
	good := []struct {
		line string
		want Hunk
	}{
		{"@@ -1 +1 @@", Hunk{OldStart: 1, OldLines: 1, NewStart: 1, NewLines: 1}},
		{"@@ -0,0 +1,3 @@", Hunk{OldStart: 0, OldLines: 0, NewStart: 1, NewLines: 3}},
		{"@@ -5,2 +4,0 @@ func (g *Gamma) Run() {", Hunk{OldStart: 5, OldLines: 2, NewStart: 4, NewLines: 0}},
		{"@@ -12 +30,7 @@", Hunk{OldStart: 12, OldLines: 1, NewStart: 30, NewLines: 7}},
	}
	for _, tc := range good {
		got, err := parseHunkHeader(tc.line)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parseHunkHeader(%q) = %+v, %v; want %+v", tc.line, got, err, tc.want)
		}
	}
	for _, line := range []string{"@@ -a +b @@", "@@ -1 +1", "@@@ -1 -1 +1 @@@", "@@ 1 1 @@", "@@ -1,x +1 @@", "@@ -1 +1,y @@"} {
		if got, err := parseHunkHeader(line); err == nil {
			t.Errorf("parseHunkHeader(%q) = %+v, want an error", line, got)
		}
	}
}

func TestParsePatch(t *testing.T) {
	t.Parallel()
	patch := "diff --git a/a.go b/a.go\nindex 1..2 100644\n--- a/a.go\n+++ b/a.go\n@@ -3 +3 @@ x\n-old\n+new\n@@ -9,0 +10,2 @@\n+one\n+two\n" +
		"diff --git a/z.bin b/z.bin\nindex 1..2 100644\nBinary files a/z.bin and b/z.bin differ\n" +
		"diff --git a/r.txt b/s.txt\nsimilarity index 100%\nrename from r.txt\nrename to s.txt\n" +
		// Content lines that look like headers are prefixed and must be ignored.
		"diff --git a/t.txt b/t.txt\n--- a/t.txt\n+++ b/t.txt\n@@ -1 +1 @@\n-diff --git a/fake b/fake\n+@@ -1 +1 @@\n+Binary files x and y differ\n"
	got, err := parsePatch([]byte(patch))
	if err != nil {
		t.Fatal(err)
	}
	want := []patchBlock{
		{hunks: []Hunk{{OldStart: 3, OldLines: 1, NewStart: 3, NewLines: 1}, {OldStart: 9, OldLines: 0, NewStart: 10, NewLines: 2}}},
		{binary: true},
		{},
		{hunks: []Hunk{{OldStart: 1, OldLines: 1, NewStart: 1, NewLines: 1}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parsePatch\n got: %+v\nwant: %+v", got, want)
	}

	if _, err := parsePatch([]byte("stray line\ndiff --git a/a b/a\n")); err == nil {
		t.Error("parsePatch accepted text before the first header")
	}
	if _, err := parsePatch([]byte("diff --git a/a b/a\n@@ nonsense @@\n")); err == nil {
		t.Error("parsePatch accepted a malformed hunk header")
	}
}

// raw builds one `git diff --raw -z` record.
func raw(oldMode, newMode, oldOID, newOID, status string, paths ...string) string {
	return ":" + oldMode + " " + newMode + " " + oldOID + " " + newOID + " " + status + "\x00" + strings.Join(paths, "\x00") + "\x00"
}

const (
	oidA = "1111111111111111111111111111111111111111"
	oidB = "2222222222222222222222222222222222222222"
	zero = "0000000000000000000000000000000000000000"
)

func TestParseDiff(t *testing.T) {
	t.Parallel()
	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		got, err := parseDiff(nil)
		if err != nil || len(got) != 0 {
			t.Errorf("parseDiff(nil) = %+v, %v", got, err)
		}
	})
	t.Run("raw records joined with patch blocks in order", func(t *testing.T) {
		t.Parallel()
		out := raw("000000", "100644", zero, oidB, "A", "new file.go") +
			raw("100644", "100644", oidA, oidB, "R087", "old.go", "moved/new.go") +
			raw("100644", "100644", oidA, zero, "M", "m.go") +
			raw("100644", "000000", oidA, zero, "D", "gone.go") +
			"\x00" +
			"diff --git a/new file.go b/new file.go\nnew file mode 100644\n@@ -0,0 +1,2 @@\n+a\n+b\n" +
			"diff --git a/old.go b/moved/new.go\nrename from old.go\nrename to moved/new.go\n@@ -4 +4 @@\n-x\n+y\n" +
			"diff --git a/m.go b/m.go\nBinary files a/m.go and b/m.go differ\n" +
			"diff --git a/gone.go b/gone.go\ndeleted file mode 100644\n@@ -1,3 +0,0 @@\n-a\n-b\n-c\n"
		got, err := parseDiff([]byte(out))
		if err != nil {
			t.Fatal(err)
		}
		want := []diffEntry{
			{status: 'A', path: "new file.go", oldMode: "000000", newMode: "100644", oldOID: zero, newOID: oidB, hunks: []Hunk{{NewStart: 1, NewLines: 2}}},
			{status: 'R', oldPath: "old.go", path: "moved/new.go", oldMode: "100644", newMode: "100644", oldOID: oidA, newOID: oidB, hunks: []Hunk{{OldStart: 4, OldLines: 1, NewStart: 4, NewLines: 1}}},
			{status: 'M', path: "m.go", oldMode: "100644", newMode: "100644", oldOID: oidA, newOID: zero, binary: true},
			{status: 'D', path: "gone.go", oldMode: "100644", newMode: "000000", oldOID: oidA, newOID: zero, hunks: []Hunk{{OldStart: 1, OldLines: 3}}},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("parseDiff\n got: %+v\nwant: %+v", got, want)
		}
	})
	t.Run("a type change is one record and two patch blocks", func(t *testing.T) {
		t.Parallel()
		out := raw("100644", "120000", oidA, oidB, "T", "link") +
			raw("100644", "100644", oidA, oidB, "M", "z.go") + "\x00" +
			"diff --git a/link b/link\ndeleted file mode 100644\n@@ -1 +0,0 @@\n-x\n" +
			"diff --git a/link b/link\nnew file mode 120000\n@@ -0,0 +1 @@\n+target\n" +
			"diff --git a/z.go b/z.go\n@@ -2 +2 @@\n-a\n+b\n"
		got, err := parseDiff([]byte(out))
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0].status != 'T' || !reflect.DeepEqual(got[0].hunks, []Hunk{{OldStart: 1, OldLines: 1}, {NewStart: 1, NewLines: 1}}) {
			t.Fatalf("type change entry = %+v", got)
		}
		if !reflect.DeepEqual(got[1].hunks, []Hunk{{OldStart: 2, OldLines: 1, NewStart: 2, NewLines: 1}}) {
			t.Errorf("the entry after a type change got the wrong block: %+v", got[1])
		}
	})
	badInputs := []struct {
		name, out, want string
	}{
		{"unmerged path", raw("100644", "100644", oidA, oidB, "U", "conflict.go") + "\x00diff --git a/c b/c\n", "unmerged"},
		{"unknown status", raw("100644", "100644", oidA, oidB, "X", "odd.go") + "\x00diff --git a/o b/o\n", "unsupported"},
		{"fewer blocks than records", raw("100644", "100644", oidA, oidB, "M", "a.go") + raw("100644", "100644", oidA, oidB, "M", "b.go") + "\x00diff --git a/a.go b/a.go\n", "patch blocks"},
		{"more blocks than records", raw("100644", "100644", oidA, oidB, "M", "a.go") + "\x00diff --git a/a.go b/a.go\ndiff --git a/b.go b/b.go\n", "patch blocks"},
		{"record without a path", ":100644 100644 " + oidA + " " + oidB + " M\x00", "no path"},
		{"short record", ":100644 100644 M\x00a.go\x00\x00", "malformed diff record"},
		{"rename without destination", ":100644 100644 " + oidA + " " + oidB + " R100\x00old.go\x00", "no destination"},
		{"output that is not a diff", "fatal: something went wrong\n", "unexpected diff output"},
	}
	for _, tc := range badInputs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseDiff([]byte(tc.out))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("parseDiff = %+v, %v; want an error containing %q", got, err, tc.want)
			}
		})
	}
}

func TestParseStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		out       string
		dirty     bool
		untracked []string
		unmerged  []string
	}{
		{"clean", "", false, nil, nil},
		{"unstaged modification", " M a.go\x00", true, nil, nil},
		{"staged modification", "M  a.go\x00", true, nil, nil},
		{"staged then edited", "MM a.go\x00", true, nil, nil},
		{"staged addition and deletion", "A  n.go\x00 D d.go\x00", true, nil, nil},
		{"untracked", "?? new file.go\x00?? nested/\x00", true, []string{"new file.go", "nested/"}, nil},
		{"ignored is not dirt", "!! debug.log\x00", false, nil, nil},
		{"both modified", "UU a.go\x00", true, nil, []string{"a.go"}},
		{"both added", "AA a.go\x00", true, nil, []string{"a.go"}},
		{"both deleted", "DD a.go\x00", true, nil, []string{"a.go"}},
		{"added by them", "AU a.go\x00UA b.go\x00", true, nil, []string{"a.go", "b.go"}},
		{"mixed", " M ok.go\x00UU bad.go\x00?? u.txt\x00", true, []string{"u.txt"}, []string{"bad.go"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseStatus([]byte(tc.out))
			if err != nil {
				t.Fatal(err)
			}
			if got.dirty != tc.dirty || !reflect.DeepEqual(got.untracked, tc.untracked) || !reflect.DeepEqual(got.unmerged, tc.unmerged) {
				t.Errorf("parseStatus(%q) = %+v, want dirty %v untracked %q unmerged %q", tc.out, got, tc.dirty, tc.untracked, tc.unmerged)
			}
		})
	}
	for _, bad := range []string{"M\x00", "Mxa.go\x00", "?\x00"} {
		if got, err := parseStatus([]byte(bad)); err == nil {
			t.Errorf("parseStatus(%q) = %+v, want an error", bad, got)
		}
	}
}

func TestIsOID(t *testing.T) {
	t.Parallel()
	valid := []string{
		strings.Repeat("a", 40), strings.Repeat("0", 40), strings.Repeat("F", 40), // SHA-1, any hex case
		strings.Repeat("1", 64),                            // SHA-256
		"0123456789abcdefABCDEF" + strings.Repeat("9", 18), // both ends of every range
	}
	for _, s := range valid {
		if !isOID(s) {
			t.Errorf("isOID(%q) = false, want true", s)
		}
	}
	invalid := []string{
		"", "HEAD", strings.Repeat("a", 39), strings.Repeat("a", 41), strings.Repeat("a", 63),
		strings.Repeat("g", 40), "--output=x^{commit}", strings.Repeat("a", 39) + "\n",
	}
	// One character just outside each hex range, in an otherwise valid name.
	for _, c := range []string{"/", ":", "@", "G", "`", "g"} {
		invalid = append(invalid, strings.Repeat("a", 39)+c)
	}
	for _, s := range invalid {
		if isOID(s) {
			t.Errorf("isOID(%q) = true, want false", s)
		}
	}
}

func TestAttribute(t *testing.T) {
	t.Parallel()
	iv, err := GoSymbols([]byte(goBase))
	if err != nil {
		t.Fatal(err)
	}
	ok := side{iv, true}
	broken := side{}
	tests := []struct {
		name           string
		h              Hunk
		old, cur       side
		hasOld, hasNew bool
		want           []string
	}{
		{"change inside one declaration", Hunk{OldStart: 7, OldLines: 1, NewStart: 7, NewLines: 1}, ok, ok, true, true, []string{"Alpha"}},
		{"range spanning three declarations", Hunk{OldStart: 11, OldLines: 6, NewStart: 11, NewLines: 6}, ok, ok, true, true, []string{"(*Gamma).Run", "Beta", "Gamma"}},
		{"removed range uses the old side", Hunk{OldStart: 10, OldLines: 3, NewStart: 9, NewLines: 0}, ok, side{ok: true}, true, true, []string{"Beta"}},
		{"added range uses the new side", Hunk{OldStart: 0, OldLines: 0, NewStart: 14, NewLines: 1}, side{ok: true}, ok, true, true, []string{"Gamma"}},
		{"range outside any declaration", Hunk{OldStart: 3, OldLines: 1, NewStart: 3, NewLines: 1}, ok, ok, true, true, []string{"__file__"}},
		{"insertion after a declaration also names it", Hunk{OldStart: 12, OldLines: 0, NewStart: 13, NewLines: 1}, ok, ok, true, true, []string{"Beta", "__file__"}},
		{"insertion at the top has no old side", Hunk{OldStart: 0, OldLines: 0, NewStart: 1, NewLines: 1}, ok, ok, true, true, []string{"__file__"}},
		{"unparsable old side", Hunk{OldStart: 7, OldLines: 1, NewStart: 7, NewLines: 1}, broken, ok, true, true, []string{"Alpha", "__file__"}},
		{"unparsable new side", Hunk{OldStart: 7, OldLines: 1, NewStart: 7, NewLines: 1}, ok, broken, true, true, []string{"Alpha", "__file__"}},
		{"unparsable old side on an insertion", Hunk{OldStart: 5, OldLines: 0, NewStart: 6, NewLines: 1}, broken, ok, true, true, []string{"Alpha", "__file__"}},
		{"added file ignores the old side", Hunk{OldStart: 0, OldLines: 0, NewStart: 1, NewLines: 18}, broken, ok, false, true, []string{"(*Gamma).Run", "Alpha", "Beta", "Gamma"}},
		{"deleted file ignores the new side", Hunk{OldStart: 1, OldLines: 18, NewStart: 0, NewLines: 0}, ok, broken, true, false, []string{"(*Gamma).Run", "Alpha", "Beta", "Gamma"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := attribute(tc.h, tc.old, tc.cur, tc.hasOld, tc.hasNew)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("attribute = %q, want %q", got, tc.want)
			}
		})
	}
}
