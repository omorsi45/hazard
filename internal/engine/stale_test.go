package engine

import (
	"strings"
	"testing"
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
		t.Fatalf("reason %q warnings %v, want the index to be usable (no_lock placeholder)", d.AbstainReason, d.Warnings)
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
