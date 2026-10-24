package mine

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func rawTestHash(raw []byte) string { return fmt.Sprintf("%x", sha256.Sum256(raw)) }

func rawTestRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func rawTestNames(t *testing.T, dir string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, entry := range entries {
		got = append(got, entry.Name())
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("remaining names = %v, want %v", got, want)
	}
}

func rawTestCreate(dir, pattern string) (rawTempFile, error) { return os.CreateTemp(dir, pattern) }

func TestRawPublishesActualBytes(t *testing.T) {
	raw := []byte("actual acquired report bytes\n")
	dir := filepath.Join(t.TempDir(), "raw")
	hash, path, err := storeRawBlob(dir, raw)
	if err != nil {
		t.Fatal(err)
	}
	if hash != rawTestHash(raw) || path != filepath.Join(dir, hash) || !filepath.IsAbs(path) {
		t.Fatalf("actual digest/absolute hash-only path = %q/%q", hash, path)
	}
	if !bytes.Equal(rawTestRead(t, path), raw) {
		t.Fatal("published bytes differ from acquired content")
	}
	rawTestNames(t, dir, hash)
}

func TestRawEmptyBody(t *testing.T) {
	dir := t.TempDir()
	raw := []byte{}
	hash, path, err := storeRawBlob(dir, raw)
	if err != nil {
		t.Fatal(err)
	}
	if hash != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" || path != filepath.Join(dir, hash) {
		t.Fatal("actual empty body did not retain its real SHA256-only file")
	}
	if len(rawTestRead(t, path)) != 0 {
		t.Fatal("empty acquired body gained invented bytes")
	}
	rawTestNames(t, dir, hash)
}

func TestRawCleanAbsoluteDirectory(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	hash, path, err := storeRawBlob("raw/../evidence", []byte("body"))
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(cwd, "evidence", hash) || !filepath.IsAbs(path) {
		t.Fatalf("unclean or ambiguous local path %q", path)
	}
}

func TestRawVerifiedReuse(t *testing.T) {
	dir := t.TempDir()
	raw := []byte("existing verified report\n")
	hash := rawTestHash(raw)
	path := filepath.Join(dir, hash)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	created, published := false, false
	gotHash, gotPath, err := storeRawBlobWith(dir, raw,
		func(string, string) (rawTempFile, error) {
			created = true
			return nil, errors.New("unnecessary temporary write")
		},
		func(string, string) error { published = true; return errors.New("unnecessary publication") })
	if err != nil || gotHash != hash || gotPath != path {
		t.Fatalf("verified existing reuse = %q/%q, %v", gotHash, gotPath, err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if created || published || !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) || !bytes.Equal(rawTestRead(t, path), raw) {
		t.Fatal("verified replay rewrote existing evidence or created another temporary file")
	}
	rawTestNames(t, dir, hash)
}

func TestRawRejectsExistingCorruption(t *testing.T) {
	raw := []byte("expected content")
	for _, tc := range []struct {
		name     string
		existing []byte
	}{
		{"same_size", []byte("replaced content")},
		{"prefix_extra", append(append([]byte{}, raw...), 'x')},
		{"short", raw[:len(raw)-1]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			hash := rawTestHash(raw)
			path := filepath.Join(dir, hash)
			if err := os.WriteFile(path, tc.existing, 0600); err != nil {
				t.Fatal(err)
			}
			created := false
			gotHash, gotPath, err := storeRawBlobWith(dir, raw,
				func(string, string) (rawTempFile, error) {
					created = true
					return nil, errors.New("corrupt evidence reached temporary creation")
				}, os.Link)
			if err == nil {
				t.Fatal("corrupt hash-named evidence was accepted for reuse")
			}
			if gotHash != "" || gotPath != "" || created || !bytes.Equal(rawTestRead(t, path), tc.existing) {
				t.Fatal("corruption changed existing bytes, reached a write, or returned a usable reference")
			}
			rawTestNames(t, dir, hash)
		})
	}
}

func TestRawRejectsNonregularDestination(t *testing.T) {
	dir := t.TempDir()
	raw := []byte("body")
	hash := rawTestHash(raw)
	path := filepath.Join(dir, hash)
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(path, "sentinel")
	if err := os.WriteFile(sentinel, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := storeRawBlob(dir, raw); err == nil {
		t.Fatal("nonregular hash-named destination accepted")
	}
	if string(rawTestRead(t, sentinel)) != "keep" {
		t.Fatal("nonregular existing evidence changed")
	}
	rawTestNames(t, dir, hash)
}

func TestRawInputBoundariesBeforeMutation(t *testing.T) {
	input := make([]byte, maxArtifactInputBytes+1)
	for _, tc := range []struct {
		name     string
		size     int
		accepted bool
	}{
		{"limit", maxArtifactInputBytes, true},
		{"next_byte", maxArtifactInputBytes + 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "not-created")
			created, published := false, false
			fault := errors.New("temporary creation reached")
			_, _, err := storeRawBlobWith(dir, input[:tc.size],
				func(string, string) (rawTempFile, error) { created = true; return nil, fault },
				func(string, string) error { published = true; return fault })
			if err == nil || published || created != tc.accepted {
				t.Fatal("input bound did not decide before filesystem work")
			}
			if tc.accepted {
				if !errors.Is(err, fault) {
					t.Fatal("accepted64MiB input did not reach the write stage")
				}
			} else if _, statErr := os.Stat(dir); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatal("over-limit input created its raw directory")
			}
		})
	}
}

