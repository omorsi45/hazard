package index

import (
	"context"
	"sort"
	"strings"

	"github.com/omorsi45/hazard/internal/config"
)

// addCoverage collects per-test coverage for the enabled languages and merges
// it into m. Coverage a language cannot provide is reported in Diagnostics and
// leaves its tests without an entry; unknown coverage is never zero coverage.
func addCoverage(ctx context.Context, root, python string, cfg config.Config, m *Manifest) error {
	var parts []*Coverage
	if cfg.Languages.Go.Enabled {
		res, err := collectGoCoverage(ctx, root, cfg, m)
		m.Commands = append(m.Commands, res.Commands...)
		m.Diagnostics = append(m.Diagnostics, res.Diagnostics...)
		if err != nil {
			return err
		}
		if res.Coverage != nil {
			parts = append(parts, res.Coverage)
		}
	}
	if cfg.Languages.Python.Enabled {
		res, err := collectPythonCoverage(ctx, root, python, cfg, m)
		m.Commands = append(m.Commands, res.Commands...)
		m.Diagnostics = append(m.Diagnostics, res.Diagnostics...)
		if err != nil {
			return err
		}
		if res.Coverage != nil {
			parts = append(parts, res.Coverage)
		}
	}
	m.Coverage = mergeCoverage(m, parts)
	return nil
}

// mergeCoverage combines per-language coverage into the manifest's single
// Coverage. It returns nil when no language produced coverage. Each language
// keeps its own granularity, so the merged Granularity lists the contract
// values of the languages that contributed, sorted and joined with ",":
// "go-top-level-test", "python-test-context" or
// "go-top-level-test,python-test-context".
func mergeCoverage(m *Manifest, parts []*Coverage) *Coverage {
	if len(parts) == 0 {
		return nil
	}
	merged := &Coverage{
		HeadSHA:       m.HeadSHA,
		ContentDigest: m.ContentDigest,
		Tests:         map[string]TestCoverage{},
		FileDigests:   map[string]string{},
	}
	var granularities []string
	for _, p := range parts {
		granularities = append(granularities, p.Granularity)
		for id, tc := range p.Tests {
			merged.Tests[id] = tc
		}
		for file, digest := range p.FileDigests {
			merged.FileDigests[file] = digest
		}
	}
	sort.Strings(granularities)
	merged.Granularity = strings.Join(granularities, ",")
	return merged
}
