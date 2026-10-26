package runner

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"
)

// pytestPlugin is written to <RepoDir>/.hazard/hazard_pytest.py before a run.
//
//go:embed plugin/hazard_pytest.py
var pytestPlugin []byte

// pytestMissingReason is Result.Reason when selected node IDs were not found.
const pytestMissingReason = "pytest_missing_ids"

// RunPytest runs pytest through the embedded selection plugin and writes JUnit
// XML to the request's JUnitPath.
//
// A narrowed run passes the node IDs in a UTF-8 file named by HAZARD_SELECTION,
// never on argv. If any selected ID matches nothing, the plugin ends the run
// before a test executes and RunPytest reports Abstained with the sorted
// Missing IDs, whatever the exit code. Otherwise ExitCode is pytest's own.
func RunPytest(ctx context.Context, req Request) (res Result, err error) {
	if err := ValidatePassthrough("python", req.Passthrough); err != nil {
		return Result{ExitCode: exitInternal}, err
	}
	var selection []string
	if !req.Full {
		if selection, err = pytestSelection(req.Targets); err != nil {
			return Result{ExitCode: exitInternal}, err
		}
	}
	repo, err := filepath.Abs(req.RepoDir)
	if err != nil {
		return Result{ExitCode: exitInternal}, fmt.Errorf("resolve repository directory: %w", err)
	}
	req.RepoDir = repo

	res = Result{ExitCode: exitInternal}
	hazardDir := filepath.Join(repo, ".hazard")
	if err := os.MkdirAll(hazardDir, 0o755); err != nil {
		return res, fmt.Errorf("create %s: %w", hazardDir, err)
	}
	if err := writePytestPlugin(hazardDir); err != nil {
		return res, err
	}
	report := junitPath(req)
	if err := prepareJUnit(report); err != nil {
		return res, err
	}

	missingPath, removeMissing, err := tempFile(hazardDir, "missing-*.txt", nil)
	if err != nil {
		return res, err
	}
	defer func() { err = errors.Join(err, removeMissing()) }()

	selectionPath := ""
	if !req.Full {
		var removeSelection func() error
		data := []byte(strings.Join(selection, "\n") + "\n")
		if selectionPath, removeSelection, err = tempFile(hazardDir, "selection-*.txt", data); err != nil {
			return res, err
		}
		defer func() { err = errors.Join(err, removeSelection()) }()
	}

	pythonPath := hazardDir
	if existing := os.Getenv("PYTHONPATH"); existing != "" {
		pythonPath += string(os.PathListSeparator) + existing
	}
	python := req.Python
	if python == "" {
		python = "python"
	}
	argv := []string{python, "-m", "pytest", "-p", "hazard_pytest", "--junitxml=" + report}
	argv = append(argv, req.PytestArgs...)
	argv = append(argv, req.Testpaths...)
	argv = append(argv, req.Passthrough...)
	inv := Invocation{
		Argv: argv,
		Dir:  repo,
		// HAZARD_SELECTION is always set, empty for a full run, so a value
		// inherited from the caller's environment can never narrow it.
		Env:   []string{"HAZARD_SELECTION=" + selectionPath, "HAZARD_MISSING_OUT=" + missingPath, "PYTHONPATH=" + pythonPath},
		Whole: req.Full,
	}
	res.Invocations = []Invocation{inv}

	code, err := runProcess(ctx, inv, req.Stdout, req.Stderr)
	res.ExitCode = code
	if err != nil {
		return res, err
	}
	missing, err := readMissingIDs(missingPath)
	if err != nil {
		if res.ExitCode == 0 {
			res.ExitCode = exitInternal
		}
		return res, err
	}
	if len(missing) > 0 {
		res.Abstained, res.Reason, res.Missing = true, pytestMissingReason, missing
	}
	return res, nil
}

// pytestSelection validates the targets and returns their node IDs, sorted and
// deduplicated. A narrowed run with no target is an error: selecting nothing is
// never silently a pass.
func pytestSelection(targets []Target) ([]string, error) {
	if len(targets) == 0 {
		return nil, errors.New("narrowed pytest run has no targets; request a full run instead")
	}
	ids := make([]string, 0, len(targets))
	for _, t := range targets {
		if t.Language != "" && t.Language != "python" {
			return nil, fmt.Errorf("target %q has language %q, want python", t.ID, t.Language)
		}
		// One ID per line: a line break or invalid UTF-8 could not round-trip.
		if t.RunnerID == "" || strings.ContainsAny(t.RunnerID, "\n\r\x00") || !utf8.ValidString(t.RunnerID) {
			return nil, fmt.Errorf("target %q has an unusable pytest node ID %q", t.ID, t.RunnerID)
		}
		ids = append(ids, t.RunnerID)
	}
	slices.Sort(ids)
	return slices.Compact(ids), nil
}

// writePytestPlugin writes the embedded plugin unless it is already current.
func writePytestPlugin(hazardDir string) error {
	path := filepath.Join(hazardDir, "hazard_pytest.py")
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, pytestPlugin) {
		return nil
	}
	if err := os.WriteFile(path, pytestPlugin, 0o644); err != nil {
		return fmt.Errorf("write pytest plugin: %w", err)
	}
	return nil
}

// tempFile creates a file in dir holding data and returns its path and a
// function that removes it.
func tempFile(dir, pattern string, data []byte) (path string, remove func() error, err error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", nil, fmt.Errorf("create temp file: %w", err)
	}
	path = f.Name()
	remove = func() error {
		if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			return fmt.Errorf("remove temp file: %w", rmErr)
		}
		return nil
	}
	if _, werr := f.Write(data); werr != nil {
		return "", nil, errors.Join(fmt.Errorf("write temp file: %w", werr), f.Close(), remove())
	}
	if cerr := f.Close(); cerr != nil {
		return "", nil, errors.Join(fmt.Errorf("close temp file: %w", cerr), remove())
	}
	return path, remove, nil
}

// readMissingIDs returns the sorted, deduplicated IDs the plugin wrote.
func readMissingIDs(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read missing node IDs: %w", err)
	}
	var ids []string
	for _, line := range strings.Split(string(data), "\n") {
		if line != "" {
			ids = append(ids, line)
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids), nil
}
