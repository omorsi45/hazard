package mutate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/omorsi45/hazard/internal/change"
	"github.com/omorsi45/hazard/internal/graph"
)

// Options configures Run.
type Options struct {
	RepoDir     string        // git repository; only Out and DBPath are ever written in it
	Base        string        // revision whose tracked files are mutated
	Files       []string      // globs (change.MatchGlob) selecting candidate files
	N           int           // execute the first N candidates by ID; <= 0 means all
	Operators   []string      // from ParseOperators; empty is an error
	Timeout     time.Duration // per-mutant `go test -timeout`
	Concurrency int           // number of slots; values < 1 mean 1
	Packages    []string      // go package patterns, e.g. ./...
	Tags        []string      // go build tags
	MaxHops     int           // reverse-import distance beyond which a killer is "beyond max hops"
	Out         string        // JSONL path; empty means no file
	DBPath      string        // SQLite path; empty means no database
	Log         io.Writer     // progress lines; nil discards
}

// Record is the outcome of one executed mutant.
type Record struct {
	ID       string `json:"id"`
	File     string `json:"file"`
	Line     int    `json:"line"`
	Operator string `json:"operator"`
	Original string `json:"original"`
	Mutated  string `json:"mutated"`
	BaseSHA  string `json:"base_sha"`
	// Status is killed, survived, build_error, timeout or unconfirmed.
	Status string `json:"status"`
	// Killers are the tests that failed in both runs (empty unless killed).
	Killers []string `json:"killers"`
	// UnreachableKillers is the subset of Killers whose package cannot import
	// the mutated file's package, directly or transitively.
	UnreachableKillers []string `json:"unreachable_killers"`
	// BeyondMaxHopsKillers is the subset of Killers that is reachable only
	// through more than Options.MaxHops reverse import edges.
	BeyondMaxHopsKillers []string `json:"beyond_max_hops_killers"`
	DurationMS           int64    `json:"duration_ms"`
}

// Summary describes one Run.
type Summary struct {
	BaseSHA    string         `json:"base_sha"`
	Candidates int            `json:"candidates"` // mutants generated for the selected files
	Executed   int            `json:"executed"`   // mutants actually run
	ByStatus   map[string]int `json:"by_status"`
	ByOperator map[string]int `json:"by_operator"` // executed mutants per operator
	Usable     int            `json:"usable"`      // killed mutants
	// UnreachableMutants counts killed mutants with at least one killer outside
	// the unbounded reverse reachability of the mutated package.
	UnreachableMutants int `json:"unreachable_mutants"`
	// FullyUnreachableMutants counts killed mutants whose every killer is unreachable.
	FullyUnreachableMutants int `json:"fully_unreachable_mutants"`
	// BeyondMaxHopsMutants counts killed mutants with a killer reachable only
	// beyond Options.MaxHops.
	BeyondMaxHopsMutants int      `json:"beyond_max_hops_mutants"`
	BaselineSeconds      float64  `json:"baseline_seconds"`
	Records              []Record `json:"records,omitempty"`
	// BaselineMS is the median of the two baseline runs' elapsed time of each
	// top-level test, in whole milliseconds rounded half up, keyed by the same
	// "go:<import path>:<Test>" IDs as Record.Killers.
	BaselineMS map[string]int64 `json:"baseline_ms"`
}

// goRunner runs the go command with args in dir. A nonzero exit is reported in
// the result with a nil error.
type goRunner func(ctx context.Context, dir string, args ...string) (cmdResult, error)

// tools are the external programs a run depends on, so that the procedure
// around them can be exercised without launching the go toolchain.
type tools struct {
	goCmd     goRunner
	loadGraph func(ctx context.Context, repoDir string, patterns, tags []string) (*graph.Graph, error)
}

func realTools() tools {
	return tools{
		goCmd: func(ctx context.Context, dir string, args ...string) (cmdResult, error) {
			return runCmd(ctx, dir, "go", args...)
		},
		loadGraph: graph.LoadGo,
	}
}

