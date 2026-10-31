package index

import (
	"reflect"
	"testing"
)

func TestMergeCoverageKeepsGranularitiesExplicit(t *testing.T) {
	m := &Manifest{HeadSHA: "head", ContentDigest: "digest"}
	goPart := &Coverage{
		Granularity: goCoverageGranularity,
		Tests:       map[string]TestCoverage{"go:example.com/m/a:TestA": {Status: "covered", Blocks: []Block{{File: "a/a.go", StartLine: 3, EndLine: 4}}}},
		FileDigests: map[string]string{"a/a.go": "go-digest"},
	}
	pyPart := &Coverage{
		Granularity: pythonCoverageGranularity,
		Tests:       map[string]TestCoverage{"py:tests.test_a:test_a": {Status: "not_run"}},
		FileDigests: map[string]string{"pkg/a.py": "py-digest"},
	}

	for name, parts := range map[string][]*Coverage{"go first": {goPart, pyPart}, "python first": {pyPart, goPart}} {
		got := mergeCoverage(m, parts)
		if got == nil {
			t.Fatalf("%s: both languages produced coverage but the merge is nil", name)
		}
		if want := "go-top-level-test,python-test-context"; got.Granularity != want {
			t.Errorf("%s: Granularity = %q, want the two contract values joined with a comma in sorted order: %q", name, got.Granularity, want)
		}
		if got.HeadSHA != "head" || got.ContentDigest != "digest" {
			t.Errorf("%s: header = %q %q, want the manifest's", name, got.HeadSHA, got.ContentDigest)
		}
		wantTests := map[string]TestCoverage{}
		for _, p := range []*Coverage{goPart, pyPart} {
			for id, tc := range p.Tests {
				wantTests[id] = tc
			}
		}
		if !reflect.DeepEqual(got.Tests, wantTests) {
			t.Errorf("%s: Tests = %+v, want %+v", name, got.Tests, wantTests)
		}
		if want := map[string]string{"a/a.go": "go-digest", "pkg/a.py": "py-digest"}; !reflect.DeepEqual(got.FileDigests, want) {
			t.Errorf("%s: FileDigests = %v, want %v", name, got.FileDigests, want)
		}
	}

	if got := mergeCoverage(m, []*Coverage{goPart}); got == nil || got.Granularity != "go-top-level-test" {
		t.Errorf("Go only: %+v", got)
	}
	if got := mergeCoverage(m, []*Coverage{pyPart}); got == nil || got.Granularity != "python-test-context" {
		t.Errorf("Python only: %+v", got)
	}
	if got := mergeCoverage(m, nil); got != nil {
		t.Errorf("no coverage from any language must stay nil: %+v", got)
	}
}
