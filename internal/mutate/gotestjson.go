package mutate

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
)

// testRun is what one `go test -json` invocation reported.
type testRun struct {
	Failed                    []string            // sorted failing top-level test IDs "go:<pkg>:<Test>"
	FailedByPackage           map[string][]string // import path -> failing top-level test names
	PackagesFailedWithoutTest []string            // packages that failed with no failing top-level test
	BuildFailed               []string            // packages whose test build failed
	TimedOut                  bool                // a test timeout panic was reported
	Durations                 map[string]float64  // top-level test ID -> seconds
}

// goTestEvent is one test2json event. ImportPath and FailedBuild appear on the
// build events go test -json emits when compilation fails.
type goTestEvent struct {
	Action      string
	Package     string
	Test        string
	Elapsed     float64
	Output      string
	ImportPath  string
	FailedBuild string
}

// testID returns the stable ID of a top-level Go test.
func testID(pkg, name string) string { return "go:" + pkg + ":" + name }

// buildPackage strips the " [pkg.test]" suffix go adds to a build target.
func buildPackage(importPath string) string {
	pkg, _, _ := strings.Cut(importPath, " [")
	return pkg
}

// parseGoTestJSON reads a `go test -json` stream. Lines that do not start with
// "{" are plain tool output and are ignored; a line that starts with "{" but
// is not a valid event is an error, so truncated output is never read as a
// green run. Only top-level tests (no "/" in the name) are reported.
func parseGoTestJSON(r io.Reader) (testRun, error) {
	run := testRun{FailedByPackage: map[string][]string{}, Durations: map[string]float64{}}
	pkgFailed := map[string]bool{}
	buildFailed := map[string]bool{}
	br := bufio.NewReader(r)
	for {
		line, readErr := br.ReadString('\n')
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "{") {
			var ev goTestEvent
			if err := json.Unmarshal([]byte(trimmed), &ev); err != nil {
				return testRun{}, fmt.Errorf("mutate: malformed go test -json event %q: %w", trimmed, err)
			}
			applyEvent(&run, ev, pkgFailed, buildFailed)
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return testRun{}, fmt.Errorf("mutate: read go test -json output: %w", readErr)
		}
	}
	for pkg := range buildFailed {
		run.BuildFailed = append(run.BuildFailed, pkg)
	}
	for pkg := range pkgFailed {
		if len(run.FailedByPackage[pkg]) == 0 && !buildFailed[pkg] {
			run.PackagesFailedWithoutTest = append(run.PackagesFailedWithoutTest, pkg)
		}
	}
	sort.Strings(run.BuildFailed)
	sort.Strings(run.PackagesFailedWithoutTest)
	for pkg, names := range run.FailedByPackage {
		sort.Strings(names)
		names = slices.Compact(names)
		run.FailedByPackage[pkg] = names
		for _, name := range names {
			run.Failed = append(run.Failed, testID(pkg, name))
		}
	}
	sort.Strings(run.Failed)
	return run, nil
}

func applyEvent(run *testRun, ev goTestEvent, pkgFailed, buildFailed map[string]bool) {
	if ev.Action == "build-fail" && ev.ImportPath != "" {
		buildFailed[buildPackage(ev.ImportPath)] = true
		return
	}
	if ev.Package == "" {
		return
	}
	if ev.FailedBuild != "" {
		buildFailed[ev.Package] = true
	}
	if ev.Action == "output" {
		if strings.Contains(ev.Output, "panic: test timed out after") {
			run.TimedOut = true
		}
		if strings.Contains(ev.Output, "[build failed]") {
			buildFailed[ev.Package] = true
		}
		return
	}
	if ev.Test == "" {
		if ev.Action == "fail" {
			pkgFailed[ev.Package] = true
		}
		return
	}
	if strings.Contains(ev.Test, "/") {
		return
	}
	switch ev.Action {
	case "pass":
		run.Durations[testID(ev.Package, ev.Test)] = ev.Elapsed
	case "fail":
		run.Durations[testID(ev.Package, ev.Test)] = ev.Elapsed
		run.FailedByPackage[ev.Package] = append(run.FailedByPackage[ev.Package], ev.Test)
	}
}
