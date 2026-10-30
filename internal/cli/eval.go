package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/omorsi45/hazard/internal/control"
	"github.com/omorsi45/hazard/internal/eval"
	"github.com/omorsi45/hazard/internal/events"
)

const evalLong = `Measure the selection configurations on held-out events and write the report
(seed 12.2) to --out: report.json, report.html (static, no script), pairs.csv
and reliability.csv.

Configurations: B0 runs everything, B1 is L1 on the graph alone, B2 is L1 with
coverage, P0 and P1 add the static and logistic predictors above lambda, P2 and
P3 add Jev and the ensemble. B1 and B2 are judged as they are and are marked
"below <recall>" when they miss the target. P0 and P1 go through the same
controller as hazard select, abstentions included; lambda is the largest value
of the grid 0.005:0.500:0.005 at which the per-commit recall on the tune
partition reaches --recall. When no value does, the row reads "infeasible at
R_c >= <recall>" and its test numbers are given at the smallest lambda. Lambda
is never chosen on the test partition and never lowered. Jev and the ensemble
need the API key named by predictor.jev.api_key_env; without it their rows read
"needs OPENROUTER_API_KEY" and no request is made.

Populations. The mutation population is synthetic (mutation labels): the mutants
of .hazard/hazard.db are replayed on the index, which must describe their base
commit, and split by a hash of the mutated file into train (never read), tune
(chooses lambda) and test (reported). The CI population is the strong units of
the current label build; with no strong unit every cell of its rows reads N/A
and the counts (raw failing cases, strong units) are shown. --split and
--split-at divide the strong units of that population only.

Time is priced with the per-test durations of --durations (default
.hazard/mutants.jsonl.baseline.json, written by hazard mutate); a test without
one costs the median of the known ones and the number of such tests is
reported. Without the file the time ratio and the effective saving read N/A.
The effective saving is 1 - (E[cost(S)] + E[overhead]) / E[cost(T)] over all
events, abstentions included. Every value has a 95% percentile bootstrap
interval over events (--bootstrap resamples, generator PCG(--seed, 0)).

Exit codes: 0 report written; 1 refused (no database, no index for the
mutation base) and nothing written; 2 usage or configuration error; 70 internal
error.`

func newEvalCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use: "eval", Short: "Evaluate the selection configurations (the ablation of seed 12.2)", Long: evalLong,
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 0 {
				return usageError{errors.New("eval accepts no positional arguments")}
			}
			return nil
		},
		RunE: runEvalCommand,
	}
	f := cmd.Flags()
	f.String("split", eval.SplitTemporal, "temporal|random: how the strong CI units divide into tune and test")
	f.String("split-at", "", "YYYY-MM-DD UTC: strong CI units before it are tune (temporal split only)")
	f.String("predictors", "static,logistic,jev,ensemble", "predictor rows, from static|logistic|jev|ensemble")
	f.String("baselines", "all,l1-only,l1-coverage", "baseline rows, from all|l1-only|l1-coverage")
	f.String("out", ".hazard/report/", "report directory (relative to the repository root)")
	f.String("source", eval.SourceBoth, "mutation|ci|both: the populations to measure")
	f.Float64("recall", 0.99, "per-commit recall lambda must reach on the tune partition, in (0,1]")
	f.Int("bootstrap", 2000, "bootstrap resamples of the events (at least 1)")
	f.Uint64("seed", 1, "seed of the bootstrap generator")
	f.String("durations", "", "baseline durations file (default: .hazard/mutants.jsonl.baseline.json)")
	f.String("config", "", "config path (default: hazard.yml, then .hazard/hazard.yml)")
	return cmd
}

// evalRequest is the validated command line.
type evalRequest struct {
	opts     eval.Options
	out      string
	jsonMode bool
	verbose  bool
}

// splitList splits a comma-separated list, dropping blanks and repeats, and
// checks every item against the allowed values.
func splitList(flag, value string, allowed ...string) ([]string, error) {
	var out []string
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		switch {
		case item == "":
		case !oneOf(item, allowed...):
			return nil, usageError{fmt.Errorf("invalid --%s item %q: want %s", flag, item, strings.Join(allowed, "|"))}
		case !slices.Contains(out, item):
			out = append(out, item)
		}
	}
	return out, nil
}

