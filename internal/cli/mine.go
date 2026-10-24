package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/omorsi45/hazard/internal/mine"
	"github.com/spf13/cobra"
)

var (
	mineTokenEnvPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	mineProjectPattern  = regexp.MustCompile(`^[A-Za-z0-9_.-]+(/[A-Za-z0-9_.-]+)*$`)
)

func newMineCommand(run func(context.Context, mine.Request, mine.Options) (mine.Stats, error)) *cobra.Command {
	req := mine.Request{DBPath: ".hazard/hazard.db", RawDir: ".hazard/raw"}
	var since, until, apiBase string
	cmd := &cobra.Command{
		Use: "ci-mine", Short: "Mine CI history",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 0 {
				return usageError{errors.New("ci-mine accepts no positional arguments")}
			}
			return nil
		},
	}
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if req.Provider != "github" && req.Provider != "gitlab" {
			return usageError{errors.New("--provider must be github or gitlab")}
		}
		if !validMineProject(req.Provider, req.Project) {
			return usageError{errors.New("--project must be a provider project identifier")}
		}
		if req.Language != "" && req.Language != "go" && req.Language != "python" {
			return usageError{errors.New("--language must be go or python, or omitted for automatic mapping")}
		}
		if req.Provider == "github" && cmd.Flags().Changed("url") {
			return usageError{errors.New("--url is supported only for GitLab")}
		}
		if !mineTokenEnvPattern.MatchString(req.TokenEnv) {
			return usageError{errors.New("--token-env must be a portable environment variable name")}
		}
		if !validMineGlob(req.JUnitGlob) {
			return usageError{errors.New("--junit-glob must be a valid relative forward-slash glob")}
		}
		if req.MaxPipelines <= 0 || req.Concurrency <= 0 {
			return usageError{errors.New("--max-pipelines and --concurrency must be positive")}
		}
		var err error
		req.Since, err = time.Parse("2006-01-02", since)
		if err != nil || req.Since.IsZero() {
			return usageError{errors.New("--since must be YYYY-MM-DD in UTC")}
		}
		req.Until, err = time.Parse("2006-01-02", until)
		if err != nil {
			return usageError{errors.New("--until must be YYYY-MM-DD in UTC")}
		}
		req.Until = req.Until.AddDate(0, 0, 1)
		if !req.Since.Before(req.Until) || req.Until.Year() > 9999 {
			return usageError{errors.New("mining bounds must increase and remain within supported UTC dates")}
		}
		stats, runErr := run(cmd.Context(), req, mine.Options{APIBase: apiBase, Token: os.Getenv(req.TokenEnv)})
		jsonMode, _ := cmd.Flags().GetBool("json")
		var outputErr error
		if jsonMode {
			outputErr = json.NewEncoder(cmd.OutOrStdout()).Encode(stats)
		} else {
			_, outputErr = fmt.Fprintf(cmd.OutOrStdout(), "stored: runs=%d attempts=%d artifacts=%d raw_cases=%d factual_results=%d factual_result_bearing_commits=%d replay_duplicates=%d\nactivity: runs=%d attempts=%d persisted_artifacts=%d new_raw_cases=%d replay_cases=%d net_factual_results=%d incomplete_runs=%d capped_runs=%d\ncoverage: incomplete_units=%d capped_units=%d active_gaps=%d resolved_gaps=%d\n",
				stats.InspectedRuns, stats.InspectedAttempts, stats.InspectedArtifacts, stats.RawCases, stats.FactualResults, stats.FactualResultBearingCommits, stats.ReplayDuplicates,
				stats.Activity.InspectedRuns, stats.Activity.InspectedAttempts, stats.Activity.PersistedArtifacts, stats.Activity.NewRawCases, stats.Activity.ReplayCases, stats.Activity.NetFactualResults, stats.Activity.IncompleteRuns, stats.Activity.CappedRuns,
				stats.IncompleteUnits, stats.CappedUnits, stats.ActiveGaps, stats.ResolvedGaps)
		}
		verbose, _ := cmd.Flags().GetCount("verbose")
		if verbose > 0 && !jsonMode {
			_, detailErr := fmt.Fprintln(cmd.ErrOrStderr(), "ci-mine:", stats)
			outputErr = errors.Join(outputErr, detailErr)
		}
		return errors.Join(runErr, outputErr)
	}
	f := cmd.Flags()
	f.StringVar(&req.Provider, "provider", "", "gitlab|github (required)")
	f.StringVar(&req.Project, "project", "", "project identifier (required)")
	f.StringVar(&since, "since", "", "YYYY-MM-DD UTC start, inclusive (required)")
	f.StringVar(&until, "until", "", "YYYY-MM-DD UTC end date, next midnight exclusive (required)")
	f.StringVar(&req.Branch, "branch", "main", "branch")
	f.StringVar(&req.TokenEnv, "token-env", "HAZARD_CI_TOKEN", "token environment variable")
	f.StringVar(&req.JUnitGlob, "junit-glob", "**/junit*.xml", "JUnit artifact glob")
	f.IntVar(&req.MaxPipelines, "max-pipelines", 5000, "pipeline limit")
	f.IntVar(&req.Concurrency, "concurrency", 8, "download concurrency")
	f.BoolVar(&req.Resume, "resume", false, "resume prior mining")
	f.StringVar(&req.Language, "language", "", "go|python (default: automatic)")
	f.StringVar(&apiBase, "url", "", "explicit GitLab API base URL")
	return cmd
}

func validMineProject(provider, project string) bool {
	if len(project) > 512 || !mineProjectPattern.MatchString(project) {
		return false
	}
	parts := strings.Split(project, "/")
	for _, part := range parts {
		if part == "." || part == ".." || part == "..." {
			return false
		}
	}
	if provider == "github" {
		return len(parts) == 2
	}
	if len(parts) > 1 {
		return true
	}
	id, err := strconv.ParseInt(project, 10, 64)
	return err == nil && id > 0
}

func validMineGlob(glob string) bool {
	if !utf8.ValidString(glob) || strings.Contains(glob, `\`) {
		return false
	}
	for _, ch := range glob {
		if ch < 0x20 || ch == 0x7f {
			return false
		}
	}
	parts := strings.Split(glob, "/")
	if strings.Contains(parts[0], ":") {
		return false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return false
		}
		if _, err := path.Match(part, ""); err != nil {
			return false
		}
	}
	return true
}
