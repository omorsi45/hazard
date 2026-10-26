package index

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/omorsi45/hazard/internal/config"
	"github.com/omorsi45/hazard/internal/graph"
)

const goCoverageGranularity = "go-top-level-test"

// profileBlock is one line range of a Go coverage profile.
type profileBlock struct {
	File               string // import-path form
	StartLine, EndLine int
	Count              int
}

var coverBlockLine = regexp.MustCompile(`^(.+):(\d+)\.\d+,(\d+)\.\d+ \d+ (\d+)$`)

// parseCoverProfile parses a Go coverage profile. Any line that is not a block
// is an error: a silently dropped block would understate a test's coverage.
func parseCoverProfile(data string) ([]profileBlock, error) {
	lines := strings.Split(strings.ReplaceAll(data, "\r\n", "\n"), "\n")
	if !strings.HasPrefix(lines[0], "mode: ") {
		return nil, fmt.Errorf("coverage profile has no mode line")
	}
	var blocks []profileBlock
	for _, line := range lines[1:] {
		if line == "" {
			continue
		}
		m := coverBlockLine.FindStringSubmatch(line)
		if m == nil {
			return nil, fmt.Errorf("malformed coverage profile line %q", line)
		}
		start, err1 := strconv.Atoi(m[2])
		end, err2 := strconv.Atoi(m[3])
		count, err3 := strconv.Atoi(m[4])
		if err1 != nil || err2 != nil || err3 != nil {
			return nil, fmt.Errorf("malformed coverage profile line %q", line)
		}
		blocks = append(blocks, profileBlock{File: m[1], StartLine: start, EndLine: end, Count: count})
	}
	return blocks, nil
}

// modulePathOf derives the main module path from the graph's Go packages. It
// reports false when there are no Go packages or their paths disagree.
func modulePathOf(g *graph.Graph) (string, bool) {
	module := ""
	for id, p := range g.Packages {
		if p.Language != "go" {
			continue
		}
		var derived string
		switch {
		case p.Dir == ".":
			derived = id
		case strings.HasSuffix(id, "/"+p.Dir):
			derived = strings.TrimSuffix(id, "/"+p.Dir)
		default:
			return "", false
		}
		if module != "" && module != derived {
			return "", false
		}
		module = derived
	}
	return module, module != ""
}

// goCoverageResult is what Go coverage collection produced.
type goCoverageResult struct {
	Coverage    *Coverage // nil when coverage is unavailable
	Diagnostics []string
	Commands    []Command
}

type goTestRun struct {
	command  Command
	coverage TestCoverage
	diag     string
	err      error
}

// collectGoCoverage runs a baseline `go test`, then one coverage run per
// top-level Go test. A failed baseline makes coverage unavailable; a failed or
// skipped or unobserved test is never recorded as covered.
func collectGoCoverage(ctx context.Context, root string, cfg config.Config, m *Manifest) (goCoverageResult, error) {
	var res goCoverageResult
	var tests []Test
	for _, t := range m.Tests {
		if t.Language == "go" {
			tests = append(tests, t)
		}
	}
	if len(tests) == 0 {
		return res, nil
	}
	unavailable := func(reason string) (goCoverageResult, error) {
		res.Diagnostics = append(res.Diagnostics, "go coverage unavailable: "+reason)
		return res, nil
	}
	modulePath, ok := modulePathOf(m.Graph)
	if !ok {
		return unavailable("cannot determine the module path from the package graph")
	}
	var tagArgs []string
	if len(cfg.Languages.Go.Tags) > 0 {
		tagArgs = []string{"-tags", strings.Join(cfg.Languages.Go.Tags, ",")}
	}
	baselineArgv := append(append([]string{"go", "test", "-count=1"}, tagArgs...), cfg.Languages.Go.Packages...)
	base, err := runCommand(ctx, root, nil, baselineArgv...)
	res.Commands = append(res.Commands, base.Command)
	if err != nil {
		return res, err
	}
	if base.Command.Exit != 0 {
		return unavailable(fmt.Sprintf("the baseline go test failed with exit status %d: %s", base.Command.Exit, tail(string(base.Stdout)+string(base.Stderr))))
	}
	var pkgIDs []string
	for id, p := range m.Graph.Packages {
		if p.Language == "go" {
			pkgIDs = append(pkgIDs, id)
		}
	}
	sort.Strings(pkgIDs)
	tmp, err := os.MkdirTemp("", "hazard-cover-*")
	if err != nil {
		return res, err
	}
	defer func() { _ = os.RemoveAll(tmp) }() // best-effort cleanup of scratch profiles

	runs := make([]goTestRun, len(tests))
	jobs := make(chan int)
	workers := min(max(runtime.NumCPU()/2, 1), 4, len(tests))
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				runs[i] = runGoTestCoverage(ctx, root, tmp, i, tests[i], tagArgs, pkgIDs, modulePath)
			}
		}()
	}
	for i := range tests {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	cov := &Coverage{
		HeadSHA:       m.HeadSHA,
		ContentDigest: m.ContentDigest,
		Granularity:   goCoverageGranularity,
		Tests:         map[string]TestCoverage{},
		FileDigests:   map[string]string{},
	}
	for i, run := range runs {
		if run.err != nil {
			return res, run.err
		}
		res.Commands = append(res.Commands, run.command)
		cov.Tests[tests[i].ID] = run.coverage
		if run.diag != "" {
			res.Diagnostics = append(res.Diagnostics, run.diag)
		}
	}
	for _, id := range pkgIDs {
		for _, rel := range m.Graph.Packages[id].Files {
			digest, ok := m.Files[rel]
			if !ok {
				data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
				if err != nil {
					return res, fmt.Errorf("hash instrumented file %s: %w", rel, err)
				}
				digest = sha(data)
			}
			cov.FileDigests[rel] = digest
		}
	}
	res.Coverage = cov
	return res, nil
}

