package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/omorsi45/hazard/internal/runner"
	"github.com/omorsi45/hazard/internal/telemetry"
)

const auditLong = `Compare the selections recorded in telemetry with a full run of the suite.

A selection made at the full run's commit is compared directly: every test that
failed in the full run and was not selected is a field miss. A failure no
selection at that commit explains is reported as unattributed, unless --bisect
finds the commit where the test first fails (running it alone in a temporary
git worktree at each step) and a selection made there left it out.

Field misses are written to .hazard/hazard.db and one line per audited
selection to .hazard/audits.jsonl.`

func newAuditCommand(rn runners) *cobra.Command {
	cmd := &cobra.Command{
		Use: "audit", Short: "Audit a selection against a full run", Long: auditLong,
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 0 {
				return usageError{errors.New("audit accepts no positional arguments")}
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.String("full-run", "", "full-run JUnit path")
	f.String("since-run", "", "selection ID or timestamp")
	f.Bool("bisect", false, "bisect suspected misses")
	f.String("commit", "", "commit the full run tested (default HEAD)")
	f.String("config", "", "config path (default: hazard.yml, then .hazard/hazard.yml)")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error { return auditCommand(cmd, rn) }
	return cmd
}

func auditCommand(cmd *cobra.Command, rn runners) error {
	f := cmd.Flags()
	fullRun, _ := f.GetString("full-run")
	sinceArg, _ := f.GetString("since-run")
	bisect, _ := f.GetBool("bisect")
	commitArg, _ := f.GetString("commit")
	configPath, _ := f.GetString("config")
	jsonMode, _ := f.GetBool("json")
	switch {
	case fullRun == "":
		return usageError{errors.New("--full-run is required: the JUnit report of a full run")}
	case sinceArg == "":
		return usageError{errors.New("--since-run is required: a selection ID or an RFC 3339 time")}
	}
	since, err := telemetry.ParseSince(sinceArg)
	if err != nil {
		return usageError{err}
	}
	ctx := cmd.Context()
	if err := ctx.Err(); err != nil {
		return err
	}
	root, err := repoRoot(ctx)
	if err != nil {
		return err
	}
	cfg, err := loadIndexConfig(root, configPath)
	if err != nil {
		return usageError{err}
	}

	data, err := os.ReadFile(fullRun)
	if err != nil {
		return usageError{fmt.Errorf("--full-run: %w", err)}
	}
	cases, err := runner.ParseJUnit(bytes.NewReader(data))
	if err != nil {
		return usageError{fmt.Errorf("--full-run %s is not a usable JUnit report: %w", fullRun, err)}
	}
	git := telemetry.Git{Dir: root}
	if commitArg == "" {
		commitArg = "HEAD"
	}
	commit, err := git.Resolve(ctx, commitArg)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return usageError{fmt.Errorf("--commit: %w", err)}
	}

	path := cfg.Telemetry.Path
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, filepath.FromSlash(path))
	}
	records, skipped, err := telemetry.Load(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	var warnings []string
	if skipped > 0 {
		warnings = append(warnings, fmt.Sprintf("telemetry %s ends with a truncated line; it was skipped", path))
	}
	for _, c := range cases {
		if c.SuiteIncomplete {
			warnings = append(warnings, "the full-run report is incomplete (a suite does not match its own counts): failures may be missing")
			break
		}
	}

	known := map[string]bool{}
	for _, r := range records {
		for _, m := range r.Members {
			known[m.TestID] = true
		}
	}
	failing, unidentified := telemetry.FailingTests(cases, known)
	sum := sha256.Sum256(data)
	in := telemetry.AuditInput{
		Records: records, Since: since, FullRunID: "junit-" + hex.EncodeToString(sum[:8]), Commit: commit,
		Failing: failing, Unidentified: unidentified, Bisect: bisect, Now: time.Now(),
	}
	deps := telemetry.AuditDeps{IsAncestor: git.IsAncestor, Range: git.Range}
	if bisect {
		prober := telemetry.Prober{Git: git, Config: cfg, Runners: telemetry.Runners{Go: rn.Go, Python: rn.Python}}
		deps.Probe = prober.Probe
	}
	rep, err := telemetry.Audit(ctx, in, deps)
	switch {
	case errors.Is(err, telemetry.ErrSinceNotFound):
		return usageError{err}
	case err != nil:
		return err
	}
	rep.Warnings = append(warnings, rep.Warnings...)

	if err := telemetry.Persist(filepath.Join(root, ".hazard", "hazard.db"), filepath.Join(root, ".hazard", "audits.jsonl"), rep); err != nil {
		return err
	}
	for _, w := range rep.Warnings {
		if _, err := fmt.Fprintln(cmd.ErrOrStderr(), "warning:", w); err != nil {
			return err
		}
	}
	if jsonMode {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(rep)
	}
	_, err = fmt.Fprint(cmd.OutOrStdout(), renderAudit(rep))
	return err
}

func renderAudit(rep telemetry.Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "audit of %s at %s: %d failing test(s)\n", rep.FullRunID, rep.Commit, len(rep.Failing))
	fmt.Fprintf(&b, "audited selections: %d\n", rep.Audited)
	fmt.Fprintf(&b, "regression-bearing: %d\n", rep.RegressionBearing)
	fmt.Fprintf(&b, "misses: %d\n", rep.Misses)
	fmt.Fprintf(&b, "attributed: %d\n", rep.Attributed)
	fmt.Fprintf(&b, "unattributed: %d\n", rep.Unattributed)
	for _, m := range rep.MissList {
		fmt.Fprintf(&b, "miss %s selection %s window %s\n", m.TestID, m.SelectionID, m.BlameWindow)
	}
	for _, u := range rep.UnattributedTests {
		fmt.Fprintf(&b, "unattributed %s: %s\n", u.TestID, u.Reason)
	}
	return b.String()
}
