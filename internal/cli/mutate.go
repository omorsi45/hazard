package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/omorsi45/hazard/internal/config"
	"github.com/omorsi45/hazard/internal/mutate"
	"github.com/spf13/cobra"
)

type mutateRunFunc func(ctx context.Context, opts mutate.Options) (mutate.Summary, error)

const mutateLong = `Generate mutation labels: apply small source changes (mutants) to the files at
--base and record which tests fail for each.

Side effects: mutate builds and runs the test suite many times in temporary copies
of the repository at --base. It first runs the whole suite twice and refuses to
continue unless both runs pass. Every mutant is then built and tested in a copy;
the working tree is never modified. Only --out and the database
.hazard/hazard.db in the repository root are written. A relative --out is
resolved against the repository root.

Operators: cmp, logic, const, ret, stmt, cfg. Expect a long run: the time is
about the number of mutants times the time of one test run, divided by
--concurrency.`

func newMutateCommand(run mutateRunFunc) *cobra.Command {
	var (
		base, files, operators, out, packages, configPath string
		n, concurrency                                    int
		timeout                                           time.Duration
	)
	cmd := &cobra.Command{
		Use: "mutate", Short: "Generate mutation labels (builds and runs the test suite many times)", Long: mutateLong,
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 0 {
				return usageError{errors.New("mutate accepts no positional arguments")}
			}
			return nil
		},
	}
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		ops, err := mutate.ParseOperators(operators)
		if err != nil {
			return usageError{err}
		}
		globs, err := parseList("--files", files)
		if err != nil {
			return usageError{err}
		}
		switch {
		case n < 0:
			return usageError{errors.New("--n must not be negative (0 means all mutants)")}
		case timeout <= 0:
			return usageError{errors.New("--timeout must be positive")}
		case concurrency < 1:
			return usageError{errors.New("--concurrency must be at least 1")}
		}
		cfg, err := config.Load(configPath)
		if err != nil {
			return usageError{err}
		}
		pkgs := cfg.Languages.Go.Packages
		if cmd.Flags().Changed("packages") {
			if pkgs, err = parseList("--packages", packages); err != nil {
				return usageError{err}
			}
		}
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		ctx := cmd.Context()
		repoDir, err := mutate.TopLevel(ctx, cwd)
		if err != nil {
			return err
		}
		if !filepath.IsAbs(out) {
			out = filepath.Join(repoDir, out)
		}
		opts := mutate.Options{
			RepoDir: repoDir, Base: base, Files: globs, N: n, Operators: ops,
			Timeout: timeout, Concurrency: concurrency, Packages: pkgs, Tags: cfg.Languages.Go.Tags,
			MaxHops: cfg.Impact.MaxHops, Out: out, DBPath: filepath.Join(repoDir, ".hazard", "hazard.db"),
		}
		jsonMode, _ := cmd.Flags().GetBool("json")
		if verbose, _ := cmd.Flags().GetCount("verbose"); verbose > 0 && !jsonMode {
			opts.Log = cmd.ErrOrStderr()
		}
		sum, err := run(ctx, opts)
		if err != nil {
			return err
		}
		if jsonMode {
			sum.Records = nil
			return json.NewEncoder(cmd.OutOrStdout()).Encode(sum)
		}
		_, err = fmt.Fprint(cmd.OutOrStdout(), mutateText(sum, opts))
		return err
	}
	f := cmd.Flags()
	f.StringVar(&base, "base", "HEAD", "base revision")
	f.StringVar(&files, "files", "internal/**/*.go", "file globs, comma separated")
	f.IntVar(&n, "n", 500, "mutation count (0 means all)")
	f.StringVar(&operators, "operators", "cmp,logic,const,ret,stmt,cfg", "mutation operators, comma separated")
	f.DurationVar(&timeout, "timeout", 5*time.Minute, "per-mutation timeout")
	f.IntVar(&concurrency, "concurrency", 4, "mutation concurrency")
	f.StringVar(&out, "out", ".hazard/mutants.jsonl", "output path")
	f.StringVar(&packages, "packages", "", "go package patterns, comma separated (default: languages.go.packages from the config)")
	f.StringVar(&configPath, "config", "", "config path (default: hazard.yml, then .hazard/hazard.yml)")
	return cmd
}

// parseList splits a comma separated flag value. Whitespace around an element
// is dropped; an empty element, which includes an empty value, is an error.
func parseList(flag, value string) ([]string, error) {
	var items []string
	for _, part := range strings.Split(value, ",") {
		item := strings.TrimSpace(part)
		if item == "" {
			return nil, fmt.Errorf("%s needs at least one entry and no empty entries, got %q", flag, value)
		}
		items = append(items, item)
	}
	return items, nil
}

// mutateText renders every count of the summary.
func mutateText(s mutate.Summary, opts mutate.Options) string {
	var b strings.Builder
	fmt.Fprintf(&b, "mutate: base=%s candidates=%d executed=%d usable=%d baseline_seconds=%.1f\n",
		s.BaseSHA, s.Candidates, s.Executed, s.Usable, s.BaselineSeconds)
	fmt.Fprintf(&b, "status: build_error=%d killed=%d survived=%d timeout=%d unconfirmed=%d\n",
		s.ByStatus["build_error"], s.ByStatus["killed"], s.ByStatus["survived"], s.ByStatus["timeout"], s.ByStatus["unconfirmed"])
	var perOperator []string
	for _, op := range slices.Sorted(maps.Keys(s.ByOperator)) {
		perOperator = append(perOperator, fmt.Sprintf("%s=%d", op, s.ByOperator[op]))
	}
	fmt.Fprintf(&b, "operators: %s\n", strings.Join(perOperator, " "))
	fmt.Fprintf(&b, "reachability: unreachable_mutants=%d fully_unreachable_mutants=%d beyond_max_hops_mutants=%d\n",
		s.UnreachableMutants, s.FullyUnreachableMutants, s.BeyondMaxHopsMutants)
	if opts.Out != "" {
		fmt.Fprintf(&b, "output: %s\n", opts.Out)
	}
	if opts.DBPath != "" {
		fmt.Fprintf(&b, "database: %s\n", opts.DBPath)
	}
	return b.String()
}
