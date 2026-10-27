package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/omorsi45/hazard/internal/engine"
)

// selectFunc is the selection pipeline; tests substitute a fake.
type selectFunc func(ctx context.Context, opts engine.Options) (engine.Decision, error)

// selectRequest is the validated flag set of `hazard select`.
type selectRequest struct {
	base, head, config, lock, predictor, redact, format, out string
	recall, budgetUSD                                        float64
	maxCandidates, budgetMS                                  int
	shadow, dryRun, explain                                  bool
	changed                                                  map[string]bool // flags the user set explicitly
}

func newSelectCommand(sel selectFunc) *cobra.Command {
	cmd := &cobra.Command{
		Use: "select", Short: "Select tests without running them",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 0 {
				return usageError{errors.New("select accepts no positional arguments")}
			}
			return nil
		},
	}
	selectFlags(cmd)
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		req, err := parseSelectFlags(cmd)
		if err != nil {
			return err
		}
		if err := cmd.Context().Err(); err != nil {
			return err
		}
		root, err := repoRoot(cmd.Context())
		if err != nil {
			return err
		}
		d, err := sel(cmd.Context(), req.engineOptions(root))
		if err != nil {
			return mapEngineError(err)
		}
		if req.shadow {
			// The engine already prints the full suite under shadow; this keeps
			// the command correct for any selection function.
			d.FullSuite, d.Tests = true, nil
		}
		data, err := renderSelection(d, req)
		if err != nil {
			return err
		}
		if req.dryRun || req.format != "json" {
			for _, w := range d.Warnings {
				if _, err := fmt.Fprintln(cmd.ErrOrStderr(), "warning:", w); err != nil {
					return err
				}
			}
		}
		if req.out != "" {
			return writeFileAtomic(req.out, data)
		}
		_, err = cmd.OutOrStdout().Write(data)
		return err
	}
	return cmd
}

// parseSelectFlags reads and validates the select flags; every violation is a
// usage error, reported before anything runs.
func parseSelectFlags(cmd *cobra.Command) (selectRequest, error) {
	f := cmd.Flags()
	req := selectRequest{changed: map[string]bool{}}
	req.base, _ = f.GetString("base")
	req.head, _ = f.GetString("head")
	req.config, _ = f.GetString("config")
	req.lock, _ = f.GetString("lock")
	req.predictor, _ = f.GetString("predictor")
	req.redact, _ = f.GetString("redact")
	req.format, _ = f.GetString("format")
	req.out, _ = f.GetString("out")
	req.recall, _ = f.GetFloat64("recall")
	req.budgetUSD, _ = f.GetFloat64("budget-usd")
	req.maxCandidates, _ = f.GetInt("max-candidates")
	req.budgetMS, _ = f.GetInt("budget-ms")
	req.shadow, _ = f.GetBool("shadow")
	req.dryRun, _ = f.GetBool("dry-run-payload")
	req.explain, _ = f.GetBool("explain")
	for _, name := range []string{"lock", "recall", "max-candidates", "budget-ms", "budget-usd"} {
		req.changed[name] = f.Changed(name)
	}

	if f.Changed("recall") {
		confirmed, _ := f.GetBool("i-know-what-im-doing")
		if !confirmed || math.IsNaN(req.recall) || math.IsInf(req.recall, 0) || req.recall <= 0 || req.recall >= 1 {
			return req, usageError{errors.New("invalid --recall override: requires --i-know-what-im-doing and a value in (0,1)")}
		}
	}
	if !oneOf(req.predictor, "", "static", "logistic", "jev", "ensemble") {
		return req, usageError{fmt.Errorf("invalid --predictor %q: want static, logistic, jev or ensemble", req.predictor)}
	}
	if !oneOf(req.redact, "", "none", "structure", "hash") {
		return req, usageError{fmt.Errorf("invalid --redact %q: want none, structure or hash", req.redact)}
	}
	if jsonMode, _ := f.GetBool("json"); jsonMode {
		if f.Changed("format") && req.format != "json" {
			return req, usageError{fmt.Errorf("--json conflicts with --format %s", req.format)}
		}
		req.format = "json"
	}
	if !oneOf(req.format, "text", "json", "go-run", "pytest-file") {
		return req, usageError{fmt.Errorf("invalid --format %q: want text, json, go-run or pytest-file", req.format)}
	}
	if req.maxCandidates <= 0 {
		return req, usageError{errors.New("--max-candidates must be positive")}
	}
	if req.budgetMS <= 0 {
		return req, usageError{errors.New("--budget-ms must be positive")}
	}
	if math.IsNaN(req.budgetUSD) || math.IsInf(req.budgetUSD, 0) || req.budgetUSD < 0 {
		return req, usageError{errors.New("--budget-usd must be a finite number, zero or more")}
	}
	return req, nil
}

func oneOf(v string, options ...string) bool {
	for _, o := range options {
		if v == o {
			return true
		}
	}
	return false
}