// parseEvalFlags reads and validates the flags; every violation is a usage
// error, reported before anything is read.
func parseEvalFlags(cmd *cobra.Command) (evalRequest, error) {
	f := cmd.Flags()
	var req evalRequest
	method, _ := f.GetString("split")
	splitAt, _ := f.GetString("split-at")
	predictors, _ := f.GetString("predictors")
	baselines, _ := f.GetString("baselines")
	req.out, _ = f.GetString("out")
	source, _ := f.GetString("source")
	req.opts.Recall, _ = f.GetFloat64("recall")
	req.opts.Bootstrap, _ = f.GetInt("bootstrap")
	req.opts.Seed, _ = f.GetUint64("seed")
	req.opts.Durations, _ = f.GetString("durations")
	req.jsonMode, _ = f.GetBool("json")
	verbose, _ := f.GetCount("verbose")
	req.verbose = verbose > 0

	switch {
	case !oneOf(source, eval.SourceMutation, eval.SourceCI, eval.SourceBoth):
		return req, usageError{fmt.Errorf("invalid --source %q: want mutation, ci or both", source)}
	case !(req.opts.Recall > 0 && req.opts.Recall <= 1):
		return req, usageError{fmt.Errorf("invalid --recall %v: want a value in (0,1]", req.opts.Recall)}
	case req.opts.Bootstrap < 1:
		return req, usageError{fmt.Errorf("invalid --bootstrap %d: want at least 1", req.opts.Bootstrap)}
	case !oneOf(method, eval.SplitTemporal, eval.SplitRandom):
		return req, usageError{fmt.Errorf("invalid --split %q: want temporal or random", method)}
	case method == eval.SplitRandom && splitAt != "":
		return req, usageError{errors.New("--split-at only applies to --split temporal")}
	case req.out == "":
		return req, usageError{errors.New("--out must not be empty")}
	}
	req.opts.Source = source
	req.opts.Split = eval.SplitSpec{Method: method}
	if splitAt != "" {
		at, err := time.Parse(eval.DateLayout, splitAt)
		if err != nil || at.Format(eval.DateLayout) != splitAt {
			return req, usageError{fmt.Errorf("invalid --split-at %q: want YYYY-MM-DD", splitAt)}
		}
		req.opts.Split.At = at
	}
	var err error
	if req.opts.Predictors, err = splitList("predictors", predictors, eval.PredictorStatic, eval.PredictorLogistic, eval.PredictorJev, eval.PredictorEnsemble); err != nil {
		return req, err
	}
	if req.opts.Baselines, err = splitList("baselines", baselines, eval.BaselineAll, eval.BaselineL1Only, eval.BaselineL1Coverage); err != nil {
		return req, err
	}
	req.opts.GridText = "0.005:0.500:0.005"
	if req.opts.Grid, err = control.ParseGrid(req.opts.GridText); err != nil {
		return req, err
	}
	return req, nil
}

func runEvalCommand(cmd *cobra.Command, _ []string) error {
	req, err := parseEvalFlags(cmd)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	if err := ctx.Err(); err != nil {
		return err
	}
	repoDir, err := repoRoot(ctx)
	if err != nil {
		return err
	}
	configPath, _ := cmd.Flags().GetString("config")
	cfg, err := loadIndexConfig(repoDir, configPath)
	if err != nil {
		return usageError{err}
	}
	opts := req.opts
	opts.RepoDir, opts.Config = repoDir, cfg
	opts.DBPath = filepath.Join(repoDir, ".hazard", "hazard.db")
	if configPath != "" {
		if opts.ConfigPath, err = filepath.Abs(configPath); err != nil {
			return usageError{err}
		}
	}
	if opts.Durations == "" {
		opts.Durations = filepath.Join(repoDir, ".hazard", "mutants.jsonl.baseline.json")
	} else {
		opts.DurationsOK = true
		if opts.Durations, err = filepath.Abs(opts.Durations); err != nil {
			return usageError{err}
		}
	}
	if req.verbose && !req.jsonMode {
		opts.Log = cmd.ErrOrStderr()
	}
	out := req.out
	if !filepath.IsAbs(out) {
		out = filepath.Join(repoDir, out)
	}

	result, err := eval.Run(ctx, opts)
	var refusal *events.RefusalError
	var usage *eval.UsageError
	switch {
	case errors.As(err, &refusal):
		return refuseEval(cmd, req.jsonMode, refusal.Reason)
	case errors.As(err, &usage):
		return usageError{usage}
	case err != nil:
		return err
	}
	if err := result.Write(out); err != nil {
		return err
	}
	if req.jsonMode {
		data, err := result.Report.JSON()
		if err != nil {
			return err
		}
		_, err = cmd.OutOrStdout().Write(data)
		return err
	}
	if err := result.Report.WriteText(cmd.OutOrStdout()); err != nil {
		return err
	}
	lw := &lineWriter{w: cmd.OutOrStdout()}
	lw.printf("\n")
	for _, name := range []string{result.Report.Files.Report, result.Report.Files.HTML, result.Report.Files.Pairs, result.Report.Files.Reliability} {
		if name != "" {
			lw.printf("wrote %s\n", filepath.ToSlash(filepath.Join(out, name)))
		}
	}
	return lw.err
}

// refuseEval prints the refusal and ends the command with exit 1: a refusal is
// a result, not an error, and nothing is written.
func refuseEval(cmd *cobra.Command, jsonMode bool, reason string) error {
	if jsonMode {
		if err := json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]string{"refused": reason}); err != nil {
			return err
		}
	} else if _, err := fmt.Fprintf(cmd.OutOrStdout(), "eval: refused: %s\n", reason); err != nil {
		return err
	}
	return exitError{code: 1, msg: "refused: " + reason}
}
