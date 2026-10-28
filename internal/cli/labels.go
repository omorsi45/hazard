package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/omorsi45/hazard/internal/labels"
	"github.com/omorsi45/hazard/internal/store"
	"github.com/spf13/cobra"
)

// labelsDBPath is the fixed database location, relative to the working
// directory, shared with ci-mine.
const labelsDBPath = ".hazard/hazard.db"

// labelsBackend is the seam between the commands and the labels engine.
type labelsBackend struct {
	build func(ctx context.Context, dbPath string, opts labels.Options) (labels.Report, error)
	stats func(ctx context.Context, dbPath string) (labels.Report, error)
}

// newLabelsBackend is replaced by tests to avoid touching a real database.
var newLabelsBackend = defaultLabelsBackend

func defaultLabelsBackend() labelsBackend {
	return labelsBackend{
		build: func(ctx context.Context, dbPath string, opts labels.Options) (report labels.Report, err error) {
			s, err := store.Open(dbPath)
			if err != nil {
				return labels.Report{}, err
			}
			defer func() { err = errors.Join(err, s.Close()) }()
			return labels.Build(ctx, s.DB(), opts)
		},
		stats: func(ctx context.Context, dbPath string) (report labels.Report, err error) {
			if _, statErr := os.Stat(dbPath); statErr != nil {
				if errors.Is(statErr, fs.ErrNotExist) {
					return labels.Report{}, fmt.Errorf("no database at %s: run ci-mine and labels build first", dbPath)
				}
				return labels.Report{}, statErr
			}
			s, err := store.Open(dbPath)
			if err != nil {
				return labels.Report{}, err
			}
			defer func() { err = errors.Join(err, s.Close()) }()
			return labels.ReadStats(ctx, s.DB())
		},
	}
}

func newLabelsCommand(be labelsBackend) *cobra.Command {
	cmd := &cobra.Command{Use: "labels", Short: "Build and inspect regression labels"}
	cmd.AddCommand(newLabelsBuildCommand(be), newLabelsFlakyCommand(be), newLabelsStatsCommand(be))
	return cmd
}

func noLabelsArgs(cmd *cobra.Command, args []string) error {
	if len(args) != 0 {
		return usageError{fmt.Errorf("%s accepts no positional arguments", cmd.CommandPath())}
	}
	return nil
}

// parseLabelsCutoff accepts exactly YYYY-MM-DD and returns UTC midnight, the
// exclusive cutoff on source observation timestamps.
func parseLabelsCutoff(value string) (time.Time, error) {
	parsed, err := time.Parse("2006-01-02", value)
	if err != nil || parsed.Format("2006-01-02") != value {
		return time.Time{}, usageError{errors.New("--before must be YYYY-MM-DD in UTC")}
	}
	return parsed, nil
}

// newLabelsCutoffCommand builds build and flaky: both run the same engine and
// differ only in what they print.
func newLabelsCutoffCommand(be labelsBackend, name, purpose string, print func(io.Writer, labels.Report) error) *cobra.Command {
	var before string
	cmd := &cobra.Command{Use: name, Short: purpose, Args: noLabelsArgs}
	cmd.Flags().StringVar(&before, "before", "", "YYYY-MM-DD UTC cutoff on source observation times, exclusive (required)")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		cutoff, err := parseLabelsCutoff(before)
		if err != nil {
			return err
		}
		report, err := be.build(cmd.Context(), labelsDBPath, labels.Options{Before: cutoff})
		if err != nil {
			return err
		}
		return writeLabelsReport(cmd, report, print)
	}
	return cmd
}

func newLabelsBuildCommand(be labelsBackend) *cobra.Command {
	return newLabelsCutoffCommand(be, "build", "Build regression labels", func(w io.Writer, r labels.Report) error {
		return printLabelsReport(w, "labels build", r)
	})
}

func newLabelsFlakyCommand(be labelsBackend) *cobra.Command {
	return newLabelsCutoffCommand(be, "flaky", "Classify flaky results", printFlakyReport)
}

