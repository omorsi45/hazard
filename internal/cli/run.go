package cli

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/omorsi45/hazard/internal/config"
	"github.com/omorsi45/hazard/internal/engine"
	"github.com/omorsi45/hazard/internal/runner"
	"github.com/omorsi45/hazard/internal/telemetry"
)

// runFunc runs one native test runner; tests substitute a fake.
type runFunc func(ctx context.Context, req runner.Request) (runner.Result, error)

// runners are the native runner adapters `hazard run` drives.
type runners struct{ Go, Python runFunc }

func nativeRunners() runners { return runners{Go: runner.RunGo, Python: runner.RunPytest} }

// exitError ends a command with a specific process exit code. An empty
// message means the command already reported the outcome (a runner's own
// output), so nothing more is printed.
type exitError struct {
	code int
	msg  string
}

func (e exitError) Error() string { return e.msg }

// exitCodeOf maps a command error to the process exit code and says whether
// the error needs no message of its own. notFound marks a command that could
// not be found, a usage error.
func exitCodeOf(err error, notFound bool) (code int, silent bool) {
	var exit exitError
	var usage usageError
	switch {
	case errors.As(err, &exit):
		return exit.code, exit.msg == ""
	case errors.As(err, &usage) || notFound:
		return 2, false
	}
	return 70, false
}

// Abstain reasons `run` records itself.
const (
	reasonForcedFull    = "forced_full"
	reasonRunnerLimit   = "runner_limit"
	reasonInternalError = "internal_error"
	reasonShadowMode    = "shadow"
)

// lockReasons are the full-suite reasons that mean the calibrated lock could
// not be used: it is missing or unusable, no longer matches the running system,
// or its predictor could not be built or gave no usable answer. With
// --no-fallback they end the command with exit 3 before any test runs.
var lockReasons = map[string]bool{
	"no_lock": true, "lock_corrupt": true, "lock_error": true,
	"model_drift": true, "rubric_mismatch": true, "feature_mismatch": true, "policy_mismatch": true,
	"predictor_unavailable": true, "predictor_error": true,
}

const (
	langGo     = "go"
	langPython = "python"
)

const runLong = `Select the tests the change could break, run them with the native runner and
exit with the runner's own exit code. Whenever the selection cannot be trusted
the full suite runs. Arguments after -- go to every runner that runs; none may
narrow the tests.`

func newRunCommand(sel selectFunc, rn runners) *cobra.Command {
	cmd := &cobra.Command{Use: "run [flags] [-- runner arguments]", Short: "Select and run tests", Long: runLong, Args: cobra.ArbitraryArgs}
	selectFlags(cmd)
	f := cmd.Flags()
	f.String("runner", "auto", "auto|gotest|pytest")
	f.String("junit", ".hazard/junit.xml", "JUnit output path")
	f.Bool("full", false, "force full suite")
	f.Bool("no-fallback", false, "return internal error without full-suite fallback")
	cmd.RunE = func(cmd *cobra.Command, args []string) error { return runCommand(cmd, args, sel, rn) }
	return cmd
}

// runFlags are the flags `run` adds to the select flags.
type runFlags struct {
	runner, junit, junitPath string
	full, noFallback         bool
	passthrough              []string
}

func parseRunFlags(cmd *cobra.Command, args []string) (runFlags, error) {
	f := cmd.Flags()
	var rf runFlags
	rf.runner, _ = f.GetString("runner")
	rf.junit, _ = f.GetString("junit")
	rf.full, _ = f.GetBool("full")
	rf.noFallback, _ = f.GetBool("no-fallback")
	if !oneOf(rf.runner, "auto", "gotest", "pytest") {
		return rf, usageError{fmt.Errorf("invalid --runner %q: want auto, gotest or pytest", rf.runner)}
	}
	if rf.junit == "" {
		return rf, usageError{errors.New("--junit must not be empty")}
	}
	switch dash := cmd.ArgsLenAtDash(); {
	case dash == -1 && len(args) > 0, dash > 0:
		return rf, usageError{errors.New("run takes no positional arguments; pass runner arguments after --")}
	case dash >= 0:
		rf.passthrough = args[dash:]
	}
	for _, name := range []string{"format", "out", "dry-run-payload"} {
		if f.Changed(name) {
			return rf, usageError{fmt.Errorf("--%s belongs to hazard select; run does not support it", name)}
		}
	}
	return rf, nil
}

