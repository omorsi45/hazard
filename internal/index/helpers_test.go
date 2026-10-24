package index

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// writeFiles creates files (slash-separated relative path -> content) under dir.
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// testPython returns the interpreter for tests that need Python modules. It
// uses HAZARD_TEST_PYTHON, else python on PATH. When a module cannot be
// imported the test is skipped with an explicit message, unless
// HAZARD_REQUIRE_PYTEST is "1", in which case it fails.
func testPython(t *testing.T, modules ...string) string {
	t.Helper()
	python := os.Getenv("HAZARD_TEST_PYTHON")
	if python == "" {
		python = "python"
	}
	out, err := exec.Command(python, "-c", "import "+strings.Join(modules, ", ")).CombinedOutput()
	if err != nil {
		msg := "Python modules " + strings.Join(modules, ", ") + " are unavailable through " + python + ": " + strings.TrimSpace(string(out)) + " " + err.Error()
		if os.Getenv("HAZARD_REQUIRE_PYTEST") == "1" {
			t.Fatal(msg)
		}
		t.Skip(msg + " (set HAZARD_TEST_PYTHON to an interpreter with pytest and coverage, HAZARD_REQUIRE_PYTEST=1 to forbid skipping)")
	}
	return python
}