func newLabelsStatsCommand(be labelsBackend) *cobra.Command {
	return &cobra.Command{
		Use: "stats", Short: "Report label yield", Args: noLabelsArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			report, err := be.stats(cmd.Context(), labelsDBPath)
			if err != nil {
				return err
			}
			return writeLabelsReport(cmd, report, func(w io.Writer, r labels.Report) error {
				return printLabelsReport(w, "labels stats", r)
			})
		},
	}
}

func writeLabelsReport(cmd *cobra.Command, report labels.Report, print func(io.Writer, labels.Report) error) error {
	if jsonMode, _ := cmd.Flags().GetBool("json"); jsonMode {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(report)
	}
	return print(cmd.OutOrStdout(), report)
}

// lineWriter keeps the first write error so output code stays linear.
type lineWriter struct {
	w   io.Writer
	err error
}

func (l *lineWriter) printf(format string, args ...any) {
	if l.err == nil {
		_, l.err = fmt.Fprintf(l.w, format, args...)
	}
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func formatCounts(counts map[string]int) string {
	if len(counts) == 0 {
		return "none"
	}
	names := make([]string, 0, len(counts))
	for name := range counts {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("%s=%d", name, counts[name]))
	}
	return strings.Join(parts, " ")
}

func printIdentity(l *lineWriter, title string, r labels.Report) {
	l.printf("%s: build_id=%s freshness=%s\n", title, orNone(r.BuildID), r.Freshness)
	l.printf("cutoff: %s\n", orNone(r.Cutoff))
	l.printf("algorithm_version: %s\n", orNone(r.AlgorithmVersion))
	l.printf("input_digest: %s\n", orNone(r.InputDigest))
	l.printf("decision_digest: %s\n", orNone(r.DecisionDigest))
}

func printAlwaysRun(l *lineWriter, r labels.Report) {
	l.printf("always_run_tests (%d):\n", len(r.AlwaysRunTests))
	for _, id := range r.AlwaysRunTests {
		l.printf("  %s\n", id)
	}
}

func printLimitations(l *lineWriter, r labels.Report) {
	l.printf("limitations:\n")
	for _, text := range r.Limitations {
		l.printf("  - %s\n", text)
	}
}

func printLabelsReport(w io.Writer, title string, r labels.Report) error {
	l := &lineWriter{w: w}
	printIdentity(l, title, r)
	l.printf("raw_failing_cases=%d grouped_member_count=%d unresolved_raw_members=%d unresolved_pending=%d unresolved_excluded=%d\n",
		r.RawFailingCases, r.GroupedMemberCount, r.UnresolvedRawMembers, r.UnresolvedPending, r.UnresolvedExcluded)
	l.printf("raw_candidates=%d excluded_candidates=%d pending_candidates=%d weak_candidates=%d strong_candidates=%d\n",
		r.RawCandidates, r.ExcludedCandidates, r.PendingCandidates, r.WeakCandidates, r.StrongCandidates)
	l.printf("candidate_reason_counts (overlapping, not additive): %s\n", formatCounts(r.CandidateReasonCounts))
	l.printf("unresolved_reason_counts (overlapping, not additive): %s\n", formatCounts(r.UnresolvedReasonCounts))
	l.printf("strong_commits_by_repository: %s\n", formatCounts(r.StrongCommitsByRepository))
	printAlwaysRun(l, r)
	l.printf("acquisition_exclusions_by_reason: %s\n", formatCounts(r.AcquisitionExclusionsByReason))
	l.printf("active_gaps=%d\n", r.ActiveGaps)
	printLimitations(l, r)
	return l.err
}

// printFlakyReport prints the flaky-hygiene subset of the shared build.
func printFlakyReport(w io.Writer, r labels.Report) error {
	l := &lineWriter{w: w}
	printIdentity(l, "labels flaky", r)
	for _, reason := range []string{"flaky_same_commit", "unstable_identity", "quarantined"} {
		l.printf("%s: candidates=%d unresolved=%d\n", reason, r.CandidateReasonCounts[reason], r.UnresolvedReasonCounts[reason])
	}
	l.printf("raw_candidates=%d excluded_candidates=%d (exclusion never removes a raw candidate)\n", r.RawCandidates, r.ExcludedCandidates)
	printAlwaysRun(l, r)
	printLimitations(l, r)
	return l.err
}