// planLanguages is the adapters that may run, in order: Go, then Python.
func planLanguages(cfg config.Config, runnerFlag string) ([]string, error) {
	goOn, pyOn := cfg.Languages.Go.Enabled, cfg.Languages.Python.Enabled
	switch runnerFlag {
	case "gotest":
		if !goOn {
			return nil, usageError{errors.New("--runner gotest: Go is disabled in the configuration")}
		}
		return []string{langGo}, nil
	case "pytest":
		if !pyOn {
			return nil, usageError{errors.New("--runner pytest: Python is disabled in the configuration")}
		}
		return []string{langPython}, nil
	}
	var langs []string
	if goOn {
		langs = append(langs, langGo)
	}
	if pyOn {
		langs = append(langs, langPython)
	}
	return langs, nil
}

// job is one adapter invocation: a language's full suite or its targets.
type job struct {
	lang    string
	full    bool
	targets []runner.Target
	junit   string
	failed  bool // the adapter ended in an internal error
}

func (j job) name() string {
	if j.lang == langGo {
		return "go test"
	}
	return "pytest"
}

// execution is one `hazard run`.
type execution struct {
	ctx            context.Context
	root           string
	cfg            config.Config
	rf             runFlags
	rn             runners
	stdout, stderr io.Writer
	widened        string // why a runner widened the run to a full suite; empty when it did not
}

func (x *execution) warnf(format string, args ...any) {
	_, _ = fmt.Fprintf(x.stderr, "warning: "+format+"\n", args...)
}

func (x *execution) widen(reason string) {
	if x.widened == "" {
		x.widened = reason
	}
}

func runCommand(cmd *cobra.Command, args []string, sel selectFunc, rn runners) error {
	req, err := parseSelectFlags(cmd)
	if err != nil {
		return err
	}
	rf, err := parseRunFlags(cmd, args)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	if err := ctx.Err(); err != nil {
		return err
	}
	root, err := repoRoot(ctx)
	if err != nil {
		return err
	}
	cfg, err := loadIndexConfig(root, req.config)
	if err != nil {
		return usageError{err}
	}
	langs, err := planLanguages(cfg, rf.runner)
	if err != nil {
		return err
	}
	for _, lang := range langs {
		if err := runner.ValidatePassthrough(lang, rf.passthrough); err != nil {
			return usageError{err}
		}
	}
	rf.junitPath = filepath.Join(root, filepath.FromSlash(rf.junit))
	if cmd.Flags().Changed("junit") {
		if rf.junitPath, err = filepath.Abs(rf.junit); err != nil {
			return usageError{fmt.Errorf("--junit: %w", err)}
		}
	}
	x := &execution{ctx: ctx, root: root, cfg: cfg, rf: rf, rn: rn, stdout: cmd.OutOrStdout(), stderr: cmd.ErrOrStderr()}
	return x.run(req, langs, sel)
}

