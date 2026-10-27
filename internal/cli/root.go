package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"

	"github.com/spf13/pflag"

	"github.com/spf13/cobra"

	"github.com/omorsi45/hazard/internal/engine"
	"github.com/omorsi45/hazard/internal/index"
	"github.com/omorsi45/hazard/internal/mine"
)

const version = "0.1.0-dev"

var errNotImplemented = errors.New("not implemented")

type usageError struct{ cause error }

func (e usageError) Error() string { return e.cause.Error() }
func (e usageError) Unwrap() error { return e.cause }

// NewRoot returns the command tree. Milestone-specific leaves are explicit stubs.
func NewRoot() *cobra.Command {
	root := &cobra.Command{Use: "hazard", Short: "Conservative test selection", SilenceUsage: true, SilenceErrors: true, Version: version}
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError{err} })
	root.PersistentFlags().Bool("json", false, "emit machine-readable output")
	root.PersistentFlags().CountP("verbose", "v", "increase human output verbosity")

	leaf := func(name, purpose string) *cobra.Command {
		return &cobra.Command{
			Use: name, Short: purpose + " (not implemented yet)",
			RunE: func(cmd *cobra.Command, _ []string) error {
				if cmd.Name() == "select" || cmd.Name() == "run" {
					if cmd.Flags().Changed("recall") {
						recall, _ := cmd.Flags().GetFloat64("recall")
						confirmed, _ := cmd.Flags().GetBool("i-know-what-im-doing")
						if !confirmed || math.IsNaN(recall) || math.IsInf(recall, 0) || recall <= 0 || recall >= 1 {
							return usageError{fmt.Errorf("invalid --recall override: requires --i-know-what-im-doing and a value in (0,1)")}
						}
					}
				}
				if err := cmd.Context().Err(); err != nil {
					return err
				}
				return fmt.Errorf("%s: %w", cmd.CommandPath(), errNotImplemented)
			},
		}
	}

	root.AddCommand(newSelectCommand(engine.Select))

	run := leaf("run", "Select and run tests")
	selectFlags(run)
	run.Flags().String("runner", "auto", "auto|gotest|pytest")
	run.Flags().String("junit", ".hazard/junit.xml", "JUnit output path")
	run.Flags().Bool("full", false, "force full suite")
	run.Flags().Bool("no-fallback", false, "return internal error without full-suite fallback")
	run.Args = cobra.ArbitraryArgs
	root.AddCommand(run)

	root.AddCommand(newMineCommand(mine.Mine))
	root.AddCommand(newIndexCommand(index.Build, index.Publish))

	labels := &cobra.Command{Use: "labels", Short: "Build and inspect regression labels"}
	for _, item := range []struct{ name, purpose string }{
		{"build", "Build regression labels"},
		{"flaky", "Classify flaky results"},
		{"stats", "Report label yield"},
	} {
		labels.AddCommand(leaf(item.name, item.purpose))
	}
	root.AddCommand(labels)

	mutate := leaf("mutate", "Generate mutation labels")
	mutate.Flags().String("base", "HEAD", "base revision")
	mutate.Flags().String("files", "internal/**/*.go", "file glob")
	mutate.Flags().Int("n", 500, "mutation count")
	mutate.Flags().String("operators", "cmp,logic,const,ret,stmt,cfg", "mutation operators")
	mutate.Flags().Duration("timeout", 300000000000, "per-mutation timeout")
	mutate.Flags().Int("concurrency", 4, "mutation concurrency")
	mutate.Flags().String("out", ".hazard/mutants.jsonl", "output path")
	root.AddCommand(mutate)

	calibrate := leaf("calibrate", "Calibrate selection risk")
	calibrate.Flags().Float64("alpha", 0.01, "target miss rate")
	calibrate.Flags().Float64("delta", 0.05, "error probability")
	calibrate.Flags().String("split", "temporal", "split method")
	calibrate.Flags().String("split-at", "", "temporal split point")
	calibrate.Flags().String("predictor", "ensemble", "predictor")
	calibrate.Flags().String("grid", "0.005:0.500:0.005", "threshold grid")
	calibrate.Flags().String("unit", "commit", "calibration unit")
	calibrate.Flags().String("out", "hazard.lock", "lock output path")
	root.AddCommand(calibrate)

	eval := leaf("eval", "Evaluate selectors")
	eval.Flags().String("split", "temporal", "split method")
	eval.Flags().String("split-at", "", "temporal split point")
	eval.Flags().String("predictors", "static,logistic,jev,ensemble", "predictors")
	eval.Flags().String("baselines", "all,l1-only,l1-coverage", "baselines")
	eval.Flags().String("out", ".hazard/report/", "report directory")
	root.AddCommand(eval)

	audit := leaf("audit", "Audit a selection against a full run")
	audit.Flags().String("full-run", "", "full-run JUnit path")
	audit.Flags().String("since-run", "", "selection ID or timestamp")
	audit.Flags().Bool("bisect", false, "bisect suspected misses")
	root.AddCommand(audit)

	root.AddCommand(newExplainCommand(engine.Select))

	root.AddCommand(&cobra.Command{Use: "version", Short: "Print version", RunE: func(cmd *cobra.Command, _ []string) error {
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "hazard %s\n", version)
		return err
	}})
	return root
}

