// Package hazard selects the tests a change could break. It is the public entry
// point to the same selection engine the hazard command line runs, for programs
// that embed Hazard instead of running it.
//
// Select never starts a test runner and never changes the working directory.
// Run what it returns with the project's own runner: when Decision.FullSuite is
// true, run the complete native suite.
package hazard

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/omorsi45/hazard/internal/engine"
)

// Options describes one selection. Empty Base, Head, ConfigPath and LockPath
// take the defaults the hazard command documents: Base is the merge base with
// the default branch, Head is the working tree, the configuration is
// hazard.yml (then .hazard/hazard.yml) and the lock is hazard.lock.
type Options struct {
	// RepoDir is the root of the git repository. It is required. A relative
	// path is resolved against the process working directory.
	RepoDir string
	// Base and Head are git refs of the change to select tests for.
	Base, Head string
	// ConfigPath and LockPath may be relative; they are then relative to RepoDir.
	ConfigPath, LockPath string
}

// SelectedTest is one test of a narrowed selection.
type SelectedTest struct {
	// ID is the stable test identifier, such as go:example.com/m/core:TestFoo.
	ID string
	// Source is why the test is in the selection: certain, added, always_run or tier.
	Source string
	// Reason is the human-readable reason for the selection.
	Reason string
	// Package and RunnerID name the test to its native runner: the Go import
	// path and test name, or the pytest node ID. They are empty for a test
	// that is not in the index.
	Package, RunnerID string
	// P is the predicted probability that the change breaks the test. It is nil
	// when no probability was computed, never zero.
	P *float64
}

// Decision is the outcome of Select.
type Decision struct {
	// ID identifies this decision.
	ID string
	// FullSuite is authoritative: when true, run the complete native suite. Tests
	// is then empty and is not a list of the suite.
	FullSuite bool
	// Abstained is true when Hazard declined to narrow; AbstainReason says why.
	Abstained     bool
	AbstainReason string
	// LockDigest identifies the calibration lock behind the decision, if any.
	LockDigest string
	// Tests is the narrowed selection, sorted by ID. It is empty for a full suite.
	Tests []SelectedTest
}

// Select decides which tests to run for opts. An ordinary refusal to narrow is a
// valid Decision with FullSuite set; the error is for an invalid invocation (a
// missing RepoDir, a ref that does not resolve, an invalid configuration) or a
// canceled context. On error the Decision is the zero value and must not be
// read as an empty selection.
func Select(ctx context.Context, opts Options) (Decision, error) {
	if opts.RepoDir == "" {
		return Decision{}, errors.New("hazard: Options.RepoDir is required")
	}
	dir, err := filepath.Abs(opts.RepoDir)
	if err != nil {
		return Decision{}, fmt.Errorf("hazard: resolve RepoDir %q: %w", opts.RepoDir, err)
	}
	cfg := opts.ConfigPath
	if cfg != "" && !filepath.IsAbs(cfg) {
		cfg = filepath.Join(dir, cfg)
	}
	d, err := engine.Select(ctx, engine.Options{RepoDir: dir, Base: opts.Base, Head: opts.Head, ConfigPath: cfg, LockPath: opts.LockPath})
	if err != nil {
		return Decision{}, fmt.Errorf("hazard: %w", err)
	}
	return fromEngine(d), nil
}

// fromEngine copies the public part of an engine decision.
func fromEngine(d engine.Decision) Decision {
	out := Decision{ID: d.ID, FullSuite: d.FullSuite, Abstained: d.Abstained, AbstainReason: d.AbstainReason, LockDigest: d.LockDigest}
	for _, s := range d.Tests {
		t := SelectedTest{ID: s.TestID, Source: s.Source, Reason: s.Reason, Package: s.Package, RunnerID: s.RunnerID}
		if s.P != nil {
			p := *s.P
			t.P = &p
		}
		out.Tests = append(out.Tests, t)
	}
	return out
}