// Run executes the mutation driver.
func Run(ctx context.Context, opts Options) (Summary, error) {
	return run(ctx, opts, realTools())
}

func run(ctx context.Context, opts Options, tl tools) (sum Summary, err error) {
	opts, err = normalizeOptions(opts)
	if err != nil {
		return Summary{}, err
	}
	logf := newLogger(opts.Log)
	sha, err := resolveBase(ctx, opts.RepoDir, opts.Base)
	if err != nil {
		return Summary{}, err
	}
	tracked, err := listTracked(ctx, opts.RepoDir, sha)
	if err != nil {
		return Summary{}, err
	}
	root, err := os.MkdirTemp("", "hazard-mutate-*")
	if err != nil {
		return Summary{}, fmt.Errorf("mutate: create the slot root: %w", err)
	}
	defer func() {
		if rmErr := removeAllRetry(root); rmErr != nil {
			err = errors.Join(err, rmErr)
		}
	}()
	logf("slot root: %s", root)
	slot0 := filepath.Join(root, "slot0")
	if err := extractArchive(ctx, opts.RepoDir, sha, slot0); err != nil {
		return Summary{}, err
	}
	read := func(rel string) ([]byte, error) { return os.ReadFile(filepath.Join(slot0, filepath.FromSlash(rel))) }
	candidates, err := candidateMutants(tracked, opts.Files, opts.Operators, read)
	if err != nil {
		return Summary{}, err
	}
	if len(candidates) == 0 {
		return Summary{}, fmt.Errorf("mutate: no mutants for files %v and operators %v at %s", opts.Files, opts.Operators, sha)
	}
	selected := limitCandidates(candidates, opts.N)
	d := &driver{opts: opts, sha: sha, logf: logf, tools: tl, base: map[string][]byte{}}
	for _, m := range selected {
		if _, ok := d.base[m.File]; ok {
			continue
		}
		src, err := read(m.File)
		if err != nil {
			return Summary{}, fmt.Errorf("mutate: read %s: %w", m.File, err)
		}
		d.base[m.File] = src
	}
	d.slots = []string{slot0}
	baselineSeconds, baselineMS, err := d.baseline(ctx, slot0)
	if err != nil {
		return Summary{}, err
	}
	for i := 1; i < min(opts.Concurrency, len(selected)); i++ {
		dir := filepath.Join(root, fmt.Sprintf("slot%d", i))
		if err := extractArchive(ctx, opts.RepoDir, sha, dir); err != nil {
			return Summary{}, err
		}
		d.slots = append(d.slots, dir)
	}
	if d.graph, err = tl.loadGraph(ctx, slot0, opts.Packages, opts.Tags); err != nil {
		return Summary{}, fmt.Errorf("mutate: load the package graph: %w", err)
	}
	var partial *jsonlWriter
	if opts.Out != "" {
		if partial, err = newJSONLWriter(opts.Out); err != nil {
			return Summary{}, err
		}
		defer func() {
			if closeErr := partial.Close(); closeErr != nil {
				err = errors.Join(err, closeErr)
			}
		}()
	}
	logf("executing %d of %d mutants on %d slots", len(selected), len(candidates), len(d.slots))
	recs, err := d.execute(ctx, selected, func(r Record) error {
		if partial == nil {
			return nil
		}
		return partial.Append(r)
	})
	if err != nil {
		return Summary{}, err
	}
	sum = summarize(sha, len(candidates), baselineSeconds, recs)
	sum.BaselineMS = baselineMS
	if partial != nil {
		if err := partial.Commit(sum.Records); err != nil {
			return Summary{}, err
		}
		if err := writeBaseline(opts.Out, sha, baselineMS); err != nil {
			return Summary{}, err
		}
	}
	if opts.DBPath != "" {
		if err := persistMutants(ctx, opts.DBPath, sum.Records, time.Now()); err != nil {
			return Summary{}, err
		}
	}
	return sum, nil
}