func runGoTestCoverage(ctx context.Context, root, tmp string, index int, test Test, tagArgs, pkgIDs []string, modulePath string) goTestRun {
	profile := filepath.Join(tmp, fmt.Sprintf("%d.out", index))
	argv := append([]string{"go", "test", "-count=1", "-v"}, tagArgs...)
	argv = append(argv, "-run", "^"+regexp.QuoteMeta(test.Name)+"$", "-coverpkg="+strings.Join(pkgIDs, ","), "-coverprofile="+profile, test.Package)
	res, err := runCommand(ctx, root, nil, argv...)
	run := goTestRun{command: res.Command, err: err, coverage: TestCoverage{Status: "failed"}}
	if err != nil {
		return run
	}
	if res.Command.Exit != 0 {
		return run
	}
	output := string(res.Stdout)
	outcome := regexp.MustCompile(`(?m)^--- (PASS|SKIP|FAIL): ` + regexp.QuoteMeta(test.Name) + ` \(`).FindStringSubmatch(output)
	switch {
	case outcome == nil:
		run.coverage.Status = "not_run"
		run.diag = fmt.Sprintf("go coverage: %s produced no result line; its coverage is unknown", test.ID)
		return run
	case outcome[1] == "SKIP":
		run.coverage.Status = "not_run"
		return run
	case outcome[1] != "PASS":
		return run
	}
	data, err := os.ReadFile(profile)
	if err == nil {
		var blocks []profileBlock
		if blocks, err = parseCoverProfile(string(data)); err == nil {
			covered, mapErr := repoBlocks(blocks, modulePath)
			if mapErr == nil {
				run.coverage = TestCoverage{Status: "covered", Blocks: covered}
				return run
			}
			err = mapErr
		}
	}
	run.coverage.Status = "not_run"
	run.diag = fmt.Sprintf("go coverage: %s profile unusable, coverage unknown: %v", test.ID, err)
	return run
}

// repoBlocks converts executed profile blocks to repo-relative blocks.
func repoBlocks(blocks []profileBlock, modulePath string) ([]Block, error) {
	var out []Block
	for _, b := range blocks {
		if b.Count <= 0 {
			continue
		}
		rel, ok := strings.CutPrefix(b.File, modulePath+"/")
		if !ok {
			return nil, fmt.Errorf("profile file %q is outside module %s", b.File, modulePath)
		}
		out = append(out, Block{File: rel, StartLine: b.StartLine, EndLine: b.EndLine})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.File != b.File {
			return a.File < b.File
		}
		if a.StartLine != b.StartLine {
			return a.StartLine < b.StartLine
		}
		return a.EndLine < b.EndLine
	})
	return out, nil
}
