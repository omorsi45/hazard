package index

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/omorsi45/hazard/internal/config"
	"github.com/omorsi45/hazard/internal/graph"
)

// Build collects the inventory, graph and (optionally) coverage for repoDir.
// A failed or canceled build returns an error and a nil manifest; the caller
// must not publish. Failed discovery is never turned into an empty inventory.
func Build(ctx context.Context, repoDir string, cfg config.Config, opts Options) (*Manifest, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := filepath.Abs(repoDir)
	if err != nil {
		return nil, err
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	python := opts.Python
	if python == "" {
		python = "python"
	}
	m := &Manifest{
		Schema:   SchemaVersion,
		RepoRoot: filepath.ToSlash(root),
		Tools:    map[string]string{},
		Graph:    &graph.Graph{Packages: map[string]*graph.Package{}},
		Complete: true,
	}
	if m.ConfigDigest, err = configDigest(cfg); err != nil {
		return nil, err
	}
	head, err := runCommand(ctx, root, nil, "git", "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	m.Commands = append(m.Commands, head.Command)
	if head.Command.Exit == 0 {
		m.HeadSHA = strings.TrimSpace(string(head.Stdout))
	} else {
		m.Diagnostics = append(m.Diagnostics, "git: repository has no commit; HeadSHA is empty")
	}
	status, err := runCommand(ctx, root, nil, "git", "status", "--porcelain=v1", "-z", "--untracked-files=all", "--", ".", ":(exclude).hazard")
	if err != nil {
		return nil, err
	}
	m.Commands = append(m.Commands, status.Command)
	if status.Command.Exit != 0 {
		return nil, fmt.Errorf("git status failed with exit status %d: %s", status.Command.Exit, tail(string(status.Stderr)))
	}
	m.Dirty = len(bytes.TrimSpace(status.Stdout)) > 0

	files, cmds, err := relevantFiles(ctx, root, cfg)
	if err != nil {
		return nil, err
	}
	m.Files = files
	m.Commands = append(m.Commands, cmds...)
	m.ContentDigest = contentDigest(files)

	var graphs []*graph.Graph
	var tests []Test
	if cfg.Languages.Go.Enabled {
		switch _, statErr := os.Stat(filepath.Join(root, "go.mod")); {
		case statErr == nil:
			goTests, goGraph, err := buildGo(ctx, root, cfg, m)
			if err != nil {
				return nil, err
			}
			graphs, tests = append(graphs, goGraph), append(tests, goTests...)
		case hasSource(m.Files, ".go"):
			// Go sources exist but nothing could be analyzed: the graph and the
			// inventory are missing them, so selection must run everything.
			m.Complete = false
			m.Diagnostics = append(m.Diagnostics, "go: Go source files exist but there is no go.mod in the repository root; Go scope skipped")
		default:
			m.Diagnostics = append(m.Diagnostics, "go: absent")
		}
	}
	if cfg.Languages.Python.Enabled {
		pyTests, pyGraph, err := buildPython(ctx, root, python, cfg, m)
		if err != nil {
			return nil, err
		}
		if pyGraph != nil {
			graphs = append(graphs, pyGraph)
		}
		tests = append(tests, pyTests...)
	}
	m.Graph = graph.Merge(graphs...)
	sort.Slice(tests, func(i, j int) bool { return tests[i].ID < tests[j].ID })
	for i := 1; i < len(tests); i++ {
		if tests[i].ID == tests[i-1].ID {
			return nil, fmt.Errorf("duplicate test ID %s (%s and %s)", tests[i].ID, tests[i-1].File, tests[i].File)
		}
	}
	m.Tests = tests
	if opts.Coverage {
		if err := addCoverage(ctx, root, python, cfg, m); err != nil {
			return nil, err
		}
	}
	m.CreatedAt = time.Now().UTC()
	return m, nil
}

func buildGo(ctx context.Context, root string, cfg config.Config, m *Manifest) ([]Test, *graph.Graph, error) {
	version, err := runCommand(ctx, root, nil, "go", "version")
	if err != nil {
		return nil, nil, err
	}
	m.Commands = append(m.Commands, version.Command)
	if fields := strings.Fields(string(version.Stdout)); version.Command.Exit == 0 && len(fields) >= 3 {
		m.Tools["go"] = fields[2]
	}
	start := time.Now()
	g, err := graph.LoadGo(ctx, root, cfg.Languages.Go.Packages, cfg.Languages.Go.Tags)
	if err != nil {
		return nil, nil, fmt.Errorf("go package discovery: %w", err)
	}
	m.Commands = append(m.Commands, Command{Argv: append([]string{"go"}, graph.GoListArgs(cfg.Languages.Go.Packages, cfg.Languages.Go.Tags)...), Dir: root, Seconds: time.Since(start).Seconds()})
	tests, err := discoverGo(root, g)
	if err != nil {
		return nil, nil, err
	}
	return tests, g, nil
}

func buildPython(ctx context.Context, root, python string, cfg config.Config, m *Manifest) ([]Test, *graph.Graph, error) {
	var g *graph.Graph
	for rel := range m.Files {
		if strings.HasSuffix(rel, ".py") {
			roots := []string{"."}
			if info, err := os.Stat(filepath.Join(root, "src")); err == nil && info.IsDir() {
				roots = append(roots, "src")
			}
			loaded, err := graph.LoadPython(root, roots)
			var unresolved *graph.UnresolvedError
			switch {
			case errors.As(err, &unresolved):
				ids := make([]string, 0, len(unresolved.Modules))
				for id := range unresolved.Modules {
					ids = append(ids, id)
				}
				sort.Strings(ids)
				for _, id := range ids {
					m.Diagnostics = append(m.Diagnostics, "python: "+id+" is an unknown dependency: "+unresolved.Modules[id])
				}
			case err != nil:
				return nil, nil, fmt.Errorf("python module graph: %w", err)
			}
			g = loaded
			break
		}
	}
	var existing []string
	for _, tp := range cfg.Languages.Python.Testpaths {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(tp))); err == nil {
			existing = append(existing, tp)
		}
	}
	if len(existing) == 0 {
		if hasSource(m.Files, ".py") {
			// Python sources exist but no test could be collected from them.
			m.Complete = false
			m.Diagnostics = append(m.Diagnostics, fmt.Sprintf("python: Python source files exist but none of the configured testpaths exist (%s); no Python tests collected", strings.Join(cfg.Languages.Python.Testpaths, ", ")))
		} else {
			m.Diagnostics = append(m.Diagnostics, "python: absent")
		}
		return nil, g, nil
	}
	for _, probe := range []struct {
		key  string
		argv []string
	}{
		{"python", []string{python, "--version"}},
		{"pytest", []string{python, "-m", "pytest", "--version"}},
	} {
		res, err := runCommand(ctx, root, pythonEnv, probe.argv...)
		if err != nil {
			return nil, nil, err
		}
		m.Commands = append(m.Commands, res.Command)
		if version := toolVersion(res); version != "" {
			m.Tools[probe.key] = version
		}
	}
	found, err := discoverPython(ctx, root, python, existing)
	m.Commands = append(m.Commands, found.Commands...)
	if err != nil {
		return nil, nil, err
	}
	m.Diagnostics = append(m.Diagnostics, found.Diagnostics...)
	if found.Incomplete {
		m.Complete = false
	}
	return found.Tests, g, nil
}