// goToolIgnores reports whether the go tool ignores the directory holding rel:
// testdata, vendor, and directories starting with "." or "_".
func goToolIgnores(rel string) bool {
	segs := strings.Split(rel, "/")
	for _, seg := range segs[:len(segs)-1] {
		if seg == "testdata" || seg == "vendor" || strings.HasPrefix(seg, ".") || strings.HasPrefix(seg, "_") {
			return true
		}
	}
	return false
}

// configMutantsSafe is ConfigMutants with its internal-error panic converted
// to an error, so one file cannot take the run down.
func configMutantsSafe(file string, src []byte) (ms []Mutant, err error) {
	defer func() {
		if r := recover(); r != nil {
			ms, err = nil, fmt.Errorf("mutate: config mutants of %s: %v", file, r)
		}
	}()
	return ConfigMutants(file, src), nil
}

// candidateMutants generates the mutants of the tracked files that match globs,
// ordered by ID. Go files (outside testdata, vendor and hidden directories) use
// GoMutants; with the cfg operator selected, configuration files use
// ConfigMutants. An empty operator list, an unreadable file and an unparsable
// Go file are errors.
func candidateMutants(tracked, globs, operators []string, read func(rel string) ([]byte, error)) ([]Mutant, error) {
	if len(operators) == 0 {
		return nil, errors.New("mutate: no operators selected (valid: " + strings.Join(allOperators, ", ") + ")")
	}
	cfgSelected := slices.Contains(operators, "cfg")
	var out []Mutant
	for _, rel := range tracked {
		if !slices.ContainsFunc(globs, func(g string) bool { return change.MatchGlob(g, rel) }) {
			continue
		}
		isGo := strings.HasSuffix(rel, ".go") && !goToolIgnores(rel)
		isCfg := cfgSelected && cfgFormatOf(rel) != cfgNone
		if !isGo && !isCfg {
			continue
		}
		src, err := read(rel)
		if err != nil {
			return nil, fmt.Errorf("mutate: read %s: %w", rel, err)
		}
		var ms []Mutant
		if isGo {
			ms, err = GoMutants(rel, src, operators)
		} else {
			ms, err = configMutantsSafe(rel, src)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, ms...)
	}
	slices.SortFunc(out, func(a, b Mutant) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

// limitCandidates keeps the first n mutants; n <= 0 keeps all.
func limitCandidates(ms []Mutant, n int) []Mutant {
	if n <= 0 || n >= len(ms) {
		return ms
	}
	return ms[:n]
}

// killerPackage returns the import path of a "go:<pkg>:<Test>" test ID.
func killerPackage(id string) string {
	rest := strings.TrimPrefix(id, "go:")
	if i := strings.LastIndex(rest, ":"); i >= 0 {
		return rest[:i]
	}
	return rest
}

// classifyKillers splits killers into those whose package cannot observe the
// mutated file's package (unbounded reverse reachability) and those reachable
// only through more than maxHops reverse import edges. A non-Go file, or a file
// the graph does not know, has no package, so every killer is unreachable.
func classifyKillers(g *graph.Graph, file string, killers []string, maxHops int) (unreachable, beyond []string) {
	var hops map[string]int
	if strings.HasSuffix(file, ".go") {
		if pkg, ok := g.PackageOfFile(file); ok {
			hops = g.ReverseHops([]string{pkg})
		}
	}
	for _, k := range killers {
		h, reachable := hops[killerPackage(k)]
		switch {
		case !reachable:
			unreachable = append(unreachable, k)
		case h > maxHops:
			beyond = append(beyond, k)
		}
	}
	return unreachable, beyond
}

// normalizeOptions validates opts and fills defaults.
func normalizeOptions(opts Options) (Options, error) {
	if len(opts.Operators) == 0 {
		return opts, errors.New("mutate: no operators selected (valid: " + strings.Join(allOperators, ", ") + ")")
	}
	for _, op := range opts.Operators {
		if !isOperator(op) {
			return opts, fmt.Errorf("mutate: unknown operator %q (valid: %s)", op, strings.Join(allOperators, ", "))
		}
	}
	if opts.RepoDir == "" {
		return opts, errors.New("mutate: no repository directory")
	}
	if opts.Timeout <= 0 {
		return opts, errors.New("mutate: the per-mutant timeout must be positive")
	}
	if opts.Concurrency < 1 {
		opts.Concurrency = 1
	}
	if len(opts.Packages) == 0 {
		opts.Packages = []string{"./..."}
	}
	return opts, nil
}

// newLogger returns a printf-style progress logger that is safe for
// concurrent use and discards output when w is nil.
func newLogger(w io.Writer) func(format string, args ...any) {
	if w == nil {
		return func(string, ...any) {}
	}
	var mu sync.Mutex
	return func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		// Progress output is best effort: a failing log writer must not fail the run.
		_, _ = fmt.Fprintf(w, format+"\n", args...)
	}
}

// driver holds the state of one Run.
type driver struct {
	opts  Options
	sha   string
	logf  func(string, ...any)
	tools tools
	slots []string          // slot directories, one per worker
	base  map[string][]byte // original bytes of every file a selected mutant touches
	graph *graph.Graph
}

type cmdResult struct {
	Stdout, Stderr []byte
	Exit           int
}

// killTree ends a command and, on Windows, its child processes: go test starts
// the test binary as a child, and killing only go.exe would leave it running.
func killTree(p *os.Process) error {
	if runtime.GOOS == "windows" {
		// Best effort: the Kill below reports a failure to end the process itself.
		_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(p.Pid)).Run()
	}
	return p.Kill()
}

