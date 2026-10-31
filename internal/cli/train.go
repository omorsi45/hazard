package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/omorsi45/hazard/internal/events"
	"github.com/omorsi45/hazard/internal/predict"
	"github.com/spf13/cobra"
)

const trainLong = `Fit the static (P0) and logistic (P1) predictors from the mutation records and
the strong and weak CI regression labels in .hazard/hazard.db, and write
static.json, logistic.json and training.json to --out (default .hazard).

--before YYYY-MM-DD is the exclusive training cutoff: an event at exactly 00:00:00
UTC of that date is excluded. Only the train partition is read (a hash of the
mutated file, or of the commit, assigns every event to train, tune or test);
tune and test events are evaluation data and are only counted. The oldest 80%
of the training groups fit the models and the newest 20% scale the logistic
output (Platt scaling).

Mutants are replayed on the repository index, which must describe the commit
the mutants were made at (run hazard index with it checked out). Each CI label
builds an index at the parent of its commit in a scratch checkout, which runs
go list and, when Python is enabled, pytest collection.

Side effects: temporary clones of the repository (removed afterwards) and the
files above. The repository itself is only read. A fit whose training set lacks
positive or negative examples, or that does not converge, is refused with its
reason; the other fit is still written. Refusal is a result, not an error.

Exit codes: 0 both artifacts written; 1 a fit or the whole training was
refused; 2 usage or configuration error; 70 internal error.`

func newTrainCommand() *cobra.Command {
	var before, out, configPath string
	cmd := &cobra.Command{
		Use: "train", Short: "Fit the static and logistic predictors (replays training events through the engine)", Long: trainLong,
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 0 {
				return usageError{errors.New("train accepts no positional arguments")}
			}
			return nil
		},
	}
	cmd.RunE = func(cmd *cobra.Command, _ []string) error { return runTrain(cmd, before, out, configPath) }
	f := cmd.Flags()
	f.StringVar(&before, "before", "", "YYYY-MM-DD UTC cutoff, exclusive (required)")
	f.StringVar(&out, "out", ".hazard", "output directory (relative to the repository root)")
	f.StringVar(&configPath, "config", "", "config path (default: hazard.yml, then .hazard/hazard.yml)")
	return cmd
}

func runTrain(cmd *cobra.Command, before, out, configPath string) error {
	cutoff, err := parseLabelsCutoff(before)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	repoDir, err := repoRoot(ctx)
	if err != nil {
		return err
	}
	cfg, err := loadIndexConfig(repoDir, configPath)
	if err != nil {
		return usageError{err}
	}
	engineConfig := ""
	if configPath != "" {
		if engineConfig, err = filepath.Abs(configPath); err != nil {
			return usageError{err}
		}
	}
	if !filepath.IsAbs(out) {
		out = filepath.Join(repoDir, out)
	}
	jsonMode, _ := cmd.Flags().GetBool("json")
	options := predict.DefaultLogisticOptions()
	collect := events.CollectOptions{
		RepoDir: repoDir, DBPath: filepath.Join(repoDir, ".hazard", "hazard.db"), ConfigPath: engineConfig, Config: cfg,
		Cutoff: cutoff, CIWeight: options.CIWeight,
	}
	if verbose, _ := cmd.Flags().GetCount("verbose"); verbose > 0 && !jsonMode {
		collect.Log = cmd.ErrOrStderr()
	}
	corpus, err := events.Collect(ctx, collect)
	var refusal *events.RefusalError
	if errors.As(err, &refusal) {
		if jsonMode {
			if err := json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]string{"refused": refusal.Reason}); err != nil {
				return err
			}
		} else if _, err := fmt.Fprintf(cmd.OutOrStdout(), "train: refused: %s\n", refusal.Reason); err != nil {
			return err
		}
		return exitError{code: 1, msg: "refused: " + refusal.Reason}
	}
	if err != nil {
		return err
	}
	manifest, err := predict.Train(predict.TrainInput{
		Cutoff: before, Events: corpus.Events, Collected: corpus.Collected, Options: options, OutDir: out,
	})
	if err != nil {
		return err
	}
	if jsonMode {
		if err := json.NewEncoder(cmd.OutOrStdout()).Encode(manifest); err != nil {
			return err
		}
	} else if err := writeTrainText(cmd.OutOrStdout(), out, manifest); err != nil {
		return err
	}
	if len(manifest.Refusals) > 0 {
		names := make([]string, len(manifest.Refusals))
		for i, r := range manifest.Refusals {
			names[i] = r.Predictor
		}
		return exitError{code: 1, msg: "refused: " + strings.Join(names, " and ") + " (reasons above)"}
	}
	return nil
}

// writeTrainText prints every count of the training manifest and the outcome
// of each fit.
func writeTrainText(w io.Writer, out string, m predict.TrainingManifest) error {
	lw := &lineWriter{w: w}
	lw.printf("cutoff: %s (exclusive, UTC midnight)\n", m.Cutoff)
	for _, c := range m.Collected.Events {
		lw.printf("observed: %s/%s/%s=%d\n", c.Origin, c.Partition, c.Status, c.Count)
	}
	lw.printf("skipped: %s\n", formatCounts(m.Collected.Skipped))
	lw.printf("failing tests not in rows: certain=%d unlisted=%d\n", m.Collected.FailingCertain, m.Collected.FailingUnlisted)
	lw.printf("examples: events=%d examples=%d positives=%d negatives=%d\n", m.Examples.Events, m.Examples.Examples, m.Examples.Positives, m.Examples.Negatives)
	for _, name := range []string{"fit", "scale"} {
		s := m.Splits[name]
		lw.printf("split %s: groups=%d events=%d examples=%d positives=%d negatives=%d\n", name, len(s.Groups), s.Events, s.Examples, s.Positives, s.Negatives)
	}
	for _, name := range []string{"static", "logistic"} {
		if a, ok := m.Artifacts[name]; ok {
			lw.printf("%s: wrote %s digest=%s\n", name, a.Path, a.Digest)
			continue
		}
		for _, r := range m.Refusals {
			if r.Predictor == name {
				lw.printf("%s: refused: %s\n", name, r.Reason)
			}
		}
	}
	if len(m.Artifacts) > 0 {
		lw.printf("training: %s\n", filepath.Join(out, "training.json"))
	} else {
		lw.printf("training: nothing written\n")
	}
	return lw.err
}