// toolVersion returns the version number from "Python 3.11.1" or "pytest 8.4.2"
// style output, which older tools print on stderr.
func toolVersion(res runResult) string {
	if res.Command.Exit != 0 {
		return ""
	}
	fields := strings.Fields(string(res.Stdout) + " " + string(res.Stderr))
	if len(fields) >= 2 {
		return fields[1]
	}
	return ""
}

func configDigest(cfg config.Config) (string, error) {
	data, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	return sha(data), nil
}

func sha(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func contentDigest(files map[string]string) string {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var b strings.Builder
	for _, p := range paths {
		b.WriteString(p + "\x00" + files[p] + "\n")
	}
	return sha([]byte(b.String()))
}

// relevantFiles hashes every tracked or untracked non-ignored file of the
// enabled languages that can change an index, plus the config file.
func relevantFiles(ctx context.Context, root string, cfg config.Config) (map[string]string, []Command, error) {
	res, err := runCommand(ctx, root, nil, "git", "-c", "core.quotePath=false", "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return nil, nil, err
	}
	if res.Command.Exit != 0 {
		return nil, nil, fmt.Errorf("git ls-files failed with exit status %d: %s", res.Command.Exit, tail(string(res.Stderr)))
	}
	files := map[string]string{}
	candidates := strings.Split(string(res.Stdout), "\x00")
	candidates = append(candidates, "hazard.yml", ".hazard/hazard.yml")
	for _, rel := range candidates {
		if rel == "" || !isRelevant(rel, cfg) {
			continue
		}
		full := filepath.Join(root, filepath.FromSlash(rel))
		if info, err := os.Stat(full); err != nil || info.IsDir() {
			continue
		}
		data, err := os.ReadFile(full)
		if err != nil {
			return nil, nil, err
		}
		files[rel] = sha(data)
	}
	return files, []Command{res.Command}, nil
}

// hasSource reports whether files (the relevant files of the enabled
// languages) contains a file with the given source extension.
func hasSource(files map[string]string, ext string) bool {
	for rel := range files {
		if strings.HasSuffix(rel, ext) {
			return true
		}
	}
	return false
}

func isRelevant(rel string, cfg config.Config) bool {
	base := path.Base(rel)
	if rel == "hazard.yml" || rel == ".hazard/hazard.yml" {
		return true
	}
	if cfg.Languages.Go.Enabled && (strings.HasSuffix(rel, ".go") || base == "go.mod" || base == "go.sum") {
		return true
	}
	return cfg.Languages.Python.Enabled && (strings.HasSuffix(rel, ".py") || base == "pyproject.toml" || base == "conftest.py")
}

// Publish atomically writes m to <repoDir>/.hazard/index/manifest.json: the
// manifest is encoded first, written to a temporary file in the same
// directory, synced, closed and renamed over the previous manifest.
func Publish(repoDir string, m *Manifest) error {
	if m == nil || m.Schema != SchemaVersion {
		return errors.New("refusing to publish a manifest without the current schema version")
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	dir := filepath.Join(repoDir, ".hazard", "index")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "manifest-*.tmp")
	if err != nil {
		return err
	}
	cleanup := func(cause error) error {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return cause
	}
	if _, err := tmp.Write(data); err != nil {
		return cleanup(err)
	}
	if err := tmp.Sync(); err != nil {
		return cleanup(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, "manifest.json")); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return nil
}

// Load reads and validates the published manifest.
func Load(repoDir string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(repoDir, ".hazard", "index", "manifest.json"))
	if err != nil {
		return nil, fmt.Errorf("load index manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("decode index manifest: %w", err)
	}
	if m.Schema != SchemaVersion {
		return nil, fmt.Errorf("unsupported index manifest schema %d (want %d)", m.Schema, SchemaVersion)
	}
	if m.Graph == nil {
		return nil, errors.New("index manifest has no graph")
	}
	return &m, nil
}

// Stale returns a non-empty reason when the manifest no longer describes the
// repository: the config digest differs, a recorded file changed or vanished,
// or a new relevant file appeared. An error means staleness could not be
// determined and must be treated as stale.
func (m *Manifest) Stale(repoDir string, cfg config.Config) (reason string, err error) {
	digest, err := configDigest(cfg)
	if err != nil {
		return "", err
	}
	if digest != m.ConfigDigest {
		return "config changed", nil
	}
	root, err := filepath.Abs(repoDir)
	if err != nil {
		return "", err
	}
	current, _, err := relevantFiles(context.Background(), root, cfg)
	if err != nil {
		return "", err
	}
	recorded := make([]string, 0, len(m.Files))
	for p := range m.Files {
		recorded = append(recorded, p)
	}
	sort.Strings(recorded)
	for _, p := range recorded {
		hash, ok := current[p]
		if !ok {
			return "file removed: " + p, nil
		}
		if hash != m.Files[p] {
			return "file changed: " + p, nil
		}
	}
	added := make([]string, 0)
	for p := range current {
		if _, ok := m.Files[p]; !ok {
			added = append(added, p)
		}
	}
	sort.Strings(added)
	if len(added) > 0 {
		return "new file: " + added[0], nil
	}
	return "", nil
}
