package runner

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// maxGoPatternLen is the longest anchored -run pattern passed on a command
// line. Windows limits a command line to about 32k characters, so a package
// whose pattern is longer runs whole instead.
const maxGoPatternLen = 8000

// goMissingReason is Result.Reason when a narrowed run was asked for tests that
// never started.
const goMissingReason = "go_missing_tests"

// GoInvocations plans the go test processes for req. It returns the
// invocations and the packages that are run whole because their anchored
// pattern would be too long.
//
// A full run is one invocation over GoPackages. A narrowed run is one
// invocation per package, sorted by import path, selecting the top-level tests
// with an anchored alternation. Tags and passthrough arguments precede the
// package argument.
func GoInvocations(req Request) ([]Invocation, []string, error) {
	if err := ValidatePassthrough("go", req.Passthrough); err != nil {
		return nil, nil, err
	}
	prefix := []string{"go", "test", "-json"}
	if len(req.GoTags) > 0 {
		for _, tag := range req.GoTags {
			if tag == "" || strings.ContainsFunc(tag, func(r rune) bool { return r == ',' || unicode.IsSpace(r) }) {
				return nil, nil, fmt.Errorf("invalid Go build tag %q", tag)
			}
		}
		prefix = append(prefix, "-tags="+strings.Join(req.GoTags, ","))
	}
	prefix = append(prefix, req.Passthrough...)

	if req.Full {
		patterns := req.GoPackages
		if len(patterns) == 0 {
			patterns = []string{"./..."}
		}
		for _, p := range patterns {
			if p == "" || strings.HasPrefix(p, "-") {
				return nil, nil, fmt.Errorf("invalid Go package pattern %q", p)
			}
		}
		return []Invocation{{Argv: slices.Concat(prefix, patterns), Dir: req.RepoDir, Whole: true}}, nil, nil
	}

	byPackage := map[string]map[string]struct{}{}
	for _, t := range req.Targets {
		parent, err := goTargetParent(t)
		if err != nil {
			return nil, nil, err
		}
		if byPackage[t.Package] == nil {
			byPackage[t.Package] = map[string]struct{}{}
		}
		byPackage[t.Package][parent] = struct{}{}
	}
	if len(byPackage) == 0 {
		return nil, nil, fmt.Errorf("narrowed Go run has no targets; request a full run instead")
	}

	packages := make([]string, 0, len(byPackage))
	for p := range byPackage {
		packages = append(packages, p)
	}
	slices.Sort(packages)

	var invocations []Invocation
	var widened []string
	for _, pkg := range packages {
		names := make([]string, 0, len(byPackage[pkg]))
		for n := range byPackage[pkg] {
			names = append(names, regexp.QuoteMeta(n))
		}
		slices.Sort(names)
		pattern := "^(" + strings.Join(names, "|") + ")$"
		inv := Invocation{Dir: req.RepoDir, Package: pkg}
		if len(pattern) > maxGoPatternLen {
			inv.Argv = slices.Concat(prefix, []string{pkg})
			inv.Whole = true
			widened = append(widened, pkg)
		} else {
			inv.Argv = slices.Concat(prefix, []string{"-run", pattern, pkg})
		}
		invocations = append(invocations, inv)
	}
	return invocations, widened, nil
}

// goTargetParent validates a Go target and returns its top-level test name: a
// subtest "Parent/sub" widens to Parent.
func goTargetParent(t Target) (string, error) {
	if t.Language != "" && t.Language != "go" {
		return "", fmt.Errorf("target %q has language %q, want go", t.ID, t.Language)
	}
	if t.Package == "" || strings.HasPrefix(t.Package, "-") || strings.ContainsFunc(t.Package, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return "", fmt.Errorf("target %q has invalid Go package %q", t.ID, t.Package)
	}
	parent, _, _ := strings.Cut(t.Name, "/")
	if parent == "" {
		return "", fmt.Errorf("target %q has no top-level test name (%q)", t.ID, t.Name)
	}
	return parent, nil
}

// missingGoTargets returns the sorted IDs of the targets whose top-level test
// never started in the stream b read. go test exits 0 with "no tests to run"
// when -run matches nothing, so a selected test that was renamed or removed
// since the index was built would otherwise count as passed. A target that
// cannot be resolved is missing too: an unknown is never a pass.
func missingGoTargets(b *goJUnitBuilder, targets []Target) []string {
	var missing []string
	for _, t := range targets {
		parent, err := goTargetParent(t)
		if err != nil || !b.started(t.Package, parent) {
			missing = append(missing, t.ID)
		}
	}
	slices.Sort(missing)
	return slices.Compact(missing)
}

// RunGo runs the planned go test invocations and writes one JUnit report to
// the request's JUnitPath.
//
// Every invocation runs, even after one fails. ExitCode is the first nonzero
// process exit code and is 0 only when every invocation passed; it is never 0
// when an error is returned. The report is written only when every process ran
// to completion, and a report from an earlier run is removed first.
//
// A narrowed run whose selected tests did not all start (see missingGoTargets)
// reports Abstained with the reason go_missing_tests and the IDs in Missing,
// like a pytest run with unknown node IDs, so the caller reruns the full
// suite. The report of the narrowed attempt is still written.
func RunGo(ctx context.Context, req Request) (Result, error) {
	invocations, widened, err := GoInvocations(req)
	if err != nil {
		return Result{ExitCode: exitInternal}, err
	}
	res := Result{Invocations: invocations, Widened: widened}
	path := junitPath(req)
	if err := prepareJUnit(path); err != nil {
		res.ExitCode = exitInternal
		return res, err
	}

	builder := newGoJUnitBuilder(req.Stdout)
	for _, inv := range invocations {
		code, err := runProcess(ctx, inv, builder, req.Stderr)
		builder.Flush()
		if err != nil {
			res.ExitCode = exitInternal
			return res, err
		}
		if res.ExitCode == 0 {
			res.ExitCode = code
		}
	}

	if !req.Full {
		if missing := missingGoTargets(builder, req.Targets); len(missing) > 0 {
			res.Abstained, res.Reason, res.Missing = true, goMissingReason, missing
		}
	}

	data, err := builder.Marshal()
	if err == nil {
		err = writeFileAtomic(path, data)
	}
	if err == nil && builder.Err() != nil {
		err = fmt.Errorf("write test output: %w", builder.Err())
	}
	if err != nil && res.ExitCode == 0 {
		res.ExitCode = exitInternal
	}
	return res, err
}