func selectFlags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.String("base", "", "base ref (default: merge-base with the default branch)")
	f.String("head", "", "head ref (default: working tree, including uncommitted changes)")
	f.String("config", "", "config path (default: hazard.yml, then .hazard/hazard.yml)")
	f.String("lock", "hazard.lock", "lock path")
	f.String("predictor", "", "static|logistic|jev|ensemble (default: from lock)")
	f.Float64("recall", 0, "override target recall")
	f.Bool("i-know-what-im-doing", false, "confirm recall override")
	f.Int("max-candidates", 400, "residual candidate budget")
	f.Int("budget-ms", 5000, "selection time budget")
	f.Float64("budget-usd", 0.01, "predictor spend budget")
	f.String("redact", "", "none|structure|hash (default: from config)")
	f.Bool("shadow", false, "print the full suite as the run set")
	f.Bool("dry-run-payload", false, "print predictor payload without sending")
	f.String("format", "text", "text|json|go-run|pytest-file")
	f.String("out", "", "selection output path")
	f.Bool("explain", false, "include per-test reasons")
}

func jsonRequested(args []string, cmd *cobra.Command) bool {
	valueFlags := map[string]bool{}
	collect := func(flag *pflag.Flag) {
		if flag.NoOptDefVal == "" {
			valueFlags[flag.Name] = true
		}
	}
	cmd.LocalFlags().VisitAll(collect)
	cmd.InheritedFlags().VisitAll(collect)
	requested := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		if arg == "--json" {
			requested = true
			continue
		}
		if strings.HasPrefix(arg, "--json=") {
			value := strings.TrimPrefix(arg, "--json=")
			parsed, err := strconv.ParseBool(value)
			requested = err != nil || parsed
			continue
		}
		if !strings.HasPrefix(arg, "--") {
			continue
		}
		name, _, hasValue := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
		if !hasValue && valueFlags[name] {
			i++
		}
	}
	return requested
}

// Execute maps command, usage and internal errors to stable process exit codes.
func Execute(ctx context.Context, args []string, out, errOut io.Writer) int {
	root := NewRoot()
	cmd, _, findErr := root.Find(args)
	jsonMode := jsonRequested(args, cmd)
	root.SetArgs(args)
	root.SetOut(out)
	root.SetErr(errOut)
	err := root.ExecuteContext(ctx)
	if err == nil {
		return 0
	}
	code := 70
	var usage usageError
	if errors.As(err, &usage) || findErr != nil {
		code = 2
	}
	if jsonMode {
		_ = json.NewEncoder(errOut).Encode(map[string]any{"error": err.Error(), "code": code})
	} else {
		_, _ = fmt.Fprintln(errOut, "error:", err)
	}
	return code
}
