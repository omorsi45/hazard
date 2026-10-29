package engine

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// headFiles are the files of the reader fixture at its commit.
func headFiles() map[string]string {
	return map[string]string{
		".gitignore":          "*.log\n",
		"a.go":                "package a\n",
		"gone.go":             "package gone\n",
		"dir with space/b.go": "package b\n",
		"ünï.go":              "package u\n",
		"big.txt":             strings.Repeat("0123456789abcdef", 20000), // 320 kB, past one pipe buffer
		"core/x.go":           "package core\n",
	}
}

// newReaderFixture commits headFiles, then leaves the work tree different:
// a.go edited, gone.go deleted, new.go untracked, debug.log ignored.
func newReaderFixture(t *testing.T) (f *fixture, commit string) {
	t.Helper()
	f = newFixtureFiles(t, headFiles())
	commit = f.git(t, "rev-parse", "HEAD")
	f.write(t, map[string]string{"a.go": "package a // edited\n", "new.go": "package n\n", "debug.log": "x\n"})
	if err := os.Remove(filepath.Join(f.dir, "gone.go")); err != nil {
		t.Fatal(err)
	}
	return f, commit
}

func sorted(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}

func TestWorkTreeReader(t *testing.T) {
	t.Parallel()
	f, _ := newReaderFixture(t)
	r := newTreeHead(context.Background(), f.dir, true, "")
	defer func() {
		if err := r.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	t.Run("lists what exists: tracked and untracked, not ignored, not deleted, not directories", func(t *testing.T) {
		got, err := r.List()
		if err != nil {
			t.Fatal(err)
		}
		want := []string{".gitignore", "a.go", "big.txt", "core/x.go", "dir with space/b.go", "new.go", "ünï.go"}
		if !reflect.DeepEqual(sorted(got), want) {
			t.Errorf("List = %q, want %q", sorted(got), want)
		}
	})
	t.Run("reads the disk, not the commit", func(t *testing.T) {
		got, err := r.Read("a.go")
		if err != nil || string(got) != "package a // edited\n" {
			t.Errorf("Read(a.go) = %q, %v", got, err)
		}
		got, err = r.Read("dir with space/b.go")
		if err != nil || string(got) != "package b\n" {
			t.Errorf("Read(dir with space/b.go) = %q, %v", got, err)
		}
	})
	t.Run("an absent file wraps ErrNotExist", func(t *testing.T) {
		if _, err := r.Read("gone.go"); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Read(gone.go) err = %v, want fs.ErrNotExist", err)
		}
	})
	t.Run("a path outside the repository is refused, not reported absent", func(t *testing.T) {
		for _, rel := range []string{"../x", "/etc/passwd", "a/../../x", ""} {
			if _, err := r.Read(rel); err == nil || errors.Is(err, fs.ErrNotExist) {
				t.Errorf("Read(%q) err = %v, want a refusal that is not ErrNotExist", rel, err)
			}
		}
	})
	t.Run("a directory is an error, not an empty file", func(t *testing.T) {
		if got, err := r.Read("core"); err == nil {
			t.Errorf("Read(core) = %q, want an error", got)
		}
	})
}

func TestCommitReader(t *testing.T) {
	t.Parallel()
	f, commit := newReaderFixture(t)
	r := newTreeHead(context.Background(), f.dir, false, commit)

	t.Run("lists the commit, not the work tree", func(t *testing.T) {
		got, err := r.List()
		if err != nil {
			t.Fatal(err)
		}
		want := []string{".gitignore", "a.go", "big.txt", "core/x.go", "dir with space/b.go", "gone.go", "ünï.go"}
		if !reflect.DeepEqual(sorted(got), want) {
			t.Errorf("List = %q, want %q", sorted(got), want)
		}
	})
	t.Run("reads the commit through one process, whatever the work tree holds", func(t *testing.T) {
		for rel, want := range map[string]string{"a.go": "package a\n", "gone.go": "package gone\n", "dir with space/b.go": "package b\n", "ünï.go": "package u\n"} {
			got, err := r.Read(rel)
			if err != nil || string(got) != want {
				t.Errorf("Read(%q) = %q, %v, want %q", rel, got, err, want)
			}
		}
		big, err := r.Read("big.txt")
		if err != nil || !bytes.Equal(big, []byte(headFiles()["big.txt"])) {
			t.Errorf("Read(big.txt): %d bytes, %v, want %d identical bytes", len(big), err, len(headFiles()["big.txt"]))
		}
	})
	t.Run("a path that is not a file at the commit wraps ErrNotExist and the stream survives", func(t *testing.T) {
		for _, rel := range []string{"new.go", "core", "no/such/file.go"} {
			if _, err := r.Read(rel); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("Read(%q) err = %v, want fs.ErrNotExist", rel, err)
			}
			if got, err := r.Read("a.go"); err != nil || string(got) != "package a\n" {
				t.Fatalf("Read(a.go) after %q = %q, %v: the stream is out of step", rel, got, err)
			}
		}
	})
	t.Run("an unusable path is refused without a request and without ErrNotExist", func(t *testing.T) {
		for _, rel := range []string{"bad\nname.go", "../x", "/abs", ""} {
			if _, err := r.Read(rel); err == nil || errors.Is(err, fs.ErrNotExist) {
				t.Errorf("Read(%q) err = %v, want a refusal that is not ErrNotExist", rel, err)
			}
		}
		if got, err := r.Read("a.go"); err != nil || string(got) != "package a\n" {
			t.Errorf("Read(a.go) afterwards = %q, %v", got, err)
		}
	})
	t.Run("Close ends the process once, and a closed reader does not read", func(t *testing.T) {
		if err := r.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if err := r.Close(); err != nil {
			t.Errorf("second Close: %v", err)
		}
		if _, err := r.Read("a.go"); err == nil {
			t.Error("Read after Close succeeded")
		}
	})
}

func TestCommitReaderWithoutReadsCloses(t *testing.T) {
	t.Parallel()
	f, commit := newReaderFixture(t)
	r := newTreeHead(context.Background(), f.dir, false, commit)
	if err := r.Close(); err != nil {
		t.Errorf("Close without any Read: %v", err)
	}
}

func TestCommitReaderUnknownCommitFailsEveryRead(t *testing.T) {
	t.Parallel()
	f, _ := newReaderFixture(t)
	r := newTreeHead(context.Background(), f.dir, false, strings.Repeat("0", 40))
	defer func() { _ = r.Close() }()

	if _, err := r.List(); err == nil {
		t.Error("List of an unknown commit succeeded")
	}
	// git answers "missing" for every path of an unknown commit: that is an
	// absent file to the index, which treats it as stale.
	if _, err := r.Read("a.go"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Read from an unknown commit err = %v, want fs.ErrNotExist", err)
	}
}

func TestCommitReaderCanceledContext(t *testing.T) {
	t.Parallel()
	f, commit := newReaderFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	r := newTreeHead(ctx, f.dir, false, commit)
	if _, err := r.Read("a.go"); err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := r.Read("a.go"); err == nil {
		t.Error("Read after the context ended succeeded")
	}
	if err := r.Close(); err != nil {
		t.Errorf("Close after cancellation: %v", err)
	}
}
