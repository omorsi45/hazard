package engine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omorsi45/hazard/internal/config"
	"github.com/omorsi45/hazard/internal/index"
)

const leftEdited = "package left\n\nimport \"example.com/m/core\"\n\nfunc Left() int { return core.Core() + 0 }\n"

// coreChangeSet is what a change to core/core.go selects through the diamond.
var coreChangeSet = []string{idCore, idLeft, idRght, idTop}

// TestSelectEditAfterIndexingIsNotStale is D055: the index may differ from the
// work tree inside the change set, so editing a source file after `hazard
// index` no longer discards the index.
func TestSelectEditAfterIndexingIsNotStale(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	f.indexNow(t, false)
	f.write(t, map[string]string{"core/core.go": coreEdited})

	d := selectOK(t, Options{RepoDir: f.dir})

	if d.AbstainReason != "no_lock" {
		t.Fatalf("reason %q warnings %v, want the index to be usable (no lock)", d.AbstainReason, d.Warnings)
	}
	if !sameStrings(ids(d.Hypothetical), coreChangeSet) {
		t.Errorf("Hypothetical = %v, want the dependents of core %v", ids(d.Hypothetical), coreChangeSet)
	}
	if !sameStrings(d.AllTests, diamondIDs) {
		t.Errorf("AllTests = %v, want %v", d.AllTests, diamondIDs)
	}
	if strings.Contains(strings.Join(d.Warnings, "\n"), "stale") {
		t.Errorf("Warnings = %v, want no staleness warning", d.Warnings)
	}
}

// TestSelectIndexedTreeDifferingOutsideTheDiff covers an explicit base and head
// (read through git) and an explicit base with the work tree as head, over one
// history: the index was taken at c1, then core changed in c2 and left in c3.
func TestSelectIndexedTreeDifferingOutsideTheDiff(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	c1 := f.git(t, "rev-parse", "HEAD")
	f.indexNow(t, false)
	f.write(t, map[string]string{"core/core.go": coreEdited})
	c2 := commitAll(t, f.dir, "edit core")
	f.write(t, map[string]string{"left/left.go": leftEdited})
	c3 := commitAll(t, f.dir, "edit left")

	fresh := func(t *testing.T, opts Options, want []string) {
		t.Helper()
		d := selectOK(t, opts)
		if d.AbstainReason != "no_lock" {
			t.Fatalf("%+v: reason %q warnings %v, want the index to be usable", opts, d.AbstainReason, d.Warnings)
		}
		if !sameStrings(ids(d.Hypothetical), want) {
			t.Errorf("%+v: Hypothetical = %v, want %v", opts, ids(d.Hypothetical), want)
		}
	}
	stale := func(t *testing.T, opts Options, file string) {
		t.Helper()
		d := selectOK(t, opts)
		if !d.FullSuite || d.AbstainReason != "index_stale" {
			t.Fatalf("%+v: FullSuite %v reason %q, want index_stale", opts, d.FullSuite, d.AbstainReason)
		}
		if !strings.Contains(strings.Join(d.Warnings, "\n"), "file changed: "+file) {
			t.Errorf("%+v: Warnings = %v, want the stale file %s", opts, d.Warnings, file)
		}
		if d.L1 != nil || d.Hypothetical != nil || len(d.Tests) != 0 || d.AllTests != nil {
			t.Errorf("%+v: a stale index must not drive selection: L1 %v hypothetical %v tests %v all %v", opts, d.L1, d.Hypothetical, d.Tests, d.AllTests)
		}
	}

	t.Run("explicit head: the only drift is inside the diff", func(t *testing.T) {
		fresh(t, Options{RepoDir: f.dir, Base: c1, Head: c2}, coreChangeSet)
	})
	t.Run("work tree head: the only drift is inside the diff", func(t *testing.T) {
		// core and left both differ from the index and both are in c1..work tree.
		fresh(t, Options{RepoDir: f.dir, Base: c1}, coreChangeSet)
	})
	t.Run("explicit head: a recorded file outside the diff differs", func(t *testing.T) {
		stale(t, Options{RepoDir: f.dir, Base: c2, Head: c3}, "core/core.go")
	})
	t.Run("work tree head: a recorded file outside the diff differs", func(t *testing.T) {
		stale(t, Options{RepoDir: f.dir, Base: c3}, "core/core.go")
	})
}

