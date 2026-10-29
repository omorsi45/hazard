package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Target is one test selected for a narrowed run.
//
// For Go, Package is the import path and Name is the top-level test name; a
// subtest ID such as "Parent/sub" widens to Parent. For Python, RunnerID is the
// pytest node ID.
type Target struct {
	ID, Language, Package, Name, RunnerID string
}

// Request describes one run of the project's native test runner.
type Request struct {
	RepoDir string
	Full    bool
	Targets []Target

	// GoPackages are the full-suite package patterns (default "./...") and
	// GoTags the build tags passed as -tags.
	GoPackages, GoTags []string

	Python     string
	Testpaths  []string
	PytestArgs []string

	// Passthrough holds the arguments given after "--".
	Passthrough []string

	// JUnitPath is absolute or relative to RepoDir.
	JUnitPath string

	Stdout, Stderr io.Writer
}

// Invocation is one native runner process.
type Invocation struct {
	Argv    []string
	Dir     string
	Env     []string
	Package string
	Whole   bool
}

// Result reports what a run did.
type Result struct {
	ExitCode    int
	Invocations []Invocation
	Widened     []string
	Missing     []string
	Abstained   bool
	Reason      string
}

// ErrPassthroughNarrows reports a passthrough argument that would change which
// tests run.
var ErrPassthroughNarrows = errors.New("passthrough would narrow the selected tests")

// exitInternal is the exit code reported when the runner itself failed (seed
// 6.10): it is never 0, so a caller that ignores the error cannot report green.
const exitInternal = 70

// processWaitDelay bounds how long Wait lingers on output pipes after the
// process was killed.
const processWaitDelay = 10 * time.Second

// junitPath returns the absolute JUnit path for req, defaulting to
// <RepoDir>/.hazard/junit.xml.
func junitPath(req Request) string {
	p := req.JUnitPath
	if p == "" {
		p = filepath.Join(".hazard", "junit.xml")
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(req.RepoDir, p)
	}
	return p
}

// prepareJUnit creates the report directory and removes a report left by an
// earlier run, so a failed run can never be read as the previous run's result.
func prepareJUnit(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create JUnit directory: %w", err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove stale JUnit report: %w", err)
	}
	return nil
}

// writeFileAtomic writes data to path through a temp file in the same
// directory, so a reader never sees a partial report.
func writeFileAtomic(path string, data []byte) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), ".junit-*.tmp")
	if err != nil {
		return fmt.Errorf("write JUnit report: %w", err)
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			if rmErr := os.Remove(tmp); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
				err = errors.Join(err, rmErr)
			}
		}
	}()
	if _, err = f.Write(data); err != nil {
		return errors.Join(fmt.Errorf("write JUnit report: %w", err), f.Close())
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("write JUnit report: %w", err)
	}
	if err = os.Rename(tmp, path); err != nil {
		return fmt.Errorf("publish JUnit report: %w", err)
	}
	return nil
}

// runProcess runs one invocation. A process that ran and exited nonzero is not
// an error: its exit code is returned, never 0. An error means the process
// could not be run to completion (it did not start, or the context ended), and
// comes with exitInternal.
func runProcess(ctx context.Context, inv Invocation, stdout, stderr io.Writer) (int, error) {
	cmd := exec.CommandContext(ctx, inv.Argv[0], inv.Argv[1:]...)
	cmd.Dir = inv.Dir
	if len(inv.Env) > 0 {
		cmd.Env = append(os.Environ(), inv.Env...)
	}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = processWaitDelay
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return exitInternal, fmt.Errorf("%s: %w", inv.Argv[0], ctxErr)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if code := exitErr.ExitCode(); code > 0 {
			return code, nil
		}
		return 1, nil
	}
	return exitInternal, fmt.Errorf("run %s: %w", inv.Argv[0], err)
}

// ValidatePassthrough rejects arguments that would narrow the selected tests.
// lang is "go" or "python". The returned error wraps ErrPassthroughNarrows and
// names the offending argument.
func ValidatePassthrough(lang string, args []string) error {
	switch lang {
	case "go":
		return validateGoPassthrough(args)
	case "python":
		return validatePytestPassthrough(args)
	}
	return fmt.Errorf("passthrough validation: unsupported language %q", lang)
}

func narrows(arg string) error {
	return fmt.Errorf("%w: %q", ErrPassthroughNarrows, arg)
}

// takesSeparateValue reports whether the argument after args[i] is the value of
// a flag written without "=". A following argument that starts with "-" is left
// for validation as a flag of its own.
func takesSeparateValue(args []string, i int) bool {
	return i+1 < len(args) && !strings.HasPrefix(args[i+1], "-")
}