// engineOptions maps the request to engine options. Limits the user did not
// set stay zero so the configuration supplies them.
func (r selectRequest) engineOptions(root string) engine.Options {
	opts := engine.Options{
		RepoDir: root, Base: r.base, Head: r.head, ConfigPath: r.config, Predictor: r.predictor, Redact: r.redact,
		Shadow: r.shadow, DryRunPayload: r.dryRun, Explain: r.explain,
	}
	if r.changed["lock"] {
		if abs, err := filepath.Abs(r.lock); err == nil {
			opts.LockPath = abs
		} else {
			opts.LockPath = r.lock
		}
	}
	if r.changed["recall"] {
		opts.Recall = r.recall
	}
	if r.changed["max-candidates"] {
		opts.MaxCandidates = r.maxCandidates
	}
	if r.changed["budget-ms"] {
		opts.BudgetMS = r.budgetMS
	}
	if r.changed["budget-usd"] {
		opts.BudgetUSD = r.budgetUSD
	}
	return opts
}

// mapEngineError turns invalid-invocation errors into usage errors.
func mapEngineError(err error) error {
	if errors.Is(err, engine.ErrInvalidRef) || errors.Is(err, engine.ErrInvalidConfig) {
		return usageError{err}
	}
	return err
}

// repoRoot is the git top level of the current working directory.
func repoRoot(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", ctxErr
	}
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		return "", usageError{fmt.Errorf("not inside a git repository: %s", strings.TrimSpace(stderr.String()))}
	case err != nil:
		return "", fmt.Errorf("run git: %w", err)
	}
	top := strings.TrimSpace(stdout.String())
	if top == "" {
		return "", errors.New("git rev-parse --show-toplevel printed nothing")
	}
	return filepath.Abs(filepath.FromSlash(top))
}

// reasonOf is the abstain reason shown for a full-suite decision.
func reasonOf(d engine.Decision, shadow bool) string {
	switch {
	case shadow:
		return "shadow"
	case d.AbstainReason == "":
		return "unknown"
	}
	return d.AbstainReason
}

// renderSelection formats a decision for the requested output.
func renderSelection(d engine.Decision, req selectRequest) ([]byte, error) {
	if req.dryRun {
		return d.Payload, nil
	}
	switch req.format {
	case "json":
		data, err := json.Marshal(d)
		if err != nil {
			return nil, fmt.Errorf("encode decision: %w", err)
		}
		return append(data, '\n'), nil
	case "go-run":
		return []byte(renderGoRun(d, req.shadow)), nil
	case "pytest-file":
		return []byte(renderPytestFile(d, req.shadow)), nil
	}
	return []byte(renderText(d, req)), nil
}

func renderText(d engine.Decision, req selectRequest) string {
	var b strings.Builder
	line := func(prefix string, s engine.Selected) {
		b.WriteString(prefix + s.TestID)
		if req.explain {
			b.WriteString("\t" + s.Source + "\t" + s.Reason)
		}
		b.WriteString("\n")
	}
	if d.FullSuite {
		fmt.Fprintf(&b, "full suite: %s\n", reasonOf(d, req.shadow))
		for _, s := range d.Hypothetical {
			line("hypothetical ", s)
		}
		return b.String()
	}
	fmt.Fprintf(&b, "selected %d of %d tests\n", len(d.Tests), len(d.AllTests))
	for _, s := range d.Tests {
		line("", s)
	}
	return b.String()
}

const runnerLimit = "#full-suite runner_limit\n"

// renderGoRun prints one `go test` line per package: the import path and a
// -run expression. Python tests belong to pytest-file and are left out; a Go
// test the runner cannot name, or an unknown language, widens to the full
// suite.
func renderGoRun(d engine.Decision, shadow bool) string {
	if d.FullSuite {
		return "#full-suite " + reasonOf(d, shadow) + "\n"
	}
	byPkg := map[string][]string{}
	for _, s := range d.Tests {
		switch {
		case strings.HasPrefix(s.TestID, "go:"):
			if s.Package == "" || s.RunnerID == "" {
				return runnerLimit
			}
			byPkg[s.Package] = append(byPkg[s.Package], s.RunnerID)
		case strings.HasPrefix(s.TestID, "py:"):
		default:
			return runnerLimit
		}
	}
	pkgs := make([]string, 0, len(byPkg))
	for p := range byPkg {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)
	var b strings.Builder
	for _, p := range pkgs {
		names := byPkg[p]
		sort.Strings(names)
		quoted := make([]string, 0, len(names))
		for i, n := range names {
			if i > 0 && n == names[i-1] {
				continue
			}
			quoted = append(quoted, regexp.QuoteMeta(n))
		}
		fmt.Fprintf(&b, "%s ^(%s)$\n", p, strings.Join(quoted, "|"))
	}
	return b.String()
}

// renderPytestFile prints one pytest node ID per Python test.
func renderPytestFile(d engine.Decision, shadow bool) string {
	if d.FullSuite {
		return "#full-suite " + reasonOf(d, shadow) + "\n"
	}
	var b strings.Builder
	for _, s := range d.Tests {
		switch {
		case strings.HasPrefix(s.TestID, "py:"):
			if s.RunnerID == "" {
				return runnerLimit
			}
			b.WriteString(s.RunnerID + "\n")
		case strings.HasPrefix(s.TestID, "go:"):
		default:
			return runnerLimit
		}
	}
	return b.String()
}

// writeFileAtomic writes data to path through a temporary file in the same
// directory, so a reader never sees a partial file.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".hazard-select-*.tmp")
	if err != nil {
		return err
	}
	fail := func(cause error) error {
		return errors.Join(cause, tmp.Close(), os.Remove(tmp.Name()))
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		return errors.Join(err, os.Remove(tmp.Name()))
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return errors.Join(err, os.Remove(tmp.Name()))
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return errors.Join(err, os.Remove(tmp.Name()))
	}
	return nil
}
