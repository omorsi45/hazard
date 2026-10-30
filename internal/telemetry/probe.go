package telemetry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/omorsi45/hazard/internal/config"
	"github.com/omorsi45/hazard/internal/runner"
)

// Runners are the native adapters a probe drives.
type Runners struct {
	Go, Python func(ctx context.Context, req runner.Request) (runner.Result, error)
}

// Prober runs one test at one commit in an isolated worktree.
type Prober struct {
	Git     Git
	Config  config.Config
	Runners Runners
}

// cleanupTimeout bounds the removal of a worktree after its probe, which must
// still happen when the probe itself was canceled.
const cleanupTimeout = time.Minute

// Probe runs testID at commit and reports whether it failed there. The commit
// is checked out detached in a temporary worktree (git worktree add --detach)
// that is removed afterwards (git worktree remove --force); the repository's
// own work tree is never touched.
//
// A test that does not exist at the commit, or that is skipped there, is not
// failing. A commit where the test could not be judged (the code does not
// build, no report was written) is StateUnknown with no error.
func (p Prober) Probe(ctx context.Context, commit, testID string) (state State, err error) {
	lang, rest, _ := strings.Cut(testID, ":")
	first, name, ok := strings.Cut(rest, ":")
	if (lang != "go" && lang != "py") || !ok || first == "" || name == "" || strings.HasPrefix(first, "-") {
		return StateUnknown, fmt.Errorf("test ID %q cannot be run: want go:<package>:<name> or py:<module.class>:<name>", testID)
	}
	if err := ctx.Err(); err != nil {
		return StateUnknown, err
	}

	tmp, err := os.MkdirTemp("", "hazard-bisect-")
	if err != nil {
		return StateUnknown, fmt.Errorf("create bisect directory: %w", err)
	}
	defer func() {
		if rmErr := os.RemoveAll(tmp); rmErr != nil {
			err = errors.Join(err, fmt.Errorf("remove %s: %w", tmp, rmErr))
		}
	}()
	wt := filepath.Join(tmp, "wt")
	if _, _, err := p.Git.run(ctx, "worktree", "add", "--detach", wt, commit); err != nil {
		return StateUnknown, fmt.Errorf("check out %s: %w", commit, err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		if _, _, rmErr := p.Git.run(cleanup, "worktree", "remove", "--force", wt); rmErr != nil {
			err = errors.Join(err, fmt.Errorf("remove worktree %s: %w", wt, rmErr))
		}
	}()

	req := runner.Request{RepoDir: wt, JUnitPath: filepath.Join(tmp, "junit.xml"), Stdout: io.Discard, Stderr: io.Discard}
	run := p.Runners.Go
	identityLang, packageHint := "go", ""
	switch lang {
	case "go":
		req.Targets = []runner.Target{{ID: testID, Language: "go", Package: first, Name: name}}
		req.GoTags = p.Config.Languages.Go.Tags
		packageHint = first
	default:
		node, found := pythonNode(wt, first, name)
		if !found {
			return StatePass, nil // the file is not there yet: the test does not exist at this commit
		}
		req.Targets = []runner.Target{{ID: testID, Language: "python", Package: first, RunnerID: node}}
		req.Testpaths, req.PytestArgs = p.Config.Languages.Python.Testpaths, p.Config.Languages.Python.PytestArgs
		run, identityLang = p.Runners.Python, "python"
	}
	res, err := run(ctx, req)
	if err != nil {
		return StateUnknown, fmt.Errorf("run %s at %s: %w", testID, commit, err)
	}
	if res.Abstained {
		return StatePass, nil // pytest did not find the node: the test does not exist at this commit
	}
	f, err := os.Open(req.JUnitPath)
	if errors.Is(err, fs.ErrNotExist) {
		return StateUnknown, nil
	}
	if err != nil {
		return StateUnknown, err
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			err = errors.Join(err, cerr)
		}
	}()
	cases, err := runner.ParseJUnit(f)
	if err != nil {
		return StateUnknown, fmt.Errorf("read the report of %s at %s: %w", testID, commit, err)
	}
	found := false
	for _, c := range cases {
		hint := packageHint
		if identityLang == "go" {
			hint = c.Classname
		}
		id, idErr := runner.NormalizeJUnitCase(identityLang, hint, c)
		if idErr != nil || id.ID != testID {
			continue
		}
		if c.Status == "fail" || c.Status == "error" {
			return StateFail, nil
		}
		found = true
	}
	if found || res.ExitCode == 0 {
		return StatePass, nil
	}
	return StateUnknown, nil
}

// pythonNode derives the pytest node ID of a test from the files of the
// worktree: qualifier "tests.test_x.TestK" is file tests/test_x.py and class
// TestK when that file exists. found is false when no file matches.
func pythonNode(worktree, qualifier, name string) (node string, found bool) {
	parts := strings.Split(qualifier, ".")
	for k := len(parts); k >= 1; k-- {
		rel := path.Join(parts[:k]...) + ".py"
		info, err := os.Stat(filepath.Join(worktree, filepath.FromSlash(rel)))
		if err != nil || info.IsDir() {
			continue
		}
		node = rel
		for _, class := range parts[k:] {
			node += "::" + class
		}
		return node + "::" + name, true
	}
	return "", false
}