// goValueFlags are go test and go build flags that take a separate value when
// written without "=". Flags missing here are read as booleans, so a following
// non-flag argument is a positional and is rejected.
var goValueFlags = map[string]bool{
	"asmflags": true, "benchtime": true, "blockprofile": true, "blockprofilerate": true,
	"buildmode": true, "C": true, "compiler": true, "count": true, "covermode": true,
	"coverpkg": true, "coverprofile": true, "cpu": true, "cpuprofile": true, "exec": true,
	"fuzz": true, "fuzzminimizetime": true, "fuzztime": true, "gccgoflags": true,
	"gcflags": true, "installsuffix": true, "ldflags": true, "memprofile": true,
	"memprofilerate": true, "mod": true, "modfile": true, "mutexprofile": true,
	"mutexprofilefraction": true, "outputdir": true, "overlay": true,
	"p": true, "parallel": true, "pgo": true, "pkgdir": true, "shuffle": true,
	"tags": true, "testlogfile": true, "timeout": true, "toolexec": true, "trace": true,
	"vet": true,
}

// goNarrowingFlags select or list tests instead of running them all. They are
// also refused after -args, where they reach the test binary as -test.run and
// friends.
var goNarrowingFlags = map[string]bool{"run": true, "skip": true, "list": true, "bench": true}

// goNoRunFlags are go test flags that compile, print or save a binary without
// running any test body.
var goNoRunFlags = map[string]bool{"c": true, "n": true, "o": true}

// goFlagName returns the normalized flag name of a "-name", "--name" or
// "-name=value" argument. go test also accepts the "test." prefix.
func goFlagName(arg string) (name string, hasValue, isFlag bool) {
	if len(arg) < 2 || arg[0] != '-' || arg == "--" {
		return "", false, false
	}
	rest := strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "-")
	name, _, hasValue = strings.Cut(rest, "=")
	return strings.TrimPrefix(name, "test."), hasValue, true
}

func validateGoPassthrough(args []string) error {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			if i+1 < len(args) {
				return narrows(args[i+1])
			}
			return nil
		}
		name, hasValue, isFlag := goFlagName(arg)
		if !isFlag {
			return narrows(arg)
		}
		if goNarrowingFlags[name] || goNoRunFlags[name] {
			return narrows(arg)
		}
		if name == "args" {
			// Everything after -args goes to the test binary, which accepts
			// -test.run and friends.
			for _, rest := range args[i+1:] {
				if n, _, ok := goFlagName(rest); ok && goNarrowingFlags[n] {
					return narrows(rest)
				}
			}
			return nil
		}
		if !hasValue && goValueFlags[name] && takesSeparateValue(args, i) {
			i++
		}
	}
	return nil
}

// pytestNarrowingLong are long options that change which tests pytest runs.
var pytestNarrowingLong = map[string]bool{
	"deselect": true, "lf": true, "last-failed": true, "ff": true, "failed-first": true,
}

// pytestNoRunLong are long options that collect, list or print without running
// test bodies.
var pytestNoRunLong = map[string]bool{
	"collect-only": true, "co": true, "setup-only": true, "setup-plan": true,
	"fixtures": true, "funcargs": true, "fixtures-per-test": true, "markers": true,
	"help": true, "version": true,
}

// pytestValueLong are long options that take a separate value when written
// without "=". Options missing here are read as flags, so a following non-flag
// argument is a positional and is rejected.
var pytestValueLong = map[string]bool{
	"maxfail": true, "tb": true, "durations": true, "durations-min": true,
	"rootdir": true, "basetemp": true, "config-file": true, "log-level": true,
	"capture": true, "color": true, "import-mode": true, "confcutdir": true,
	"numprocesses": true, "dist": true, "timeout": true, "override-ini": true,
	"pythonwarnings": true, "show-capture": true, "assert": true, "junit-prefix": true,
	"log-file": true, "log-file-level": true, "log-format": true, "log-date-format": true,
}

// pytestValueShort are short options whose value follows the letter, attached
// or as the next argument. -k and -m are narrowing and handled separately, as
// are -h and -V (help and version), which run no test.
const pytestValueShort = "pWocnr"

func validatePytestPassthrough(args []string) error {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			if i+1 < len(args) {
				return narrows(args[i+1])
			}
			return nil
		case strings.HasPrefix(arg, "--"):
			name, _, hasValue := strings.Cut(arg[2:], "=")
			if pytestNarrowingLong[name] || pytestNoRunLong[name] {
				return narrows(arg)
			}
			if !hasValue && pytestValueLong[name] && takesSeparateValue(args, i) {
				i++
			}
		case len(arg) > 1 && arg[0] == '-':
			// A cluster of short options such as -xk or -vv. argparse also
			// accepts the value attached: -kexpr, -pname.
			cluster := arg[1:]
			for j := 0; j < len(cluster); j++ {
				c := cluster[j]
				if c == 'k' || c == 'm' || c == 'h' || c == 'V' {
					return narrows(arg)
				}
				if strings.IndexByte(pytestValueShort, c) >= 0 {
					if j == len(cluster)-1 && takesSeparateValue(args, i) {
						i++
					}
					break
				}
			}
		default:
			return narrows(arg)
		}
	}
	return nil
}
