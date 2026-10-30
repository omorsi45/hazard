package index

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/omorsi45/hazard/internal/config"
)

// fakeHead is an in-memory HeadReader.
type fakeHead struct {
	files    map[string]string // listed and readable
	readable map[string]string // readable but not listed (an ignored file)
	listErr  error
	readErr  map[string]error
}

func (f fakeHead) List() ([]string, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	paths := make([]string, 0, len(f.files))
	for p := range f.files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths, nil
}

func (f fakeHead) Read(rel string) ([]byte, error) {
	if err, ok := f.readErr[rel]; ok {
		return nil, err
	}
	if content, ok := f.files[rel]; ok {
		return []byte(content), nil
	}
	if content, ok := f.readable[rel]; ok {
		return []byte(content), nil
	}
	return nil, fmt.Errorf("read %s: %w", rel, fs.ErrNotExist)
}

func staleBase() map[string]string {
	return map[string]string{
		"go.mod":      goFixtureMod,
		"a/a.go":      "package a\n\nfunc A() int { return 1 }\n",
		"a/a_test.go": "package a\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {}\n",
		"b/b.go":      "package b\n",
		"README.md":   "# readme\n",
	}
}

// staleManifest records the relevant files of files the way Build does.
func staleManifest(t *testing.T, cfg config.Config, files map[string]string) *Manifest {
	t.Helper()
	digest, err := configDigest(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m := &Manifest{ConfigDigest: digest, Files: map[string]string{}}
	for p, content := range files {
		if isRelevant(p, cfg) {
			m.Files[p] = hexSHA(content)
		}
	}
	return m
}

func changedSet(paths ...string) map[string]bool {
	set := map[string]bool{}
	for _, p := range paths {
		set[p] = true
	}
	return set
}

func TestStaleForAppliesTheChangeSetRule(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		edit     func(head *fakeHead)
		changed  map[string]bool
		recorded func(cfg *config.Config) // configuration the manifest was built with
		current  func(cfg *config.Config) // configuration passed to StaleFor, on top of recorded
		want     string                   // "" means usable, else a substring of the reason
	}{
		{name: "identical head, no change set", want: ""},
		{name: "identical head, a change set naming files", changed: changedSet("a/a.go", "zzz.txt"), want: ""},
		{
			name:    "a changed file may differ from the index",
			edit:    func(h *fakeHead) { h.files["a/a.go"] = "package a\n\nfunc A() int { return 2 }\n" },
			changed: changedSet("a/a.go"),
			want:    "",
		},
		{
			name: "drift outside the change set",
			edit: func(h *fakeHead) { h.files["a/a.go"] = "package a\n\nfunc A() int { return 2 }\n" },
			want: "file changed: a/a.go",
		},
		{
			name:    "drift outside the change set while another file changed",
			edit:    func(h *fakeHead) { h.files["a/a.go"] = "package a\n\nfunc A() int { return 2 }\n" },
			changed: changedSet("b/b.go"),
			want:    "file changed: a/a.go",
		},
		{
			name: "recorded file removed outside the change set",
			edit: func(h *fakeHead) { delete(h.files, "b/b.go") },
			want: "file removed: b/b.go",
		},
		{
			name:    "recorded file removed inside the change set",
			edit:    func(h *fakeHead) { delete(h.files, "b/b.go") },
			changed: changedSet("b/b.go"),
			want:    "",
		},
		{
			name:    "renamed file: old path gone, new path unrecorded, both in the change set",
			edit:    func(h *fakeHead) { h.files["b/c.go"] = h.files["b/b.go"]; delete(h.files, "b/b.go") },
			changed: changedSet("b/b.go", "b/c.go"),
			want:    "",
		},
		{
			name:    "renamed file: the old path alone in the change set leaves the new path unrecorded",
			edit:    func(h *fakeHead) { h.files["b/c.go"] = h.files["b/b.go"]; delete(h.files, "b/b.go") },
			changed: changedSet("b/b.go"),
			want:    "new file: b/c.go",
		},
		{
			name: "unrecorded relevant file outside the change set",
			edit: func(h *fakeHead) { h.files["c/c.go"] = "package c\n" },
			want: "new file: c/c.go",
		},
		{
			name:    "unrecorded relevant file outside the change set while another file changed",
			edit:    func(h *fakeHead) { h.files["c/c.go"] = "package c\n" },
			changed: changedSet("a/a.go"),
			want:    "new file: c/c.go",
		},
		{
			name:    "unrecorded relevant file inside the change set",
			edit:    func(h *fakeHead) { h.files["c/c.go"] = "package c\n" },
			changed: changedSet("c/c.go"),
			want:    "",
		},
		{
			name: "unrecorded test file outside the change set",
			edit: func(h *fakeHead) { h.files["b/b_test.go"] = "package b\n" },
			want: "new file: b/b_test.go",
		},
		{
			name: "unrecorded irrelevant file",
			edit: func(h *fakeHead) { h.files["notes.txt"] = "x"; h.files["README.md"] = "changed" },
			want: "",
		},
		{
			name:     "unrecorded file of a disabled language",
			edit:     func(h *fakeHead) { h.files["tools/x.py"] = "x = 1\n" },
			recorded: func(cfg *config.Config) { cfg.Languages.Python.Enabled = false },
			want:     "",
		},
		{
			name: "unrecorded Python file with Python enabled",
			edit: func(h *fakeHead) { h.files["tools/x.py"] = "x = 1\n" },
			want: "new file: tools/x.py",
		},
		{
			name: "unrecorded config file that is not listed is compared by digest, not as head content",
			edit: func(h *fakeHead) { h.readable[".hazard/hazard.yml"] = "x: 1\n" },
			want: "",
		},
		{
			name:    "unrecorded config file that is not listed, inside the change set",
			edit:    func(h *fakeHead) { h.readable[".hazard/hazard.yml"] = "x: 1\n" },
			changed: changedSet(".hazard/hazard.yml"),
			want:    "",
		},
		{
			name: "unrecorded hazard.yml that is listed is compared by digest, not as head content",
			edit: func(h *fakeHead) { h.files["hazard.yml"] = "version: 1\n" },
			want: "",
		},
		{
			name: "unrecorded .hazard/hazard.yml that is listed is compared by digest, not as head content",
			edit: func(h *fakeHead) { h.files[".hazard/hazard.yml"] = "version: 1\n" },
			want: "",
		},
		{
			name:    "changed configuration with every file identical",
			current: func(cfg *config.Config) { cfg.Impact.MaxHops++ },
			changed: changedSet("a/a.go", "a/a_test.go", "b/b.go", "go.mod"),
			want:    "config changed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			files := staleBase()
			cfg := config.Defaults()
			if tc.recorded != nil {
				tc.recorded(&cfg)
			}
			m := staleManifest(t, cfg, files)
			head := fakeHead{files: files, readable: map[string]string{}}
			if tc.edit != nil {
				tc.edit(&head)
			}
			if tc.current != nil {
				tc.current(&cfg)
			}
			reason, err := m.StaleFor(cfg, tc.changed, head)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" && reason != "" {
				t.Fatalf("unexpectedly stale: %q", reason)
			}
			if tc.want != "" && !strings.Contains(reason, tc.want) {
				t.Fatalf("reason = %q, want it to contain %q", reason, tc.want)
			}
		})
	}
}