func (x *execution) run(req selectRequest, langs []string, sel selectFunc) error {
	d, err := x.decide(req, sel)
	if err != nil {
		return err
	}
	for _, w := range d.Warnings {
		x.warnf("%s", w)
	}
	if x.rf.noFallback && d.FullSuite && lockReasons[d.AbstainReason] {
		return exitError{code: 3, msg: fmt.Sprintf("no usable lock (%s) and --no-fallback is set; nothing was run", d.AbstainReason)}
	}
	if req.explain {
		if _, err := fmt.Fprint(x.stderr, renderText(d, req)); err != nil {
			return err
		}
	}
	jobs, limited := x.jobsFor(d, langs)
	if limited {
		x.warnf("a selected test cannot be expressed to its runner; running the full suite")
		x.widen(reasonRunnerLimit)
	}
	x.announce(d, jobs)

	exit, err := x.execute(jobs)
	if err != nil {
		return err
	}

	mode := telemetry.ModeRun
	if req.shadow {
		mode = telemetry.ModeShadow
	}
	rec := telemetry.FromDecision(d, mode, version, time.Now())
	if x.widened != "" && !rec.FullSuite {
		rec.FullSuite, rec.AbstainReason = true, x.widened
	}
	if rec.Commit == "" {
		rec.Commit = headSHA(x.ctx, x.root)
	}
	rec.RunnerExit, rec.JUnit = &exit, x.rf.junit
	if err := writeTelemetry(x.root, x.cfg, rec); err != nil {
		x.warnf("telemetry: %v", err)
	}
	if exit != 0 {
		return exitError{code: exit}
	}
	return nil
}

// decide makes the selection decision: forced full, from the selector, or the
// full-suite fallback of an internal error.
func (x *execution) decide(req selectRequest, sel selectFunc) (engine.Decision, error) {
	if x.rf.full {
		return fullSuiteDecision(reasonForcedFull)
	}
	d, err := sel(x.ctx, req.engineOptions(x.root))
	if err != nil {
		mapped := mapEngineError(err)
		var usage usageError
		switch {
		case errors.As(mapped, &usage):
			return d, mapped
		case x.ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
			return d, err
		case x.rf.noFallback:
			return d, fmt.Errorf("selection failed and --no-fallback is set: %w", err)
		}
		x.warnf("selection failed (%v); running the full suite", err)
		return fullSuiteDecision(reasonInternalError)
	}
	if req.shadow {
		d.FullSuite, d.Tests = true, nil
		if d.AbstainReason == "" {
			d.AbstainReason = reasonShadowMode
		}
	}
	return d, nil
}

func fullSuiteDecision(reason string) (engine.Decision, error) {
	id, err := telemetry.NewID()
	if err != nil {
		return engine.Decision{}, err
	}
	return engine.Decision{ID: id, FullSuite: true, AbstainReason: reason}, nil
}

// jobsFor turns the decision into adapter jobs, Go before Python. A full-suite
// decision runs every adapter's full suite. A narrowed one runs each adapter
// that has targets. A test that cannot be expressed to its runner (unknown
// language, missing package or runner ID, or a language that is disabled)
// widens the whole run to the full suite: limited is true.
func (x *execution) jobsFor(d engine.Decision, langs []string) (jobs []job, limited bool) {
	full := func() []job {
		out := make([]job, len(langs))
		for i, lang := range langs {
			out[i] = job{lang: lang, full: true}
		}
		return out
	}
	if d.FullSuite {
		return full(), false
	}
	targets := map[string][]runner.Target{}
	for _, s := range d.Tests {
		switch {
		case strings.HasPrefix(s.TestID, "go:"):
			if s.Package == "" || s.RunnerID == "" || !x.cfg.Languages.Go.Enabled {
				return full(), true
			}
			targets[langGo] = append(targets[langGo], runner.Target{ID: s.TestID, Language: langGo, Package: s.Package, Name: s.RunnerID})
		case strings.HasPrefix(s.TestID, "py:"):
			if s.RunnerID == "" || !x.cfg.Languages.Python.Enabled {
				return full(), true
			}
			targets[langPython] = append(targets[langPython], runner.Target{ID: s.TestID, Language: langPython, Package: s.Package, RunnerID: s.RunnerID})
		default:
			return full(), true
		}
	}
	for _, lang := range langs {
		if len(targets[lang]) > 0 {
			jobs = append(jobs, job{lang: lang, targets: targets[lang]})
		}
	}
	return jobs, false
}

