package runner

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeGoEnv carries a JSON fakeGoSpec. When it is set, the test binary acts as
// a stand-in for the go command, so the RunGo bookkeeping (exit codes, report
// handling, cancellation) is tested without waiting for real compiles.
const fakeGoEnv = "HAZARD_RUNNER_FAKE_GO"

// fakePythonEnv carries a JSON fakePythonSpec for the stand-in python.
const fakePythonEnv = "HAZARD_RUNNER_FAKE_PY"

// fakeGoDir holds copies of the test binary named like the go and python commands.
var fakeGoDir string

// nativeRoot holds the repositories that the tests running the real go command
// share, so go's build cache is reused across them. Tests treat them as read-only.
var nativeRoot string

type fakeGoPackage struct {
	Exit   int
	Events []goEvent
	Stderr string
}

type fakeGoSpec struct {
	// Packages is keyed by the last argument of the command line.
	Packages map[string]fakeGoPackage
	// Log is a file that receives one fakeGoCall per run.
	Log     string
	SleepMS int
	// BlockPath, when set, becomes a non-empty directory during the run, so
	// publishing a file to that path fails after a passing run.
	BlockPath string
}

type fakeGoCall struct {
	Args []string
	Dir  string
}

func TestMain(m *testing.M) {
	if spec := os.Getenv(fakeGoEnv); spec != "" {
		os.Exit(runFakeGo(spec))
	}
	if spec := os.Getenv(fakePythonEnv); spec != "" {
		os.Exit(runFakePython(spec))
	}
	dir, err := os.MkdirTemp("", "hazard-fakego-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := installFakeGo(dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fakeGoDir = dir
	if nativeRoot, err = os.MkdirTemp("", "hazard-native-"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	for _, d := range []string{dir, nativeRoot} {
		if err := os.RemoveAll(d); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	}
	os.Exit(code)
}

func installFakeGo(dir string) error {
	for _, name := range []string{"go", "python"} {
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		if err := copyFile(os.Args[0], filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(from, to string) (err error) {
	src, err := os.Open(from)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := src.Close(); err == nil {
			err = cerr
		}
	}()
	dst, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := dst.Close(); err == nil {
			err = cerr
		}
	}()
	_, err = io.Copy(dst, src)
	return err
}

// fakePythonSpec makes the stand-in python behave like pytest after a
// collection that found missing node IDs (or not).
type fakePythonSpec struct {
	Exit    int
	Missing []string
	// DeleteMissingOut removes the missing-ids file, as a crashed plugin would.
	DeleteMissingOut bool
	// BlockSelection replaces the selection file with a non-empty directory, so
	// removing it during cleanup fails after a passing run.
	BlockSelection bool
}

// blockWithDirectory replaces whatever is at path with a non-empty directory:
// removing it with os.Remove then fails with "directory not empty".
func blockWithDirectory(path string) error {
	if path == "" {
		return errors.New("no path to block")
	}
	if err := os.RemoveAll(path); err != nil {
		return err
	}
	return os.MkdirAll(filepath.Join(path, "keep"), 0o755)
}

func runFakePython(raw string) int {
	var spec fakePythonSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		fmt.Fprintln(os.Stderr, "fake python: bad spec:", err)
		return 98
	}
	if spec.BlockSelection {
		if err := blockWithDirectory(os.Getenv("HAZARD_SELECTION")); err != nil {
			fmt.Fprintln(os.Stderr, "fake python:", err)
			return 98
		}
	}
	if out := os.Getenv("HAZARD_MISSING_OUT"); out != "" && spec.DeleteMissingOut {
		if err := os.Remove(out); err != nil {
			fmt.Fprintln(os.Stderr, "fake python:", err)
			return 98
		}
	} else if out != "" && len(spec.Missing) > 0 {
		if err := os.WriteFile(out, []byte(strings.Join(spec.Missing, "\n")+"\n"), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "fake python:", err)
			return 98
		}
	}
	return spec.Exit
}

func runFakeGo(raw string) int {
	var spec fakeGoSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		fmt.Fprintln(os.Stderr, "fake go: bad spec:", err)
		return 98
	}
	args := os.Args[1:]
	if spec.Log != "" {
		wd, err := os.Getwd()
		if err != nil {
			fmt.Fprintln(os.Stderr, "fake go:", err)
			return 98
		}
		line, err := json.Marshal(fakeGoCall{Args: args, Dir: wd})
		if err != nil {
			fmt.Fprintln(os.Stderr, "fake go:", err)
			return 98
		}
		f, err := os.OpenFile(spec.Log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintln(os.Stderr, "fake go:", err)
			return 98
		}
		if _, err := f.Write(append(line, '\n')); err != nil {
			fmt.Fprintln(os.Stderr, "fake go:", err)
			return 98
		}
		if err := f.Close(); err != nil {
			fmt.Fprintln(os.Stderr, "fake go:", err)
			return 98
		}
	}
	if spec.SleepMS > 0 {
		time.Sleep(time.Duration(spec.SleepMS) * time.Millisecond)
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "fake go: no arguments")
		return 98
	}
	pkg, ok := spec.Packages[args[len(args)-1]]
	if !ok {
		fmt.Fprintf(os.Stderr, "fake go: no scenario for %q\n", args[len(args)-1])
		return 98
	}
	if spec.BlockPath != "" {
		if err := blockWithDirectory(spec.BlockPath); err != nil {
			fmt.Fprintln(os.Stderr, "fake go:", err)
			return 98
		}
	}
	for _, ev := range pkg.Events {
		b, err := json.Marshal(ev)
		if err != nil {
			fmt.Fprintln(os.Stderr, "fake go:", err)
			return 98
		}
		if _, err := os.Stdout.Write(append(b, '\n')); err != nil {
			return 98
		}
	}
	if pkg.Stderr != "" {
		fmt.Fprint(os.Stderr, pkg.Stderr)
	}
	return pkg.Exit
}

// useFakeGo makes the stand-in the only go command on PATH for the test. It
// must not be used by parallel tests: it changes the process environment.
func useFakeGo(t *testing.T, spec fakeGoSpec) (logPath string) {
	t.Helper()
	if spec.Log == "" {
		spec.Log = filepath.Join(t.TempDir(), "calls.jsonl")
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeGoDir)
	t.Setenv(fakeGoEnv, string(raw))
	return spec.Log
}

func readFakeGoLog(t *testing.T, path string) []fakeGoCall {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var calls []fakeGoCall
	dec := json.NewDecoder(bytes.NewReader(data))
	for dec.More() {
		var c fakeGoCall
		if err := dec.Decode(&c); err != nil {
			t.Fatal(err)
		}
		calls = append(calls, c)
	}
	return calls
}