// configHeads are the states a configuration file can have at the selection
// head. The engine loads the configuration from the working tree (or --config),
// never from the head, so none of them says anything about the configuration.
var configHeads = []struct {
	name string
	edit func(h *fakeHead, rel string)
}{
	{"absent from the head (ignored, never committed)", func(h *fakeHead, rel string) { delete(h.files, rel) }},
	{"present at the head with other bytes", func(h *fakeHead, rel string) { h.files[rel] = "version: 1\n# edited at the head\n" }},
	{"present at the head with the recorded bytes", func(h *fakeHead, rel string) {}},
}

// configManifest records a manifest of staleBase plus the configuration file
// rel, the way Build does for a config that is ignored by git.
func configManifest(t *testing.T, rel string) (*Manifest, map[string]string) {
	t.Helper()
	files := staleBase()
	files[rel] = "version: 1\n"
	m := staleManifest(t, config.Defaults(), files)
	if _, ok := m.Files[rel]; !ok {
		t.Fatalf("premise: the manifest does not record %s: %v", rel, m.Files)
	}
	return m, files
}

// TestStaleForComparesTheConfigurationByDigestNotAsHeadContent: the configuration
// is not repository content at a revision (D055 condition 1 compares the digest
// of the loaded configuration), so a recorded config file that the head lacks or
// holds with other bytes does not make the index stale while the configuration
// is the one the index was built with.
func TestStaleForComparesTheConfigurationByDigestNotAsHeadContent(t *testing.T) {
	t.Parallel()
	for _, rel := range []string{"hazard.yml", ".hazard/hazard.yml"} {
		for _, hc := range configHeads {
			t.Run(rel+" "+hc.name, func(t *testing.T) {
				t.Parallel()
				m, files := configManifest(t, rel)
				head := fakeHead{files: files, readable: map[string]string{}}
				hc.edit(&head, rel)
				reason, err := m.StaleFor(config.Defaults(), nil, head)
				if err != nil {
					t.Fatal(err)
				}
				if reason != "" {
					t.Fatalf("StaleFor = %q, want the index to be usable: the configuration digest is unchanged", reason)
				}
			})
		}
	}
}

