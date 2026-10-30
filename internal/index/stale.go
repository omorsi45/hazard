package index

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/omorsi45/hazard/internal/config"
)

// HeadReader reads the selection head: the working tree or a commit.
type HeadReader interface {
	// List returns the repo-relative forward-slash paths of every tracked or
	// untracked non-ignored file at head.
	List() ([]string, error)
	// Read returns the content of rel at head. An absent file is an error
	// wrapping fs.ErrNotExist.
	Read(rel string) ([]byte, error)
}

// StaleFor returns a non-empty reason when the manifest cannot describe the
// repository at head for the change set changed. The rule is D055: the config
// digest is equal, every recorded file outside changed is byte-identical at
// head, and no unrecorded relevant file exists at head outside changed.
// changed holds the Path and OldPath of every changed file; files inside it
// are exempt from both content checks, because a graph edge out of a changed
// file starts at a package the change already selects. The two configuration
// files (isConfigPath) are exempt from both content checks too: the
// configuration is compared by digest, not as head content. The engine loads it
// from the working tree (or --config), never from the head, and git may ignore
// it, so a commit need not hold it. An error means staleness could not be
// determined and must be treated as stale.
func (m *Manifest) StaleFor(cfg config.Config, changed map[string]bool, head HeadReader) (reason string, err error) {
	digest, err := configDigest(cfg)
	if err != nil {
		return "", err
	}
	if digest != m.ConfigDigest {
		return "config changed", nil
	}
	recorded := make([]string, 0, len(m.Files))
	for p := range m.Files {
		recorded = append(recorded, p)
	}
	sort.Strings(recorded)
	for _, p := range recorded {
		if changed[p] || isConfigPath(p) {
			continue
		}
		data, err := head.Read(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return "file removed: " + p, nil
		case err != nil:
			return "", fmt.Errorf("read %s at head: %w", p, err)
		case sha(data) != m.Files[p]:
			return "file changed: " + p, nil
		}
	}
	listed, err := head.List()
	if err != nil {
		return "", fmt.Errorf("list files at head: %w", err)
	}
	candidates := append([]string(nil), listed...)
	sort.Strings(candidates)
	for _, p := range candidates {
		if _, ok := m.Files[p]; ok || changed[p] || isConfigPath(p) || !isRelevant(p, cfg) {
			continue
		}
		return "new file: " + p, nil
	}
	return "", nil
}

// isConfigPath reports whether rel is one of the two configuration locations.
func isConfigPath(rel string) bool {
	return rel == "hazard.yml" || rel == ".hazard/hazard.yml"
}

// workTreeHead is the working tree of a git repository as a HeadReader.
type workTreeHead struct{ root string }

// List returns the tracked and untracked non-ignored files that exist in the
// working tree: a tracked file deleted from disk and a directory (a submodule
// entry) are not listed.
func (w workTreeHead) List() ([]string, error) {
	res, err := runCommand(context.Background(), w.root, nil, "git", "-c", "core.quotePath=false", "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	if res.Command.Exit != 0 {
		return nil, fmt.Errorf("git ls-files failed with exit status %d: %s", res.Command.Exit, tail(string(res.Stderr)))
	}
	var paths []string
	for _, rel := range strings.Split(string(res.Stdout), "\x00") {
		if rel == "" {
			continue
		}
		if info, err := os.Stat(filepath.Join(w.root, filepath.FromSlash(rel))); err == nil && !info.IsDir() {
			paths = append(paths, rel)
		}
	}
	return paths, nil
}

// Read returns the working tree content of rel. A path that is not a plain
// repo-relative slash path (absolute, rooted at a volume, or containing "..")
// is rejected with an error that does not wrap fs.ErrNotExist.
func (w workTreeHead) Read(rel string) ([]byte, error) {
	native := filepath.FromSlash(rel)
	if !fs.ValidPath(rel) || filepath.VolumeName(native) != "" || filepath.Separator == '\\' && strings.Contains(rel, `\`) {
		return nil, fmt.Errorf("path %q is not a path inside the repository", rel)
	}
	return os.ReadFile(filepath.Join(w.root, native))
}
