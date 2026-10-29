package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omorsi45/hazard/internal/config"
	"github.com/omorsi45/hazard/internal/index"
)

// TestSelectChangeError: any extraction failure other than a missing merge
// base is a full suite carrying the error text, never an empty change.
func TestSelectChangeError(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "plain")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := runGit(context.Background(), dir, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		t.Fatal(err)
	}
	if res.exit == 0 {
		t.Fatalf("premise: %s is inside a git repository (%s)", dir, strings.TrimSpace(string(res.stdout)))
	}

	d, err := Select(context.Background(), Options{RepoDir: dir})

	if err != nil {
		t.Fatalf("Select: %v, want a full-suite decision, not an error", err)
	}
	if !d.FullSuite || !d.Abstained || d.AbstainReason != "change_error" {
		t.Fatalf("FullSuite %v Abstained %v reason %q, want a change_error abstention", d.FullSuite, d.Abstained, d.AbstainReason)
	}
	if !strings.Contains(strings.Join(d.Warnings, "\n"), "not a git repository") {
		t.Errorf("Warnings = %v, want the git error text", d.Warnings)
	}
	if len(d.Tests) != 0 || d.Hypothetical != nil || d.Change != nil || d.L1 != nil || d.AllTests != nil {
		t.Errorf("an unknown change must not select anything: tests %v hypothetical %v change %v l1 %v all %v", ids(d.Tests), ids(d.Hypothetical), d.Change, d.L1, d.AllTests)
	}
}

// TestSelectFailSafeBranches shares one indexed repository between the two
// fail-safe branches that a normal run cannot reach. It swaps a package
// variable, so neither it nor its sub-tests run in parallel.
func TestSelectFailSafeBranches(t *testing.T) {
	f := newFixture(t, nil)
	f.indexNow(t, false)
	f.write(t, map[string]string{"README.md": "# docs only\n"})

	t.Run("a lock path that cannot be inspected runs the full suite", func(t *testing.T) {
		d := selectOK(t, Options{RepoDir: f.dir, LockPath: "bad\x00.lock"})

		if !d.FullSuite || !d.Abstained || d.AbstainReason != "lock_error" {
			t.Fatalf("FullSuite %v Abstained %v reason %q, want lock_error", d.FullSuite, d.Abstained, d.AbstainReason)
		}
		if !strings.Contains(strings.Join(d.Warnings, "\n"), "lock file could not be inspected") {
			t.Errorf("Warnings = %v, want the inspection failure", d.Warnings)
		}
		if len(d.Tests) != 0 {
			t.Errorf("Tests = %v, want none while FullSuite", ids(d.Tests))
		}
	})

	t.Run("a staleness check that fails makes the index stale", func(t *testing.T) {
		old := staleFor
		staleFor = func(*index.Manifest, config.Config, map[string]bool, index.HeadReader) (string, error) {
			return "", errors.New("head is unreadable")
		}
		t.Cleanup(func() { staleFor = old })

		d := selectOK(t, Options{RepoDir: f.dir})

		if !d.FullSuite || !d.Abstained || d.AbstainReason != "index_stale" {
			t.Fatalf("FullSuite %v Abstained %v reason %q, want index_stale", d.FullSuite, d.Abstained, d.AbstainReason)
		}
		if !strings.Contains(strings.Join(d.Warnings, "\n"), "index staleness could not be determined: head is unreadable") {
			t.Errorf("Warnings = %v, want the failure named", d.Warnings)
		}
		if d.L1 != nil || d.Hypothetical != nil || len(d.Tests) != 0 || d.AllTests != nil {
			t.Errorf("an index of unknown freshness must not drive selection: L1 %v hypothetical %v tests %v all %v", d.L1, ids(d.Hypothetical), ids(d.Tests), d.AllTests)
		}
	})
}