// runCmd runs name in dir. A nonzero exit is reported in Exit with a nil error;
// an error means the command could not run to completion or ctx ended.
func runCmd(ctx context.Context, dir, name string, args ...string) (cmdResult, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.Cancel = func() error { return killTree(cmd.Process) }
	cmd.WaitDelay = 15 * time.Second
	err := cmd.Run()
	res := cmdResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return res, ctxErr
	}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		res.Exit = exitErr.ExitCode()
	default:
		return res, fmt.Errorf("mutate: run %s %s: %w", name, strings.Join(args, " "), err)
	}
	return res, nil
}

// goArgs builds a go command line: sub, -json for test, optional -tags, flags,
// packages.
func (d *driver) goArgs(sub string, pkgs []string, flags ...string) []string {
	args := []string{sub}
	if sub == "test" {
		args = append(args, "-json")
	}
	if len(d.opts.Tags) > 0 {
		args = append(args, "-tags", strings.Join(d.opts.Tags, ","))
	}
	args = append(args, flags...)
	return append(args, pkgs...)
}

// baseline runs the whole suite twice in slot and requires both runs to pass.
// It returns the wall time of the second run and the median elapsed
// milliseconds of each top-level test across the two runs.
func (d *driver) baseline(ctx context.Context, slot string) (float64, map[string]int64, error) {
	var seconds float64
	var elapsed [2]map[string]float64
	for run := 1; run <= 2; run++ {
		d.logf("baseline run %d of 2 starting", run)
		start := time.Now()
		res, err := d.tools.goCmd(ctx, slot, d.goArgs("test", d.opts.Packages, "-count=1")...)
		if err != nil {
			return 0, nil, err
		}
		seconds = time.Since(start).Seconds()
		parsed, perr := parseGoTestJSON(bytes.NewReader(res.Stdout))
		switch {
		case perr != nil:
			return 0, nil, fmt.Errorf("mutate: baseline is not green (run %d): %w", run, perr)
		case res.Exit != 0 || len(parsed.Failed) > 0 || len(parsed.PackagesFailedWithoutTest) > 0 || len(parsed.BuildFailed) > 0 || parsed.TimedOut:
			return 0, nil, fmt.Errorf("mutate: baseline is not green (run %d): exit %d, failing tests %v, failing packages %v, build failures %v, timed out %t: %s",
				run, res.Exit, parsed.Failed, parsed.PackagesFailedWithoutTest, parsed.BuildFailed, parsed.TimedOut, stderrTail(res.Stderr))
		}
		if elapsed[run-1], err = baselineElapsed(res.Stdout); err != nil {
			return 0, nil, fmt.Errorf("%w (run %d)", err, run)
		}
		d.logf("baseline run %d of 2 passed in %.1fs (%d tests timed)", run, seconds, len(parsed.Durations))
	}
	ms, err := medianMS(elapsed[0], elapsed[1])
	if err != nil {
		return 0, nil, err
	}
	return seconds, ms, nil
}

