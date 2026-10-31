package change

import (
	"reflect"
	"testing"
)

const notesBase = "l1\nl2\nl3\nl4\nl5\nl6\n"

// hunkTexts returns the Text of every hunk of every file, keyed by path.
func hunkTexts(cs *ChangeSet) map[string][]string {
	out := map[string][]string{}
	for _, f := range cs.Files {
		for _, h := range f.Hunks {
			out[f.Path] = append(out[f.Path], h.Text)
		}
	}
	return out
}

// withoutText returns cs with every Hunk.Text cleared, so two change sets can
// be compared on everything but the text.
func withoutText(cs *ChangeSet) *ChangeSet {
	cp := *cs
	cp.Files = make([]ChangedFile, len(cs.Files))
	for i, f := range cs.Files {
		f.Hunks = append([]Hunk(nil), f.Hunks...)
		for j := range f.Hunks {
			f.Hunks[j].Text = ""
		}
		cp.Files[i] = f
	}
	return &cp
}

func TestExtractHunkTextOfAWorkingTree(t *testing.T) {
	t.Parallel()
	h := &history{}
	h.commit(heads("main"), 0, "base",
		put("a.go", goBase),
		put("notes.txt", notesBase),
		put("tail.txt", "a\nb"),
		putBytes("blob.bin", []byte{0, 1, 2, 3}))
	r := newRepo(t, h, "main", true)
	// Every edit changes the file size: git decides from stat data, and an edit
	// of the same size made right after the checkout can look unchanged.
	r.write(t, "a.go", replaceLine(goBase, 7, "\tfmt.Println(\"ALPHA!\")\n"))
	r.write(t, "notes.txt", "l1\nL2\nl3\nl4\nl6\n")
	r.write(t, "tail.txt", "a\ncc")
	r.writeBytes(t, "blob.bin", []byte{0, 1, 2, 4, 5})
	r.write(t, "fresh.txt", "x\ny\n")

	plain := mustExtract(t, r.dir, testOptions())
	opts := testOptions()
	opts.HunkText = true
	kept := mustExtract(t, r.dir, opts)

	t.Run("text is empty unless asked for", func(t *testing.T) {
		for path, texts := range hunkTexts(plain) {
			for i, text := range texts {
				if text != "" {
					t.Errorf("%s hunk %d: Text = %q without HunkText", path, i, text)
				}
			}
		}
	})
	t.Run("the diff lines of each hunk without headers", func(t *testing.T) {
		want := map[string][]string{
			"a.go":      {"-\tfmt.Println(\"alpha\")\n+\tfmt.Println(\"ALPHA!\")\n"},
			"notes.txt": {"-l2\n+L2\n", "-l5\n"},
			"tail.txt":  {"-b\n+cc\n"}, // the no-newline markers are not diff lines
			"fresh.txt": {"+x\n+y\n"},
		}
		if got := hunkTexts(kept); !reflect.DeepEqual(got, want) {
			t.Errorf("hunk texts\n got: %q\nwant: %q", got, want)
		}
	})
	t.Run("a binary file has no hunks and so no text", func(t *testing.T) {
		f := fileOf(t, kept, "blob.bin")
		if !f.Binary || len(f.Hunks) != 0 {
			t.Errorf("blob.bin: Binary %v, %d hunks, want a binary file without hunks", f.Binary, len(f.Hunks))
		}
	})
	t.Run("everything but the text is unchanged", func(t *testing.T) {
		if got, want := withoutText(kept), plain; !reflect.DeepEqual(got, want) {
			t.Errorf("HunkText changed more than Text\n got: %+v\nwant: %+v", got, want)
		}
	})
}

func TestExtractHunkTextOfACommitRange(t *testing.T) {
	t.Parallel()
	h := &history{}
	m1 := h.commit(heads("main"), 0, "base", put("old.go", goBase), put("notes.txt", notesBase))
	h.commit(heads("main"), m1, "edits",
		rename("old.go", "pkg/new.go"),
		put("pkg/new.go", replaceLine(goBase, 7, alphaEdited)),
		put("notes.txt", "l1\nl2\nl3\nl4\nl5\nl6\nl7\n"))
	r := newRepo(t, h, "main", false)
	opts := testOptions()
	opts.Base, opts.Head = r.sha(1), r.sha(2)
	plain := mustExtract(t, r.dir, opts)
	opts.HunkText = true
	kept := mustExtract(t, r.dir, opts)

	want := map[string][]string{
		"pkg/new.go": {"-\tfmt.Println(\"alpha\")\n+\tfmt.Println(\"ALPHA\")\n"},
		"notes.txt":  {"+l7\n"},
	}
	if got := hunkTexts(kept); !reflect.DeepEqual(got, want) {
		t.Errorf("hunk texts\n got: %q\nwant: %q", got, want)
	}
	for path, texts := range hunkTexts(plain) {
		for _, text := range texts {
			if text != "" {
				t.Errorf("%s: Text = %q without HunkText", path, text)
			}
		}
	}
	if got, want := withoutText(kept), plain; !reflect.DeepEqual(got, want) {
		t.Errorf("HunkText changed more than Text\n got: %+v\nwant: %+v", got, want)
	}
}

func TestParsePatchKeepsOnlyTheLinesOfEachHunk(t *testing.T) {
	t.Parallel()
	// Content lines that look like headers, a context line, a marker line and
	// the file headers that precede the first hunk of a block.
	patch := "diff --git a/t.txt b/t.txt\nindex 1..2 100644\n--- a/t.txt\n+++ b/t.txt\n" +
		"@@ -1 +1 @@\n-diff --git a/fake b/fake\n+@@ -1 +1 @@\n" +
		"@@ -5,2 +5,2 @@\n ctx\n-old\n\\ No newline at end of file\n+new\n" +
		"diff --git a/u.txt b/u.txt\n--- a/u.txt\n+++ b/u.txt\n@@ -1,0 +2 @@\n+--- a/not-a-header\n"

	got, err := parsePatch([]byte(patch), true)
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"-diff --git a/fake b/fake\n+@@ -1 +1 @@\n", " ctx\n-old\n+new\n"}, {"+--- a/not-a-header\n"}}
	if len(got) != len(want) {
		t.Fatalf("%d blocks, want %d", len(got), len(want))
	}
	for i, block := range got {
		if len(block.hunks) != len(want[i]) {
			t.Fatalf("block %d: %d hunks, want %d", i, len(block.hunks), len(want[i]))
		}
		for j, h := range block.hunks {
			if h.Text != want[i][j] {
				t.Errorf("block %d hunk %d: Text = %q, want %q", i, j, h.Text, want[i][j])
			}
		}
	}

	plain, err := parsePatch([]byte(patch), false)
	if err != nil {
		t.Fatal(err)
	}
	for i, block := range plain {
		for j, h := range block.hunks {
			if h.Text != "" {
				t.Errorf("block %d hunk %d: Text = %q without keepText", i, j, h.Text)
			}
		}
	}
}
