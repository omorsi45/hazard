package impact

import (
	"sort"
	"strings"
	"testing"

	"github.com/omorsi45/hazard/internal/change"
	"github.com/omorsi45/hazard/internal/config"
	"github.com/omorsi45/hazard/internal/graph"
	"github.com/omorsi45/hazard/internal/index"
)

// The fixture graph is a diamond with a tail, plus an unrelated package and
// two Python modules:
//
//	m/core <- m/left, m/right <- m/top <- m/app <- m/cli      (hops from core: 0 1 1 2 3 4)
//	m/solo                                                      (unrelated)
//	py:app.mod <- py:tests.test_mod                             (hops from app.mod: 0 1)
//
// Every package owns the tests named in fixtureTests.

func goPkg(id string, imports ...string) *graph.Package {
	dir := strings.TrimPrefix(id, "m/")
	return &graph.Package{
		ID: id, Language: "go", Dir: dir,
		Files:     []string{dir + "/" + dir + ".go"},
		TestFiles: []string{dir + "/" + dir + "_test.go"},
		Imports:   imports,
	}
}

func goTest(pkg string, name string) index.Test {
	dir := strings.TrimPrefix(pkg, "m/")
	return index.Test{ID: "go:" + pkg + ":" + name, Language: "go", Package: pkg, Name: name, File: dir + "/" + dir + "_test.go", RunnerID: name}
}

func pyTest(pkg, file, name string) index.Test {
	return index.Test{ID: "py:" + pkg + ":" + name, Language: "python", Package: pkg, Name: name, File: file, RunnerID: file + "::" + name}
}

// fixtureIndex returns a complete manifest over the fixture graph.
func fixtureIndex() *index.Manifest {
	g := &graph.Graph{Packages: map[string]*graph.Package{}}
	for _, p := range []*graph.Package{
		goPkg("m/core"),
		goPkg("m/left", "m/core"),
		goPkg("m/right", "m/core"),
		goPkg("m/top", "m/left", "m/right"),
		goPkg("m/app", "m/top"),
		goPkg("m/cli", "m/app"),
		goPkg("m/solo"),
		{ID: "py:app.mod", Language: "python", Dir: "app", Files: []string{"app/mod.py"}},
		{ID: "py:tests.test_mod", Language: "python", Dir: "tests", TestFiles: []string{"tests/test_mod.py"}, Imports: []string{"py:app.mod"}},
	} {
		g.Packages[p.ID] = p
	}
	tests := []index.Test{
		goTest("m/core", "TestCore"), goTest("m/core", "TestCoreAux"),
		goTest("m/left", "TestLeft"), goTest("m/right", "TestRight"),
		goTest("m/top", "TestTop"), goTest("m/app", "TestApp"),
		goTest("m/cli", "TestCli"), goTest("m/solo", "TestSolo"),
		// Python tests carry the module (and class) as their Package, not the graph ID.
		pyTest("tests.test_mod", "tests/test_mod.py", "test_a"),
		pyTest("tests.test_mod.TestK", "tests/test_mod.py", "test_b"),
	}
	sort.Slice(tests, func(i, j int) bool { return tests[i].ID < tests[j].ID })
	return &index.Manifest{Schema: index.SchemaVersion, Graph: g, Tests: tests, Complete: true}
}

// testID builds the Go test ID used by the fixture.
func testID(pkg, name string) string { return "go:" + pkg + ":" + name }

func langOfPath(path string) string {
	switch {
	case strings.HasSuffix(path, ".go"):
		return "go"
	case strings.HasSuffix(path, ".py"):
		return "python"
	}
	return ""
}

// cf builds a changed file; hunks are given as {oldStart, oldLines, newStart, newLines, symbols...}.
func cf(path, status string, kind change.FileKind, hunks ...change.Hunk) change.ChangedFile {
	return change.ChangedFile{Path: path, Status: status, Kind: kind, Language: langOfPath(path), Hunks: hunks}
}

func hunk(oldStart, oldLines int, symbols ...string) change.Hunk {
	return change.Hunk{OldStart: oldStart, OldLines: oldLines, NewStart: oldStart, NewLines: oldLines, Symbols: symbols}
}

func changeSet(files ...change.ChangedFile) *change.ChangeSet {
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	cs := &change.ChangeSet{BaseSHA: "base", HeadSHA: "head", Files: files}
	for _, f := range files {
		for _, h := range f.Hunks {
			cs.LinesAdded += h.NewLines
			cs.LinesRemoved += h.OldLines
		}
	}
	return cs
}

func defaultImpact() config.Impact {
	return config.Impact{
		MaxHops:               3,
		CoverageMaxAgeCommits: 200,
		MaxCandidates:         400,
		CandidateRanking:      []string{"coverage_overlap", "hops", "cofailure", "recency"},
	}
}

// graphInput is a ModeGraph input over the fixture index.
func graphInput(cs *change.ChangeSet) Input {
	return Input{Change: cs, Index: fixtureIndex(), Impact: defaultImpact(), Mode: ModeGraph}
}

func mustL1(t *testing.T, in Input) *Result {
	t.Helper()
	r, err := L1(in)
	if err != nil {
		t.Fatalf("L1 error: %v", err)
	}
	if r == nil {
		t.Fatal("L1 returned a nil result without an error")
	}
	return r
}

// certainMap returns test ID -> reason.
func certainMap(r *Result) map[string]string {
	m := map[string]string{}
	for _, c := range r.Certain {
		m[c.TestID] = c.Reason
	}
	return m
}

func candidateIDs(r *Result) []string {
	ids := make([]string, 0, len(r.Candidates))
	for _, c := range r.Candidates {
		ids = append(ids, c.TestID)
	}
	return ids
}