// elapsedEvent is the part of a test2json event that carries a test's elapsed
// time. Elapsed is a pointer so that an absent field is not read as 0: go test
// reports 0.00 for a fast test, which is a measurement.
type elapsedEvent struct {
	Action  string
	Package string
	Test    string
	Elapsed *float64
}

// baselineElapsed reads the elapsed seconds of every top-level test that
// passed in a `go test -json` stream, keyed by test ID. Subtests have no key of
// their own. A passing test whose event carries no elapsed field, or a negative
// one, is an error naming the test: a duration is never invented.
func baselineElapsed(stdout []byte) (map[string]float64, error) {
	elapsed := map[string]float64{}
	for _, line := range bytes.Split(stdout, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("{")) {
			continue
		}
		var ev elapsedEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			return nil, fmt.Errorf("mutate: malformed go test -json event %q: %w", line, err)
		}
		if ev.Action != "pass" || ev.Package == "" || ev.Test == "" || strings.Contains(ev.Test, "/") {
			continue
		}
		id := testID(ev.Package, ev.Test)
		switch {
		case ev.Elapsed == nil:
			return nil, fmt.Errorf("mutate: baseline test %s has no elapsed time", id)
		case *ev.Elapsed < 0:
			return nil, fmt.Errorf("mutate: baseline test %s has a negative elapsed time (%g)", id, *ev.Elapsed)
		}
		elapsed[id] = *ev.Elapsed
	}
	return elapsed, nil
}

// medianMS merges the elapsed seconds of the two baseline runs into the
// median of each test in whole milliseconds, rounded half up. The median of
// two values is their mean. A test that only one run reported is an error naming
// the first such test by ID: the baseline must be the same suite twice.
func medianMS(first, second map[string]float64) (map[string]int64, error) {
	var onlyOne []string
	for id := range first {
		if _, ok := second[id]; !ok {
			onlyOne = append(onlyOne, id)
		}
	}
	for id := range second {
		if _, ok := first[id]; !ok {
			onlyOne = append(onlyOne, id)
		}
	}
	if len(onlyOne) > 0 {
		return nil, fmt.Errorf("mutate: baseline runs disagree on the test set: %s", slices.Min(onlyOne))
	}
	ms := make(map[string]int64, len(first))
	for id, a := range first {
		// Whole microseconds are finer than the decimals go test prints, and make
		// the sum and the half-up rounding exact: (0.01+0.011)*500 comes out just
		// under 10.5 in floating point.
		sum := microseconds(a) + microseconds(second[id])
		ms[id] = (sum + 1000) / 2000
	}
	return ms, nil
}

// microseconds converts non-negative seconds to the nearest whole microsecond.
func microseconds(seconds float64) int64 { return int64(math.Round(seconds * 1e6)) }

// classifyRun names the outcome of a mutant's first test run. A failing test
// is only a candidate kill: it is confirmed by a rerun. A package that failed
// without a failing test, or a nonzero exit that nothing explains, is
// unconfirmed rather than survived: unknown is never a pass.
func classifyRun(exit int, run testRun) string {
	switch {
	case len(run.BuildFailed) > 0:
		return "build_error"
	case run.TimedOut:
		return "timeout"
	case len(run.Failed) > 0:
		return "killed"
	case exit != 0 || len(run.PackagesFailedWithoutTest) > 0:
		return "unconfirmed"
	}
	return "survived"
}