func (x *execution) announce(d engine.Decision, jobs []job) {
	var msg string
	switch {
	case d.FullSuite || x.widened != "":
		reason := d.AbstainReason
		if !d.FullSuite {
			reason = x.widened
		}
		msg = fmt.Sprintf("running the full suite (%s)", reason)
	case len(jobs) == 0:
		msg = "no tests selected, nothing to run"
	default:
		msg = fmt.Sprintf("running %d of %d tests", len(d.Tests), len(d.AllTests))
	}
	_, _ = fmt.Fprintf(x.stderr, "hazard: %s\n", msg)
}

// execute runs the jobs and publishes the JUnit report. It returns the first
// nonzero runner exit code, else 0. The error is non-nil only when the run
// must stop at once: cancellation, or an internal error under --no-fallback.
func (x *execution) execute(jobs []job) (exit int, err error) {
	final := x.rf.junitPath
	switch {
	case len(jobs) == 0:
		return 0, writeFileAtomic(final, []byte(xml.Header+"<testsuites></testsuites>\n"))
	case len(jobs) > 1:
		// Two adapters would overwrite each other's report: each writes a
		// part, merged below. A report left by an earlier run goes first.
		if err := os.Remove(final); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return 0, fmt.Errorf("remove stale JUnit report: %w", err)
		}
		for i := range jobs {
			jobs[i].junit = final + "." + jobs[i].lang + ".part"
		}
		defer x.removeParts(jobs)
	default:
		jobs[0].junit = final
	}
	for i := range jobs {
		code, err := x.runJob(&jobs[i])
		if err != nil {
			return 70, err
		}
		if exit == 0 {
			exit = code
		}
	}
	if len(jobs) > 1 {
		if err := x.mergeParts(final, jobs); err != nil {
			x.warnf("JUnit report not written: %v", err)
		}
	}
	return exit, nil
}

// runJob runs one adapter. An internal runner error, or a pytest abstention,
// widens the job to its full suite once. The error is non-nil only for
// cancellation or an internal error under --no-fallback.
func (x *execution) runJob(j *job) (int, error) {
	for {
		res, err := x.invoke(*j)
		switch {
		case err != nil:
			if x.ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return 70, err
			}
			if x.rf.noFallback {
				return 70, fmt.Errorf("%s failed and --no-fallback is set: %w", j.name(), err)
			}
			if j.full {
				x.warnf("%s failed: %v", j.name(), err)
				j.failed = true
				return 70, nil
			}
			x.warnf("%s failed (%v); running its full suite", j.name(), err)
			x.widen(reasonInternalError)
			j.full = true
		case res.Abstained && !j.full:
			x.warnf("%d selected test(s) were not found by %s (%s); running its full suite", len(res.Missing), j.name(), res.Reason)
			x.widen(reasonRunnerLimit)
			j.full = true
		default:
			return res.ExitCode, nil
		}
	}
}

func (x *execution) invoke(j job) (runner.Result, error) {
	req := runner.Request{
		RepoDir: x.root, Full: j.full, Passthrough: x.rf.passthrough, JUnitPath: j.junit,
		Stdout: x.stdout, Stderr: x.stderr,
	}
	run := x.rn.Go
	switch j.lang {
	case langGo:
		req.GoPackages, req.GoTags = x.cfg.Languages.Go.Packages, x.cfg.Languages.Go.Tags
	case langPython:
		run = x.rn.Python
		req.Testpaths, req.PytestArgs = x.cfg.Languages.Python.Testpaths, x.cfg.Languages.Python.PytestArgs
	}
	if !j.full {
		req.Targets = j.targets
	}
	return run(x.ctx, req)
}

func (x *execution) removeParts(jobs []job) {
	for _, j := range jobs {
		if err := os.Remove(j.junit); err != nil && !errors.Is(err, fs.ErrNotExist) {
			x.warnf("remove %s: %v", j.junit, err)
		}
	}
}

