package mutate

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type tarEntry struct {
	name string
	typ  byte
	body string
}

func buildTar(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: 0o644, Size: int64(len(e.body))}
		switch e.typ {
		case tar.TypeDir:
			hdr.Mode, hdr.Size = 0o755, 0
		case tar.TypeXGlobalHeader:
			hdr.Mode, hdr.Size = 0, 0
			hdr.PAXRecords = map[string]string{"comment": "0123456789abcdef"}
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractTarWritesFiles(t *testing.T) {
	dst := t.TempDir()
	data := buildTar(t,
		tarEntry{"pax_global_header", tar.TypeXGlobalHeader, ""},
		tarEntry{"a/", tar.TypeDir, ""},
		tarEntry{"a/b.txt", tar.TypeReg, "hello\r\nworld"},
		tarEntry{"top.txt", tar.TypeReg, ""},
	)
	if err := extractTar(bytes.NewReader(data), dst); err != nil {
		t.Fatalf("extractTar: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "a", "b.txt"))
	if err != nil || string(got) != "hello\r\nworld" {
		t.Fatalf("a/b.txt = %q, %v; want the exact bytes", got, err)
	}
	if _, err := os.Stat(filepath.Join(dst, "top.txt")); err != nil {
		t.Fatalf("top.txt: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "pax_global_header")); err == nil {
		t.Fatalf("the pax global header was written as a file")
	}
}

func TestExtractTarRejectsEscapingEntries(t *testing.T) {
	for _, name := range []string{
		"../evil.txt",
		"a/../../evil.txt",
		"/abs/evil.txt",
		`\evil.txt`,
		`..\evil.txt`,
		"C:/evil.txt",
		"C:evil.txt",
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			dst := filepath.Join(root, "slot")
			if err := os.Mkdir(dst, 0o755); err != nil {
				t.Fatal(err)
			}
			data := buildTar(t, tarEntry{name, tar.TypeReg, "x"})
			err := extractTar(bytes.NewReader(data), dst)
			if err == nil || !strings.Contains(err.Error(), "unsafe") {
				t.Fatalf("extractTar(%q) error = %v, want an unsafe path error", name, err)
			}
			entries, readErr := os.ReadDir(root)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(entries) != 1 {
				t.Fatalf("an entry escaped the slot: %v", entries)
			}
		})
	}
}

func TestTopLevelFindsTheRepositoryRootFromASubdirectory(t *testing.T) {
	repo := newRepo(t, map[string]string{"a/b/c.txt": "x\n"})
	got, err := TopLevel(context.Background(), filepath.Join(repo, "a", "b"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.Stat(repo)
	if err != nil {
		t.Fatal(err)
	}
	gotInfo, err := os.Stat(got)
	if err != nil || !os.SameFile(want, gotInfo) {
		t.Errorf("TopLevel = %q (%v), want the directory %s", got, err, repo)
	}
}

func TestTopLevelOutsideARepositoryIsAnError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir))
	if got, err := TopLevel(context.Background(), dir); err == nil {
		t.Errorf("TopLevel(%s) = %q, want an error outside a git work tree", dir, got)
	}
}