func TestRawEmptyDirectoryBeforeMutation(t *testing.T) {
	created := false
	_, _, err := storeRawBlobWith("", []byte("body"),
		func(string, string) (rawTempFile, error) { created = true; return nil, errors.New("unexpected temp") }, os.Link)
	if err == nil || created {
		t.Fatal("empty raw directory reached filesystem work")
	}
}

type rawCountingReader struct {
	io.Reader
	bytes int
}

func (r *rawCountingReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.bytes += n
	return n, err
}

func TestRawVerificationReadBound(t *testing.T) {
	expected := []byte("body")
	actual := append(append([]byte{}, expected...), bytes.Repeat([]byte("x"), 1024)...)
	reader := &rawCountingReader{Reader: bytes.NewReader(actual)}
	err := verifyRawContent(reader, rawTestHash(expected), len(expected))
	if err == nil {
		t.Error("oversize existing content was verified")
	}
	if reader.bytes > len(expected)+1 {
		t.Fatalf("verification read %d bytes, bound %d", reader.bytes, len(expected)+1)
	}
}

func TestRawConcurrentDestination(t *testing.T) {
	raw := []byte("competing publication")
	for _, corrupt := range []bool{false, true} {
		name := "good"
		if corrupt {
			name = "corrupt"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			sentinel := filepath.Join(dir, "sentinel")
			if err := os.WriteFile(sentinel, []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			hash := rawTestHash(raw)
			path := filepath.Join(dir, hash)
			existing := raw
			if corrupt {
				existing = []byte("corrupt competitor")
			}
			published := false
			gotHash, gotPath, err := storeRawBlobWith(dir, raw, rawTestCreate,
				func(old, target string) error {
					published = true
					if filepath.Dir(old) != dir || target != path {
						t.Fatal("publication escaped its same-directory hash-only path")
					}
					if writeErr := os.WriteFile(target, existing, 0600); writeErr != nil {
						t.Fatal(writeErr)
					}
					return os.Link(old, target)
				})
			if !published {
				t.Fatal("missing destination did not reach real publication")
			}
			if corrupt {
				if err == nil || gotHash != "" || gotPath != "" {
					t.Fatal("competing corrupt destination was accepted")
				}
			} else if err != nil || gotHash != hash || gotPath != path {
				t.Fatalf("competing good destination not verified: %v", err)
			}
			if !bytes.Equal(rawTestRead(t, path), existing) || string(rawTestRead(t, sentinel)) != "keep" {
				t.Fatal("race handling changed existing evidence or sentinel")
			}
			rawTestNames(t, dir, hash, "sentinel")
		})
	}
}

func TestRawOtherPublicationError(t *testing.T) {
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "sentinel")
	if err := os.WriteFile(sentinel, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	fault := errors.New("publication unavailable")
	hash, path, err := storeRawBlobWith(dir, []byte("body"), rawTestCreate, func(string, string) error { return fault })
	if !errors.Is(err, fault) || hash != "" || path != "" {
		t.Fatal("non-ErrExist publication failure was not retained")
	}
	rawTestNames(t, dir, "sentinel")
	if string(rawTestRead(t, sentinel)) != "keep" {
		t.Fatal("publication failure removed unrelated evidence")
	}
}

type rawFaultTemp struct {
	*os.File
	fault  error
	kind   string
	events *[]string
}

func (f *rawFaultTemp) Write(p []byte) (int, error) {
	*f.events = append(*f.events, "write")
	if f.kind == "write" {
		n, err := f.File.Write(p[:len(p)/2])
		return n, errors.Join(err, f.fault)
	}
	if f.kind == "short_write" {
		return f.File.Write(p[:len(p)-1])
	}
	return f.File.Write(p)
}
func (f *rawFaultTemp) Sync() error {
	*f.events = append(*f.events, "sync")
	if f.kind == "sync" {
		return f.fault
	}
	return f.File.Sync()
}
func (f *rawFaultTemp) Close() error {
	*f.events = append(*f.events, "close")
	err := f.File.Close()
	if f.kind == "close" {
		return errors.Join(err, f.fault)
	}
	return err
}

