package change

import (
	"testing"
)

// buildWorldHazard has Hazard's own state directory in several shapes next to
// look-alike names that are ordinary files:
//
//	.hazard/state.json   tracked, edited          .hazard/old.go   renamed out to moved.go
//	src/x.go             renamed into .hazard/    sub/.hazard/     not the top-level directory
//	.hazardous/          shares only a prefix     a.go             an ordinary edit
func buildWorldHazard() (*history, string) {
	h := &history{}
	m1 := h.commit(heads("main"), 0, "base",
		put("a.go", goBase),
		put(".hazard/state.json", "{}\n"),
		put(".hazard/old.go", uniqueGo("hold")),
		put("sub/.hazard/keep.txt", "1\n"),
		put(".hazardous/keep.txt", "1\n"),
		put("src/x.go", uniqueGo("xsrc")))
	h.commit(heads("main"), m1, "edits",
		put("a.go", replaceLine(goBase, 7, alphaEdited)),
		put(".hazard/state.json", "{\"n\":1}\n"),
		put("sub/.hazard/keep.txt", "2\n"),
		put(".hazardous/keep.txt", "2\n"),
		rename(".hazard/old.go", "moved.go"),
		rename("src/x.go", ".hazard/x.go"))
	return h, "main"
}

var worldHazard shared

func TestExtractExcludesHazardStateFromACommitRange(t *testing.T) {
	t.Parallel()
	r := worldHazard.get(t, "worldHazard", buildWorldHazard)
	opts := testOptions()
	opts.Base, opts.Head = r.sha(1), r.sha(2)

	cs := mustExtract(t, r.dir, opts)

	// Only the top-level .hazard directory is Hazard's state: a path on either
	// side of a rename counts, a nested .hazard and .hazardous do not.
	wantPaths(t, cs, ".hazardous/keep.txt", "a.go", "sub/.hazard/keep.txt")
	if cs.LinesAdded != 3 || cs.LinesRemoved != 3 {
		t.Errorf("lines +%d -%d, want +3 -3 counted from the remaining files only", cs.LinesAdded, cs.LinesRemoved)
	}
}

func TestExtractExcludesHazardStateFromTheWorkingTree(t *testing.T) {
	t.Parallel()
	h := &history{}
	h.commit(heads("main"), 0, "base", put("a.go", goBase), put(".hazard/state.json", "{}\n"))
	r := newRepo(t, h, "main", true)

	// Hazard's own files are the only difference: nothing changed.
	r.write(t, ".hazard/state.json", "{\"n\":1}\n")
	r.write(t, ".hazard/index/manifest.json", "{}\n")
	cs := mustExtract(t, r.dir, testOptions())
	wantPaths(t, cs)
	if cs.Dirty {
		t.Error("Dirty = true when only .hazard/ differs from HEAD")
	}
	if len(cs.TriggersFull) != 0 || cs.LinesAdded != 0 || cs.LinesRemoved != 0 {
		t.Errorf("triggers %q lines +%d -%d, want none", cs.TriggersFull, cs.LinesAdded, cs.LinesRemoved)
	}

	// A real edit next to them is the whole change set.
	r.write(t, "a.go", replaceLine(goBase, 7, alphaEdited))
	cs = mustExtract(t, r.dir, testOptions())
	wantPaths(t, cs, "a.go")
	if !cs.Dirty {
		t.Error("Dirty = false with an edited a.go")
	}
	if cs.LinesAdded != 1 || cs.LinesRemoved != 1 {
		t.Errorf("lines +%d -%d, want +1 -1", cs.LinesAdded, cs.LinesRemoved)
	}
}