// TestStaleForStillCatchesAChangedConfiguration is the safety side of the
// exemption above: with the same heads, a configuration whose digest differs
// from the recorded one is always "config changed", whatever the head holds.
func TestStaleForStillCatchesAChangedConfiguration(t *testing.T) {
	t.Parallel()
	for _, rel := range []string{"hazard.yml", ".hazard/hazard.yml"} {
		for _, hc := range configHeads {
			t.Run(rel+" "+hc.name, func(t *testing.T) {
				t.Parallel()
				m, files := configManifest(t, rel)
				head := fakeHead{files: files, readable: map[string]string{}}
				hc.edit(&head, rel)
				changed := config.Defaults()
				changed.Impact.MaxHops++
				reason, err := m.StaleFor(changed, nil, head)
				if err != nil {
					t.Fatal(err)
				}
				if reason != "config changed" {
					t.Fatalf("StaleFor = %q, want %q", reason, "config changed")
				}
			})
		}
	}
}

func TestStaleForReportsErrorsInsteadOfFreshness(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	for _, tc := range []struct {
		name string
		head func(files map[string]string) fakeHead
	}{
		{"list fails", func(files map[string]string) fakeHead {
			return fakeHead{files: files, listErr: boom}
		}},
		{"read of a recorded file fails", func(files map[string]string) fakeHead {
			return fakeHead{files: files, readErr: map[string]error{"a/a.go": boom}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			files := staleBase()
			m := staleManifest(t, config.Defaults(), files)
			reason, err := m.StaleFor(config.Defaults(), nil, tc.head(files))
			if err == nil || !errors.Is(err, boom) || reason != "" {
				t.Fatalf("StaleFor = %q, %v; want an error wrapping the read failure and no reason", reason, err)
			}
		})
	}
}

func TestWorkTreeHeadListsAndReadsTheWorkingTree(t *testing.T) {
	t.Parallel()
	shared, _ := sharedGoRepo(t)
	dir := copyRepo(t, shared)
	if err := os.Remove(filepath.Join(dir, "b", "b.go")); err != nil { // tracked, now deleted
		t.Fatal(err)
	}
	writeFiles(t, dir, map[string]string{
		"c/c.go":             "package c\n",                              // untracked, not ignored
		"a/a.go":             "package a\n\nfunc A() int { return 2 }\n", // tracked, modified
		"d/d.txt":            "notes",                                    // untracked, irrelevant
		".hazard/hazard.yml": "x: 1\n",                                   // ignored
	})
	head := workTreeHead{root: dir}
	list, err := head.List()
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(list)
	want := []string{".gitignore", "README.md", "a/a.go", "a/a_test.go", "c/c.go", "d/d.txt", "go.mod"}
	if !reflect.DeepEqual(list, want) {
		t.Errorf("List = %q, want %q (no deleted, ignored or .git entries)", list, want)
	}
	got, err := head.Read("a/a.go")
	if err != nil || string(got) != "package a\n\nfunc A() int { return 2 }\n" {
		t.Errorf("Read(a/a.go) = %q, %v; want the working tree content", got, err)
	}
	if _, err := head.Read("b/b.go"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Read of a deleted file = %v, want an error wrapping fs.ErrNotExist", err)
	}
	if got, err := head.Read(".hazard/hazard.yml"); err != nil || string(got) != "x: 1\n" {
		t.Errorf("Read of an ignored but present file = %q, %v", got, err)
	}
	outside := []string{"../outside.go", "/etc/passwd", "a/../../outside.go", ""}
	if runtime.GOOS == "windows" { // a volume and a backslash are path syntax only on Windows
		outside = append(outside, "C:/Windows/win.ini", `a\..\..\outside.go`)
	}
	for _, rel := range outside {
		if got, err := head.Read(rel); err == nil || errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Read(%q) = %q, %v; want a non-ErrNotExist error for a path outside the repository", rel, got, err)
		}
	}
}

func TestStaleKeepsTheWorkingTreeContract(t *testing.T) {
	t.Parallel()
	shared, m := sharedGoRepo(t)
	dir := copyRepo(t, shared)
	writeFiles(t, dir, map[string]string{"a/a.go": "package a\n\nfunc A() int { return 2 }\n"})
	cfg := config.Defaults()
	reason, err := m.Stale(dir, cfg)
	if err != nil || !strings.Contains(reason, "file changed: a/a.go") {
		t.Fatalf("Stale = %q, %v; want it to report a/a.go", reason, err)
	}
	// With that file in the change set the same working tree is usable.
	head := workTreeHead{root: dir}
	if reason, err := m.StaleFor(cfg, changedSet("a/a.go"), head); err != nil || reason != "" {
		t.Fatalf("StaleFor with a/a.go changed = %q, %v; want usable", reason, err)
	}
	if reason, err := m.StaleFor(cfg, nil, head); err != nil || reason != "file changed: a/a.go" {
		t.Fatalf("StaleFor with an empty change set = %q, %v; want the same reason as Stale", reason, err)
	}
	// A new file elsewhere still makes it stale.
	writeFiles(t, dir, map[string]string{"c/c.go": "package c\n"})
	if reason, err := m.StaleFor(cfg, changedSet("a/a.go"), head); err != nil || reason != "new file: c/c.go" {
		t.Fatalf("StaleFor with an unrecorded file outside the change set = %q, %v", reason, err)
	}
}
