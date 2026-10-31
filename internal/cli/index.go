package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/omorsi45/hazard/internal/config"
	"github.com/omorsi45/hazard/internal/index"
	"github.com/spf13/cobra"
)

type indexBuildFunc func(ctx context.Context, repoDir string, cfg config.Config, opts index.Options) (*index.Manifest, error)
type indexPublishFunc func(repoDir string, m *index.Manifest) error

const indexLong = `Build the test index of the current git repository, always at its top level as
select does: the Go package and Python module graph, the test inventory and,
with --coverage, per-test coverage. The result is published atomically to
.hazard/index/manifest.json; a failed or canceled build never replaces the
previous index.

Side effects:
  - Test collection can execute test package initialization code (go list reads
    packages, and pytest imports test modules while collecting).
  - --coverage executes the test suite: a full baseline go test, then one go test
    per top-level Go test, and one pytest run under coverage.py.`

type indexCoverageSummary struct {
	Granularity string `json:"granularity"`
	Covered     int    `json:"covered"`
	Failed      int    `json:"failed"`
	NotRun      int    `json:"not_run"`
}

type indexSummary struct {
	Manifest    string                `json:"manifest"`
	HeadSHA     string                `json:"head_sha"`
	Dirty       bool                  `json:"dirty"`
	Tests       int                   `json:"tests"`
	Packages    int                   `json:"packages"`
	Complete    bool                  `json:"complete"`
	Coverage    *indexCoverageSummary `json:"coverage,omitempty"`
	Diagnostics []string              `json:"diagnostics"`
}

func newIndexCommand(build indexBuildFunc, publish indexPublishFunc) *cobra.Command {
	var configPath, python string
	var coverage bool
	cmd := &cobra.Command{
		Use: "index", Short: "Build the test index (collection and --coverage execute project code)", Long: indexLong,
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 0 {
				return usageError{errors.New("index accepts no positional arguments")}
			}
			return nil
		},
	}
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		ctx := cmd.Context()
		repoDir, err := repoRoot(ctx)
		if err != nil {
			return err
		}
		cfg, err := loadIndexConfig(repoDir, configPath)
		if err != nil {
			return usageError{err}
		}
		m, err := build(ctx, repoDir, cfg, index.Options{Coverage: coverage, Python: python})
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := publish(repoDir, m); err != nil {
			return err
		}
		summary := summarizeIndex(repoDir, m, coverage)
		if jsonMode, _ := cmd.Flags().GetBool("json"); jsonMode {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(summary)
		}
		return writeIndexText(cmd, summary, coverage)
	}
	f := cmd.Flags()
	f.BoolVar(&coverage, "coverage", false, "also collect per-test coverage (executes the test suite)")
	f.StringVar(&configPath, "config", "", "config path (default: hazard.yml, then .hazard/hazard.yml)")
	f.StringVar(&python, "python", "", "Python interpreter for pytest and coverage.py (default: python on PATH)")
	return cmd
}

// loadIndexConfig loads the configuration the engine will use for repoDir: the
// explicit path, else hazard.yml, else .hazard/hazard.yml at the repository
// root, else the defaults. An explicit relative path is relative to the
// working directory, as for select.
func loadIndexConfig(repoDir, path string) (config.Config, error) {
	if path == "" {
		for _, rel := range []string{"hazard.yml", filepath.Join(".hazard", "hazard.yml")} {
			candidate := filepath.Join(repoDir, rel)
			_, err := os.Stat(candidate)
			if err == nil {
				path = candidate
				break
			}
			if !errors.Is(err, fs.ErrNotExist) {
				return config.Config{}, err
			}
		}
		if path == "" {
			return config.Defaults(), nil
		}
	}
	return config.Load(path)
}

func summarizeIndex(repoDir string, m *index.Manifest, coverage bool) indexSummary {
	s := indexSummary{
		Manifest:    filepath.ToSlash(filepath.Join(repoDir, ".hazard", "index", "manifest.json")),
		HeadSHA:     m.HeadSHA,
		Dirty:       m.Dirty,
		Tests:       len(m.Tests),
		Complete:    m.Complete,
		Diagnostics: append([]string{}, m.Diagnostics...),
	}
	if m.Graph != nil {
		s.Packages = len(m.Graph.Packages)
	}
	if m.Coverage != nil {
		s.Coverage = &indexCoverageSummary{Granularity: m.Coverage.Granularity}
		for _, tc := range m.Coverage.Tests {
			switch tc.Status {
			case "covered":
				s.Coverage.Covered++
			case "failed":
				s.Coverage.Failed++
			default:
				s.Coverage.NotRun++
			}
		}
	}
	return s
}

func writeIndexText(cmd *cobra.Command, s indexSummary, coverageRequested bool) error {
	var b strings.Builder
	fmt.Fprintf(&b, "indexed: tests=%d packages=%d complete=%t head=%s dirty=%t\n", s.Tests, s.Packages, s.Complete, s.HeadSHA, s.Dirty)
	switch {
	case s.Coverage != nil:
		fmt.Fprintf(&b, "coverage: granularity=%s covered=%d failed=%d not_run=%d\n", s.Coverage.Granularity, s.Coverage.Covered, s.Coverage.Failed, s.Coverage.NotRun)
	case coverageRequested:
		b.WriteString("coverage: unavailable\n")
	default:
		b.WriteString("coverage: not requested\n")
	}
	for _, d := range s.Diagnostics {
		fmt.Fprintf(&b, "diagnostic: %s\n", d)
	}
	fmt.Fprintf(&b, "manifest: %s\n", s.Manifest)
	_, err := fmt.Fprint(cmd.OutOrStdout(), b.String())
	return err
}