func TestRawWriteStageFailures(t *testing.T) {
	for _, kind := range []string{"write", "short_write", "sync", "close"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			sentinel := filepath.Join(dir, "sentinel")
			if err := os.WriteFile(sentinel, []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			fault := errors.New("write stage fault")
			events := []string{}
			published := false
			hash, path, err := storeRawBlobWith(dir, []byte("body"),
				func(dir, pattern string) (rawTempFile, error) {
					file, createErr := os.CreateTemp(dir, pattern)
					if createErr != nil {
						return nil, createErr
					}
					return &rawFaultTemp{File: file, fault: fault, kind: kind, events: &events}, nil
				}, func(old, target string) error { published = true; return os.Link(old, target) })
			expected := fault
			if kind == "short_write" {
				expected = io.ErrShortWrite
			}
			if !errors.Is(err, expected) {
				t.Fatalf("%s fault did not reject publication: %v", kind, err)
			}
			if hash != "" || path != "" || published {
				t.Fatal("failed write stage returned/published a usable blob")
			}
			closes := 0
			for _, event := range events {
				if event == "close" {
					closes++
				}
			}
			if closes != 1 {
				t.Fatalf("failed write stage closed owned temp %d times", closes)
			}
			rawTestNames(t, dir, "sentinel")
			if string(rawTestRead(t, sentinel)) != "keep" {
				t.Fatal("write-stage failure removed unrelated evidence")
			}
		})
	}
}

func TestRawWriteStageOrderAndSameDirectory(t *testing.T) {
	dir := t.TempDir()
	events := []string{}
	var file *os.File
	hash, path, err := storeRawBlobWith(dir, []byte("body"),
		func(dir, pattern string) (rawTempFile, error) {
			created, createErr := os.CreateTemp(dir, pattern)
			if createErr != nil {
				return nil, createErr
			}
			file = created
			return &rawFaultTemp{File: created, events: &events}, nil
		}, func(old, target string) error {
			events = append(events, "link")
			if filepath.Dir(old) != dir || filepath.Dir(target) != dir {
				t.Fatal("temporary publication crossed directories")
			}
			if _, writeErr := file.Write([]byte("late")); !errors.Is(writeErr, os.ErrClosed) {
				t.Fatal("publication began before owned temp closed")
			}
			return os.Link(old, target)
		})
	if err != nil || hash != rawTestHash([]byte("body")) || path != filepath.Join(dir, hash) {
		t.Fatalf("real staged publication failed: %v", err)
	}
	if !reflect.DeepEqual(events, []string{"write", "sync", "close", "link"}) {
		t.Fatalf("stage order = %v", events)
	}
	rawTestNames(t, dir, hash)
}

func TestRawCleanupFailurePreservesPublishedBlob(t *testing.T) {
	dir := t.TempDir()
	raw := []byte("body")
	hash, path, err := storeRawBlobWith(dir, raw, rawTestCreate,
		func(old, target string) error {
			if filepath.Dir(old) != dir {
				t.Fatal("owned temp escaped fixture")
			}
			if linkErr := os.Link(old, target); linkErr != nil {
				return linkErr
			}
			if removeErr := os.Remove(old); removeErr != nil {
				t.Fatal(removeErr)
			}
			if mkdirErr := os.Mkdir(old, 0700); mkdirErr != nil {
				t.Fatal(mkdirErr)
			}
			if writeErr := os.WriteFile(filepath.Join(old, "sentinel"), []byte("keep"), 0600); writeErr != nil {
				t.Fatal(writeErr)
			}
			return nil
		})
	if err == nil || hash != "" || path != "" {
		t.Fatal("owned-temp cleanup failure was silently accepted")
	}
	destination := filepath.Join(dir, rawTestHash(raw))
	if !bytes.Equal(rawTestRead(t, destination), raw) {
		t.Fatal("cleanup failure removed already-published valid evidence")
	}
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 2 {
		t.Fatal("cleanup failure scanned/deleted unrelated directory contents")
	}
	for _, entry := range entries {
		if entry.IsDir() && string(rawTestRead(t, filepath.Join(dir, entry.Name(), "sentinel"))) != "keep" {
			t.Fatal("cleanup failure removed owned-path child evidence")
		}
	}
}

type rawCleanupFaultTemp struct {
	*rawFaultTemp
	cleanupFault error
}

func (f *rawCleanupFaultTemp) Close() error {
	return errors.Join(f.rawFaultTemp.Close(), f.cleanupFault)
}

func TestRawCleanupCloseFailure(t *testing.T) {
	dir := t.TempDir()
	writeFault, closeFault := errors.New("write failed"), errors.New("cleanup close failed")
	events := []string{}
	published := false
	hash, path, err := storeRawBlobWith(dir, []byte("body"),
		func(dir, pattern string) (rawTempFile, error) {
			file, createErr := os.CreateTemp(dir, pattern)
			if createErr != nil {
				return nil, createErr
			}
			return &rawCleanupFaultTemp{rawFaultTemp: &rawFaultTemp{File: file, kind: "write", fault: writeFault, events: &events}, cleanupFault: closeFault}, nil
		}, func(string, string) error { published = true; return nil })
	if !errors.Is(err, writeFault) || !errors.Is(err, closeFault) {
		t.Fatal("cleanup close error erased or was erased by original write failure")
	}
	if hash != "" || path != "" || published {
		t.Fatal("failed cleanup exposed a usable blob")
	}
	rawTestNames(t, dir)
}
