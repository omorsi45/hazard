package runner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// fakeGoEnv carries a JSON fakeGoSpec. When it is set, the test binary acts as
// a stand-in for the go command, so the RunGo bookkeeping (exit codes, report
// handling, cancellation) is tested without waiting for real compiles.
const fakeGoEnv = "HAZARD_RUNNER_FAKE_GO"

// fakeGoDir holds a copy of the test binary named like the go command.
var fakeGoDir string

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
}

type fakeGoCall struct {
	Args []string
	Dir  string
}

func TestMain(m *testing.M) {
	if spec := os.Getenv(fakeGoEnv); spec != "" {
		os.Exit(runFakeGo(spec))
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
	code := m.Run()
	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(code)
}

func installFakeGo(dir string) (err error) {
	src, err := os.Open(os.Args[0])
	if err != nil {
		return err
	}
	defer func() {
		if cerr := src.Close(); err == nil {
			err = cerr
		}
	}()
	name := "go"
	if runtime.GOOS == "windows" {
		name = "go.exe"
	}
	dst, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
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
