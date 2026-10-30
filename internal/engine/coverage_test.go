package engine

import (
	"testing"

	"github.com/omorsi45/hazard/internal/impact"
	"github.com/omorsi45/hazard/internal/index"
)

const (
	// The initializer of table is a package-level statement: Go coverage counts
	// statements inside function bodies and never makes the initializer a block.
	tableCore       = "package core\n\nvar table = map[string]int{\"a\": 1}\n\nfunc Core() int { return table[\"a\"] }\n"
	tableCoreEdited = "package core\n\nvar table = map[string]int{\"a\": 2}\n\nfunc Core() int { return table[\"a\"] }\n"

	tableInitLine = 3
	tableFuncLine = 5
)

// TestSelectCoverageKeepsTestsForAnUninstrumentedChange: a committed change to
// a package-level variable initializer lies in no coverage block of any test,
// so coverage says nothing about it. The test that reads the variable, and
// every test that depends on its package, must stay selected.
func TestSelectCoverageKeepsTestsForAnUninstrumentedChange(t *testing.T) {
	t.Parallel()
	f := newFixture(t, map[string]string{"core/core.go": tableCore})
	f.indexNow(t, true)
	base := f.git(t, "rev-parse", "HEAD")
	f.write(t, map[string]string{"core/core.go": tableCoreEdited})
	head := commitAll(t, f.dir, "change the table")

	// The premise: coverage was collected, the function body is covered, and no
	// block of any test contains the initializer line.
	m, err := index.Load(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	if m.Coverage == nil {
		t.Fatalf("the index has no coverage, diagnostics: %v", m.Diagnostics)
	}
	var inFunc bool
	for id, tc := range m.Coverage.Tests {
		for _, b := range tc.Blocks {
			if b.File != "core/core.go" {
				continue
			}
			if b.StartLine <= tableInitLine && tableInitLine <= b.EndLine {
				t.Fatalf("%s: block %+v contains the initializer line, the fixture no longer shows the blind spot", id, b)
			}
			inFunc = inFunc || b.StartLine <= tableFuncLine && tableFuncLine <= b.EndLine
		}
	}
	if !inFunc {
		t.Fatalf("no block covers the function line: %+v", m.Coverage.Tests)
	}

	d := selectOK(t, Options{RepoDir: f.dir, Base: base, Head: head, Mode: impact.ModeCoverage, ModeSet: true})
	if d.L1 == nil || d.L1.CoverageState != "fresh" || d.L1.FullReason != "" {
		t.Fatalf("L1 = %+v, want fresh coverage without a full-suite directive", d.L1)
	}
	got, ok := byID(d.Hypothetical, idCore)
	if !ok {
		t.Fatalf("Hypothetical = %v, want %s kept: it reads the changed variable", ids(d.Hypothetical), idCore)
	}
	if got.Source != "certain" || got.Reason != "graph" {
		t.Errorf("%s: source %q reason %q, want certain/graph", idCore, got.Source, got.Reason)
	}
	// Everything that depends on core stays; the unrelated package does not.
	if want := []string{idCore, idLeft, idRght, idTop}; !sameStrings(ids(d.Hypothetical), want) {
		t.Errorf("Hypothetical = %v, want %v", ids(d.Hypothetical), want)
	}
}