// mergeParts publishes the union of the adapters' reports at final. A report
// missing a language would hide its failures, so unless every adapter wrote its
// part nothing is published.
func (x *execution) mergeParts(final string, jobs []job) error {
	parts := make([]string, len(jobs))
	for i, j := range jobs {
		if j.failed {
			return fmt.Errorf("%s ended in an internal error", j.name())
		}
		if _, err := os.Stat(j.junit); err != nil {
			return fmt.Errorf("%s wrote no report: %w", j.name(), err)
		}
		parts[i] = j.junit
	}
	return mergeJUnit(final, parts...)
}

// junitRoot is the root element of a JUnit report, kept as raw XML.
type junitRoot struct {
	XMLName xml.Name
	Attrs   []xml.Attr `xml:",any,attr"`
	Inner   []byte     `xml:",innerxml"`
}

// mergeJUnit writes one <testsuites> report at dst holding every suite of the
// parts, in order.
func mergeJUnit(dst string, parts ...string) error {
	var b bytes.Buffer
	b.WriteString(xml.Header + "<testsuites>\n")
	for _, part := range parts {
		data, err := os.ReadFile(part)
		if err != nil {
			return err
		}
		var root junitRoot
		if err := xml.Unmarshal(data, &root); err != nil {
			return fmt.Errorf("%s: %w", part, err)
		}
		switch root.XMLName.Local {
		case "testsuites":
			b.Write(bytes.TrimSpace(root.Inner))
		case "testsuite":
			b.WriteString("<testsuite")
			for _, a := range root.Attrs {
				b.WriteString(" " + a.Name.Local + `="`)
				if err := xml.EscapeText(&b, []byte(a.Value)); err != nil {
					return err
				}
				b.WriteString(`"`)
			}
			b.WriteString(">")
			b.Write(root.Inner)
			b.WriteString("</testsuite>")
		default:
			return fmt.Errorf("%s: unexpected JUnit root %q", part, root.XMLName.Local)
		}
		b.WriteString("\n")
	}
	b.WriteString("</testsuites>\n")
	return writeFileAtomic(dst, b.Bytes())
}

// headSHA is the commit HEAD names in dir, or "" when there is none (an empty
// repository): a record without a commit is still a valid record.
func headSHA(ctx context.Context, dir string) string {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// writeTelemetry appends rec when telemetry is enabled: to the configured JSONL
// file, relative to the repository root, and to the database when it exists.
func writeTelemetry(root string, cfg config.Config, rec telemetry.Record) error {
	if !cfg.Telemetry.Enabled {
		return nil
	}
	path := cfg.Telemetry.Path
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, filepath.FromSlash(path))
	}
	return telemetry.Write(path, filepath.Join(root, ".hazard", "hazard.db"), rec)
}

// recordSelection wraps sel so that every real selection is appended to
// telemetry. A payload dry run is not a selection and an error has no
// decision, so neither is recorded. A telemetry failure becomes a warning on
// the decision: it never changes the outcome.
func recordSelection(sel selectFunc) selectFunc {
	return func(ctx context.Context, opts engine.Options) (engine.Decision, error) {
		d, err := sel(ctx, opts)
		if err != nil || opts.DryRunPayload {
			return d, err
		}
		cfg, err := loadIndexConfig(opts.RepoDir, opts.ConfigPath)
		if err != nil {
			d.Warnings = append(d.Warnings, "telemetry: not recorded: "+err.Error())
			return d, nil
		}
		mode := telemetry.ModeSelect
		if opts.Shadow {
			mode = telemetry.ModeShadow
		}
		rec := telemetry.FromDecision(d, mode, version, time.Now())
		if rec.Commit == "" {
			rec.Commit = headSHA(ctx, opts.RepoDir)
		}
		if err := writeTelemetry(opts.RepoDir, cfg, rec); err != nil {
			d.Warnings = append(d.Warnings, "telemetry: "+err.Error())
		}
		return d, nil
	}
}