// TestSelectUnrecordedFileOutsideTheDiffIsStale: a source file that appeared
// after indexing and is not part of the change cannot be in the graph.
func TestSelectUnrecordedFileOutsideTheDiffIsStale(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	f.indexNow(t, false)
	f.write(t, map[string]string{"solo/extra.go": "package solo\n\nfunc Extra() int { return 2 }\n"})
	c2 := commitAll(t, f.dir, "add extra")
	f.write(t, map[string]string{"core/core.go": coreEdited})
	c3 := commitAll(t, f.dir, "edit core")

	for _, opts := range []Options{
		{RepoDir: f.dir, Base: c2, Head: c3}, // listed from the commit
		{RepoDir: f.dir, Base: c2},           // listed from the work tree
	} {
		d := selectOK(t, opts)
		if !d.FullSuite || d.AbstainReason != "index_stale" {
			t.Fatalf("%+v: FullSuite %v reason %q, want index_stale", opts, d.FullSuite, d.AbstainReason)
		}
		if !strings.Contains(strings.Join(d.Warnings, "\n"), "new file: solo/extra.go") {
			t.Errorf("%+v: Warnings = %v, want the unrecorded file solo/extra.go", opts, d.Warnings)
		}
	}
}

// ignoredConfigYML is a configuration that keeps the diamond module usable
// without Python.
const ignoredConfigYML = "version: 1\nlanguages:\n  python:\n    enabled: false\n"

// ignoredConfigFixture commits the diamond module without a hazard.yml, writes
// the configuration to .hazard/hazard.yml (ignored by git, so no commit ever
// holds it) and publishes an index built with that configuration. The index
// records the config file: Build hashes it even though git ignores it.
func ignoredConfigFixture(t *testing.T) *fixture {
	t.Helper()
	files := diamondFiles()
	delete(files, "hazard.yml")
	f := newFixtureFiles(t, files)
	f.write(t, map[string]string{".hazard/hazard.yml": ignoredConfigYML})
	f.git(t, "check-ignore", "-q", ".hazard/hazard.yml") // premise: ignored
	if tracked := f.git(t, "ls-files", ".hazard/hazard.yml"); tracked != "" {
		t.Fatalf("premise: git tracks %q", tracked)
	}
	cfg, err := config.Load(filepath.Join(f.dir, ".hazard", "hazard.yml"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := index.Build(context.Background(), f.dir, cfg, index.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Files[".hazard/hazard.yml"]; !ok {
		t.Fatalf("premise: the index does not record the ignored config file: %v", m.Files)
	}
	if err := index.Publish(f.dir, m); err != nil {
		t.Fatal(err)
	}
	return f
}

// TestSelectExplicitHeadWithAnUntrackedConfigIsNotStale: the configuration is
// compared by digest, not as content of the head. A config file that git
// ignores is in no commit, so reading it from an explicit head used to report
// "file removed: .hazard/hazard.yml" and abstain every selection.
func TestSelectExplicitHeadWithAnUntrackedConfigIsNotStale(t *testing.T) {
	t.Parallel()
	f := ignoredConfigFixture(t)
	c1 := f.git(t, "rev-parse", "HEAD")
	f.write(t, map[string]string{"core/core.go": coreEdited})
	c2 := commitAll(t, f.dir, "edit core")

	d := selectOK(t, Options{RepoDir: f.dir, Base: c1, Head: c2})

	if d.AbstainReason == "index_stale" {
		t.Fatalf("reason %q warnings %v, want the index to be usable", d.AbstainReason, d.Warnings)
	}
	if d.L1 == nil {
		t.Fatalf("L1 is nil (reason %q, warnings %v), want the deterministic layer", d.AbstainReason, d.Warnings)
	}
	if !sameStrings(ids(d.Hypothetical), coreChangeSet) {
		t.Errorf("Hypothetical = %v, want the dependents of core %v", ids(d.Hypothetical), coreChangeSet)
	}
}

// TestSelectExplicitHeadStillAbstainsWhenTheUntrackedConfigChanges is the safety
// side: the digest of the loaded configuration still decides, so editing the
// ignored config after indexing makes the index stale for an explicit head.
func TestSelectExplicitHeadStillAbstainsWhenTheUntrackedConfigChanges(t *testing.T) {
	t.Parallel()
	f := ignoredConfigFixture(t)
	c1 := f.git(t, "rev-parse", "HEAD")
	f.write(t, map[string]string{"core/core.go": coreEdited})
	c2 := commitAll(t, f.dir, "edit core")
	f.write(t, map[string]string{".hazard/hazard.yml": ignoredConfigYML + "impact:\n  max_hops: 1\n"})

	d := selectOK(t, Options{RepoDir: f.dir, Base: c1, Head: c2})

	if !d.FullSuite || d.AbstainReason != "index_stale" {
		t.Fatalf("FullSuite %v reason %q, want index_stale", d.FullSuite, d.AbstainReason)
	}
	if !strings.Contains(strings.Join(d.Warnings, "\n"), "config changed") {
		t.Errorf("Warnings = %v, want the changed configuration reported", d.Warnings)
	}
	if d.L1 != nil || d.Hypothetical != nil || len(d.Tests) != 0 || d.AllTests != nil {
		t.Errorf("a stale index must not drive selection: L1 %v hypothetical %v tests %v all %v", d.L1, d.Hypothetical, d.Tests, d.AllTests)
	}
}
