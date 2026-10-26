package index

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/omorsi45/hazard/internal/runner"
)

// pythonEnv makes interpreter output independent of the console code page.
var pythonEnv = []string{"PYTHONIOENCODING=utf-8", "PYTHONUTF8=1"}

// pythonNoTestsExit is the pytest exit status for "no tests collected".
const pythonNoTestsExit = 5

// pythonDiscovery is the result of collecting pytest tests.
type pythonDiscovery struct {
	Tests       []Test
	Diagnostics []string
	Incomplete  bool // some collected node could not be given an identity
	Commands    []Command
}

var (
	pytestSummary   = regexp.MustCompile(`(?m)^(\d+) tests? collected\b`)
	pytestFileCount = regexp.MustCompile(`^\S.*\.py: \d+$`)
)

// parseCollect extracts node IDs from `pytest --collect-only -q` output. It
// fails when the output cannot be trusted to list every test, rather than
// returning an inventory that is silently short.
func parseCollect(output string) ([]string, error) {
	var nodes []string
	started := false
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			if started {
				break
			}
			continue
		}
		started = true
		if pytestFileCount.MatchString(line) {
			return nil, fmt.Errorf("pytest printed per-file counts instead of node IDs (does addopts add -q?): %q", line)
		}
		if strings.Contains(line, ".py::") && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "ERROR") {
			nodes = append(nodes, line)
		}
	}
	if m := pytestSummary.FindStringSubmatch(output); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil && n != len(nodes) {
			return nil, fmt.Errorf("pytest collected %d tests but %d node IDs were parsed", n, len(nodes))
		}
	}
	return nodes, nil
}

// discoverPython collects pytest tests under testpaths with the interpreter.
// Collection can execute test module initialization code; that is inherent to
// pytest and documented in the command help.
func discoverPython(ctx context.Context, repoDir, python string, testpaths []string) (pythonDiscovery, error) {
	var out pythonDiscovery
	argv := append([]string{python, "-m", "pytest", "--collect-only", "-q", "-p", "no:cacheprovider"}, testpaths...)
	res, err := runCommand(ctx, repoDir, pythonEnv, argv...)
	out.Commands = append(out.Commands, res.Command)
	if err != nil {
		return out, err
	}
	switch res.Command.Exit {
	case 0, pythonNoTestsExit:
	default:
		return out, fmt.Errorf("pytest collection failed with exit status %d: %s", res.Command.Exit, tail(string(res.Stdout)+string(res.Stderr)))
	}
	nodes, err := parseCollect(string(res.Stdout))
	if err != nil {
		return out, err
	}
	if res.Command.Exit == 0 && len(nodes) == 0 {
		return out, fmt.Errorf("pytest exited 0 but no node IDs were parsed: %s", tail(string(res.Stdout)))
	}
	cache := map[string][]pyStmt{}
	for _, node := range nodes {
		identity, err := runner.PythonIdentityFromNodeID(node)
		if err != nil {
			out.Diagnostics = append(out.Diagnostics, fmt.Sprintf("python test %q has no usable identity: %v", node, err))
			out.Incomplete = true
			continue
		}
		file, classes, function := splitNode(node)
		stmts, seen := cache[file]
		if !seen {
			if src, readErr := os.ReadFile(filepath.Join(repoDir, filepath.FromSlash(file))); readErr == nil {
				stmts = lexPython(src)
			}
			cache[file] = stmts
		}
		test := Test{ID: identity.ID, Language: "python", Package: identity.Package, Name: identity.Name, File: identity.File, RunnerID: identity.RunnerID, Unstable: identity.Unstable, Purpose: function}
		if fn, ok := findPyFunc(stmts, classes, function); ok {
			test.Line, test.Asserts = fn.Line, fn.Asserts
			if fn.Purpose != "" {
				test.Purpose = fn.Purpose
			}
		}
		out.Tests = append(out.Tests, test)
	}
	sort.Slice(out.Tests, func(i, j int) bool { return out.Tests[i].ID < out.Tests[j].ID })
	return out, nil
}

// splitNode splits "file.py::Class::test_a[params]" into its file, class path
// and function name without parameters.
func splitNode(node string) (file string, classes []string, function string) {
	head := node
	if i := strings.IndexByte(node, '['); i >= 0 {
		head = node[:i]
	}
	parts := strings.Split(head, "::")
	return parts[0], parts[1 : len(parts)-1], parts[len(parts)-1]
}

// tail returns the last lines of command output for an error message.
func tail(s string) string {
	s = strings.TrimSpace(s)
	const limit = 1500
	if len(s) > limit {
		s = "..." + s[len(s)-limit:]
	}
	return s
}