// intersectKillers returns the test IDs present in both a and b, in a's order.
func intersectKillers(a, b []string) []string {
	var out []string
	for _, id := range a {
		if slices.Contains(b, id) {
			out = append(out, id)
		}
	}
	return out
}

// killerName returns the test name of a "go:<pkg>:<Test>" test ID.
func killerName(id string) string {
	return id[strings.LastIndex(id, ":")+1:]
}

// rerunPlan returns the packages that hold the killers and the -run pattern
// that selects exactly their test names, both sorted and without duplicates.
func rerunPlan(killers []string) (pkgs []string, pattern string) {
	var names []string
	for _, k := range killers {
		pkgs = append(pkgs, killerPackage(k))
		names = append(names, killerName(k))
	}
	slices.Sort(pkgs)
	slices.Sort(names)
	return slices.Compact(pkgs), "^(" + strings.Join(slices.Compact(names), "|") + ")$"
}

// timeoutGrace is how long a mutant's go test may run beyond its -timeout
// before the driver ends it: building and linking the test binaries count
// against the deadline too.
const timeoutGrace = 30 * time.Second

// goTestState says how a mutant's go test ended.
type goTestState int

const (
	testCompleted  goTestState = iota // the command ran and its output parsed
	testDeadline                      // the driver's per-mutant deadline ended it
	testUnreadable                    // the command ran but its output cannot be read
)

// goTest runs go test for a mutant under the per-mutant deadline. With
// pattern set it is the confirming rerun: uncached, restricted to the named
// tests. err is set for anything that is not a result of the mutant: the
// caller's context ending, or a go command that cannot run.
func (d *driver) goTest(ctx context.Context, slot string, pkgs []string, pattern string) (cmdResult, testRun, goTestState, error) {
	tctx, cancel := context.WithTimeout(ctx, d.opts.Timeout+timeoutGrace)
	defer cancel()
	var flags []string
	if pattern != "" {
		flags = append(flags, "-count=1")
	}
	flags = append(flags, "-timeout", d.opts.Timeout.String())
	if pattern != "" {
		flags = append(flags, "-run", pattern)
	}
	res, err := d.tools.goCmd(tctx, slot, d.goArgs("test", pkgs, flags...)...)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return res, testRun{}, testCompleted, ctxErr
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return res, testRun{}, testDeadline, nil
		}
		return res, testRun{}, testCompleted, err
	}
	run, perr := parseGoTestJSON(bytes.NewReader(res.Stdout))
	if perr != nil {
		d.logf("unreadable go test output in %s: %v", slot, perr)
		return res, testRun{}, testUnreadable, nil
	}
	return res, run, testCompleted, nil
}

// evaluate builds and tests the mutant applied in slot and returns its status
// and, for a killed mutant, the killers both runs agree on. Whatever cannot be
// read or confirmed is unconfirmed, never survived and never killed.
func (d *driver) evaluate(ctx context.Context, slot string) (status string, killers []string, err error) {
	build, err := d.tools.goCmd(ctx, slot, d.goArgs("build", d.opts.Packages)...)
	if err != nil {
		return "", nil, err
	}
	if build.Exit != 0 {
		return "build_error", nil, nil
	}
	res, first, state, err := d.goTest(ctx, slot, d.opts.Packages, "")
	switch {
	case err != nil:
		return "", nil, err
	case state == testDeadline:
		return "timeout", nil, nil
	case state == testUnreadable:
		return "unconfirmed", nil, nil
	}
	if status = classifyRun(res.Exit, first); status != "killed" {
		return status, nil, nil
	}
	pkgs, pattern := rerunPlan(first.Failed)
	_, second, state, err := d.goTest(ctx, slot, pkgs, pattern)
	switch {
	case err != nil:
		return "", nil, err
	case state != testCompleted:
		return "unconfirmed", nil, nil
	}
	if killers = intersectKillers(first.Failed, second.Failed); len(killers) == 0 {
		return "unconfirmed", nil, nil
	}
	return "killed", killers, nil
}

