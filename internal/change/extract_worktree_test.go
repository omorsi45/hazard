package change

import (
	"bytes"
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// goFor is goBase under a distinct package name, so the files of one fixture
// differ and git can only pair a rename with its true source.
func goFor(name string) string { return replaceLine(goBase, 1, "package "+name+"\n") }

func crlf(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }

// buildWorktreeRepo returns a fixture whose working tree holds every kind of
// uncommitted state at once. HEAD (mark 2) is one commit past mark 1.
//
//	a.go   staged edit            b.go   unstaged edit        c.go   staged then edited again
//	d.go   deleted on disk        e.go   staged rename + edit f.go   removed from the index, edited on disk
//	g.go   moved on disk only     h.go   untouched, stat-stale
//
// plus untracked files of several shapes, an ignored file and a nested repo.
// core.autocrlf is on and c.go and new.go carry CRLF line endings.
func buildWorktreeRepo(t *testing.T) *repo {
	t.Helper()
	h := &history{}
	m1 := h.commit(heads("main"), 0, "base",
		put(".gitignore", "*.log\n"), put("seed.txt", "x\n"), put("x.go", goFor("x")),
		put("a.go", goFor("a")), put("b.go", goFor("b")), put("c.go", goFor("c")), put("d.go", goFor("d")),
		put("e.go", goFor("e")), put("f.go", goFor("f")), put("g.go", goFor("g")), put("h.go", goFor("h")))
	h.commit(heads("main"), m1, "x edited", put("x.go", replaceLine(goFor("x"), 7, alphaEdited)))
	r := newRepo(t, h, "main", true)

	r.git(t, "config", "core.autocrlf", "true")
	r.write(t, "a.go", replaceLine(goFor("a"), 7, alphaEdited))
	staged := replaceLine(goFor("c"), 7, alphaEdited)
	r.write(t, "c.go", crlf(staged))
	r.git(t, "add", "a.go", "c.go")
	r.write(t, "c.go", crlf(replaceLine(staged, 17, runEdited)))
	r.write(t, "b.go", replaceLine(goFor("b"), 17, runEdited))
	r.remove(t, "d.go")
	r.mv(t, "e.go", "pkg/e2.go")
	r.write(t, "pkg/e2.go", replaceLine(goFor("e"), 7, alphaEdited))
	r.git(t, "rm", "-q", "--cached", "f.go")
	r.write(t, "f.go", replaceLine(goFor("f"), 7, alphaEdited))
	if err := os.MkdirAll(r.path("pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(r.path("g.go"), r.path("pkg/g3.go")); err != nil {
		t.Fatal(err)
	}

	r.write(t, "new.go", crlf(goFor("fresh")))
	r.write(t, "-new.go", goFor("dash"))
	r.write(t, "ünï cödé/ß.go", goFor("uni"))
	r.write(t, "dir with space/x y.go", goFor("space"))
	r.write(t, "notes.md", "n1\nn2\n")
	r.write(t, "sub/new.py", pyBase)
	r.writeBytes(t, "blob.bin", []byte{0, 1, 2, 0})
	r.write(t, "empty.txt", "")
	r.write(t, "go.mod", "module x\n")
	r.write(t, "no_newline.txt", "a\nb")
	r.write(t, "debug.log", "ignored\n")
	r.write(t, "nested/inner.txt", "x\n")
	if _, err := gitRun(r.path("nested"), "init", "-q", "-b", "main"); err != nil {
		t.Fatal(err)
	}

	future := time.Now().Add(time.Hour) // same content, newer mtime: the index entry is stat-stale
	if err := os.Chtimes(r.path("h.go"), future, future); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestExtractWorkingTree(t *testing.T) {
	t.Parallel()
	r := buildWorktreeRepo(t)
	indexBefore, err := os.ReadFile(r.path(".git/index"))
	if err != nil {
		t.Fatal(err)
	}

	cs := mustExtract(t, r.dir, testOptions())

	t.Run("identity", func(t *testing.T) {
		if cs.BaseSHA != r.sha(2) || cs.HeadSHA != r.sha(2) {
			t.Errorf("SHAs = %s..%s, want both %s", cs.BaseSHA, cs.HeadSHA, r.sha(2))
		}
		if !cs.Dirty {
			t.Error("Dirty = false for a dirty working tree")
		}
	})
	wantPaths(t, cs,
		"-new.go", "a.go", "b.go", "blob.bin", "c.go", "d.go", "dir with space/x y.go", "empty.txt",
		"f.go", "g.go", "go.mod", "nested/", "new.go", "no_newline.txt", "notes.md",
		"pkg/e2.go", "pkg/g3.go", "sub/new.py", "ünï cödé/ß.go")

	t.Run("staged, unstaged and staged-then-edited", func(t *testing.T) {
		wantHunks(t, fileOf(t, cs, "a.go"), hunk(7, 1, 7, 1, "Alpha"))
		wantHunks(t, fileOf(t, cs, "b.go"), hunk(17, 1, 17, 1, "(*Gamma).Run"))
		// One entry for the final content, with CRLF line endings on disk.
		wantHunks(t, fileOf(t, cs, "c.go"), hunk(7, 1, 7, 1, "Alpha"), hunk(17, 1, 17, 1, "(*Gamma).Run"))
	})
	t.Run("deletion, rename and move", func(t *testing.T) {
		d := fileOf(t, cs, "d.go")
		if d.Status != "D" {
			t.Errorf("d.go Status = %q, want D", d.Status)
		}
		wantHunks(t, d, hunk(1, 18, 0, 0, allGoBase...))

		e := fileOf(t, cs, "pkg/e2.go")
		if e.Status != "R" || e.OldPath != "e.go" {
			t.Errorf("pkg/e2.go = %+v, want a rename from e.go", e)
		}
		wantHunks(t, e, hunk(7, 1, 7, 1, "Alpha"))

		// Moved on disk without git: a deletion plus an untracked addition.
		if got := fileOf(t, cs, "g.go"); got.Status != "D" {
			t.Errorf("g.go Status = %q, want D", got.Status)
		}
		g3 := fileOf(t, cs, "pkg/g3.go")
		if g3.Status != "A" {
			t.Errorf("pkg/g3.go Status = %q, want A", g3.Status)
		}
		wantHunks(t, g3, hunk(0, 0, 1, 18, allGoBase...))
	})
	t.Run("removed from the index but still on disk is one modification", func(t *testing.T) {
		f := fileOf(t, cs, "f.go")
		if f.Status != "M" {
			t.Errorf("f.go Status = %q, want M", f.Status)
		}
		wantHunks(t, f, hunk(1, 18, 0, 0, allGoBase...), hunk(0, 0, 1, 18, allGoBase...))
	})
	t.Run("untracked files", func(t *testing.T) {
		for _, p := range []string{"-new.go", "new.go", "ünï cödé/ß.go", "dir with space/x y.go"} {
			f := fileOf(t, cs, p)
			if f.Status != "A" || f.Language != "go" || f.Binary {
				t.Errorf("%s = %+v", p, f)
			}
			wantHunks(t, f, hunk(0, 0, 1, 18, allGoBase...))
		}
		notes := fileOf(t, cs, "notes.md")
		if notes.Status != "A" || notes.Kind != KindDocs || notes.Language != "" {
			t.Errorf("notes.md = %+v", notes)
		}
		wantHunks(t, notes, hunk(0, 0, 1, 2, "__file__"))
		wantHunks(t, fileOf(t, cs, "no_newline.txt"), hunk(0, 0, 1, 2, "__file__")) // last line has no LF
		py := fileOf(t, cs, "sub/new.py")
		if py.Status != "A" || py.Language != "python" {
			t.Errorf("sub/new.py = %+v", py)
		}
		wantHunks(t, py, hunk(0, 0, 1, 12, "Box", "helper"))
		if b := fileOf(t, cs, "blob.bin"); !b.Binary || len(b.Hunks) != 0 || b.Status != "A" {
			t.Errorf("blob.bin = %+v", b)
		}
		if e := fileOf(t, cs, "empty.txt"); e.Binary || len(e.Hunks) != 0 || e.Status != "A" {
			t.Errorf("empty.txt = %+v", e)
		}
		if n := fileOf(t, cs, "nested/"); n.Status != "A" || len(n.Hunks) != 0 || n.Binary {
			t.Errorf("nested/ = %+v, want an addition without hunks", n)
		}
		gomod := fileOf(t, cs, "go.mod")
		if gomod.Kind != KindBuild || gomod.Status != "A" {
			t.Errorf("go.mod = %+v", gomod)
		}
		if !reflect.DeepEqual(cs.TriggersFull, []string{"go.mod:go.mod"}) {
			t.Errorf("TriggersFull = %q, want the untracked go.mod", cs.TriggersFull)
		}
	})
	t.Run("ignored and unchanged files are absent", func(t *testing.T) {
		for _, f := range cs.Files {
			if f.Path == "debug.log" || f.Path == "h.go" || f.Path == "x.go" {
				t.Errorf("unexpected file %q", f.Path)
			}
		}
	})
	t.Run("line totals", func(t *testing.T) {
		// added: a1 b1 c2 e1 f18 g3 18 -new 18 new 18 ünï 18 space 18 py 12 notes 2 no_newline 2 go.mod 1
		if cs.LinesAdded != 130 || cs.LinesRemoved != 59 {
			t.Errorf("lines +%d -%d, want +130 -59", cs.LinesAdded, cs.LinesRemoved)
		}
	})
	t.Run("the index is not rewritten", func(t *testing.T) {
		after, err := os.ReadFile(r.path(".git/index"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(indexBefore, after) {
			t.Error("Extract rewrote .git/index")
		}
	})

	t.Run("explicit base spans the commit and the working tree", func(t *testing.T) {
		opts := testOptions()
		opts.Base = r.sha(1)[:10] // abbreviated
		got := mustExtract(t, r.dir, opts)
		if got.BaseSHA != r.sha(1) || got.HeadSHA != r.sha(2) || !got.Dirty {
			t.Errorf("BaseSHA %s HeadSHA %s Dirty %v; want %s %s true", got.BaseSHA, got.HeadSHA, got.Dirty, r.sha(1), r.sha(2))
		}
		if len(got.Files) != len(cs.Files)+1 {
			t.Errorf("%d files, want %d (the working-tree set plus x.go)", len(got.Files), len(cs.Files)+1)
		}
		wantHunks(t, fileOf(t, got, "x.go"), hunk(7, 1, 7, 1, "Alpha"))
	})
	t.Run("explicit head ignores the working tree", func(t *testing.T) {
		opts := testOptions()
		opts.Base, opts.Head = r.sha(1), "HEAD"
		got := mustExtract(t, r.dir, opts)
		if got.Dirty || got.HeadSHA != r.sha(2) {
			t.Errorf("Dirty %v HeadSHA %s; want false %s", got.Dirty, got.HeadSHA, r.sha(2))
		}
		wantPaths(t, got, "x.go")
	})
}

func TestExtractDirtyFlag(t *testing.T) {
	t.Parallel()
	h := &history{}
	m1 := h.commit(heads("main"), 0, "base", put(".gitignore", "*.log\n"), put("a.go", goBase))
	h.commit(heads("main"), m1, "edit", put("a.go", replaceLine(goBase, 7, alphaEdited)))
	r := newRepo(t, h, "main", true)

	// An ignored file and an edit reverted to the committed bytes are not dirt.
	r.write(t, "debug.log", "ignored\n")
	r.write(t, "a.go", goBase)
	r.write(t, "a.go", replaceLine(goBase, 7, alphaEdited))
	cs := mustExtract(t, r.dir, testOptions())
	if cs.Dirty {
		t.Error("Dirty = true with only an ignored file and a reverted edit")
	}
	wantPaths(t, cs)

	r.write(t, "new.txt", "x\n")
	cs = mustExtract(t, r.dir, testOptions())
	if !cs.Dirty {
		t.Error("Dirty = false with an untracked file")
	}
	wantPaths(t, cs, "new.txt")

	// Dirty is about the working tree against HEAD, not against the base.
	r.remove(t, "new.txt")
	opts := testOptions()
	opts.Base = r.sha(1)
	cs = mustExtract(t, r.dir, opts)
	if cs.Dirty {
		t.Error("Dirty = true for a clean tree whose HEAD differs from the base")
	}
	wantPaths(t, cs, "a.go")
}

func TestExtractUnmergedPathsAreAnError(t *testing.T) {
	t.Parallel()
	h := &history{}
	m1 := h.commit(heads("main"), 0, "base", put("a.txt", "1\n"))
	h.commit(heads("other"), m1, "other", put("a.txt", "2\n"))
	h.commit(heads("main"), m1, "main", put("a.txt", "3\n"))
	r := newRepo(t, h, "main", true)
	if _, err := gitRun(r.dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "merge", "other"); err == nil {
		t.Fatal("the fixture merge unexpectedly succeeded")
	}

	cs, err := Extract(context.Background(), r.dir, testOptions())
	if err == nil {
		t.Fatalf("Extract succeeded on a conflicted tree: %+v", cs)
	}
	if !strings.Contains(err.Error(), "unmerged") || !strings.Contains(err.Error(), "a.txt") {
		t.Errorf("error = %v, want it to name the unmerged path", err)
	}
}