// runMutant applies m in slot, evaluates it and restores the slot. A restore
// that fails is an error even when the mutant was evaluated: the slot is no
// longer the base.
func (d *driver) runMutant(ctx context.Context, slot string, m Mutant) (rec Record, err error) {
	start := time.Now()
	path := filepath.Join(slot, filepath.FromSlash(m.File))
	base := d.base[m.File]
	writeErr := writeMutant(path, base, m)
	defer func() {
		if restoreErr := restoreFile(path, base); restoreErr != nil {
			err = errors.Join(err, fmt.Errorf("mutate: restore %s in %s: %w", m.File, slot, restoreErr))
		}
	}()
	if writeErr != nil {
		return Record{}, writeErr
	}
	d.logf("%s applied %s:%d %s", m.ID, m.File, m.Line, m.Operator)
	status, killers, err := d.evaluate(ctx, slot)
	if err != nil {
		return Record{}, err
	}
	rec = Record{
		ID: m.ID, File: m.File, Line: m.Line, Operator: m.Operator, Original: m.Original, Mutated: m.Mutated,
		BaseSHA: d.sha, Status: status, Killers: killers,
	}
	if status == "killed" {
		rec.UnreachableKillers, rec.BeyondMaxHopsKillers = classifyKillers(d.graph, m.File, killers, d.opts.MaxHops)
	}
	rec.DurationMS = time.Since(start).Milliseconds()
	d.logf("%s %s %s:%d in %dms", m.ID, status, m.File, m.Line, rec.DurationMS)
	return rec, nil
}

// execute runs the selected mutants, one per slot at a time, and returns one
// record each. onRecord sees every record as soon as it is known. The first
// failure ends the run; the slots that are mid-mutant restore before returning.
func (d *driver) execute(ctx context.Context, selected []Mutant, onRecord func(Record) error) ([]Record, error) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu       sync.Mutex
		recs     []Record
		firstErr error
	)
	fail := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
		cancel()
	}
	jobs := make(chan Mutant)
	var wg sync.WaitGroup
	for _, slot := range d.slots {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for m := range jobs {
				if runCtx.Err() != nil {
					continue
				}
				rec, err := d.runMutant(runCtx, slot, m)
				if err != nil {
					fail(err)
					continue
				}
				mu.Lock()
				recs = append(recs, rec)
				appendErr := onRecord(rec)
				mu.Unlock()
				if appendErr != nil {
					fail(appendErr)
				}
			}
		}()
	}
feed:
	for _, m := range selected {
		select {
		case jobs <- m:
		case <-runCtx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	switch {
	case firstErr != nil:
		return nil, firstErr
	case ctx.Err() != nil:
		return nil, ctx.Err()
	}
	return recs, nil
}

// writeFileRetry writes data to path, retrying briefly: on Windows a scanner
// can hold a just-written file open for a moment.
func writeFileRetry(path string, data []byte) error {
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			time.Sleep(100 * time.Millisecond)
		}
		if err = os.WriteFile(path, data, 0o644); err == nil {
			return nil
		}
	}
	return err
}

// writeMutant checks that the file at path holds the base bytes and writes the
// mutant's bytes in their place.
func writeMutant(path string, base []byte, m Mutant) error {
	current, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("mutate: read %s: %w", path, err)
	}
	if sha256.Sum256(current) != sha256.Sum256(base) {
		return fmt.Errorf("mutate: %s does not hold the base bytes of %s", path, m.File)
	}
	mutated, err := Apply(base, m)
	if err != nil {
		return err
	}
	if err := writeFileRetry(path, mutated); err != nil {
		return fmt.Errorf("mutate: write mutant %s: %w", m.ID, err)
	}
	return nil
}

// restoreFile writes the base bytes back to path and checks them by hash.
func restoreFile(path string, base []byte) error {
	if err := writeFileRetry(path, base); err != nil {
		return err
	}
	got, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if sha256.Sum256(got) != sha256.Sum256(base) {
		return fmt.Errorf("the file holds %d bytes after the restore, want the %d base bytes", len(got), len(base))
	}
	return nil
}
