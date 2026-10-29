package impact

import (
	"testing"

	"github.com/omorsi45/hazard/internal/change"
	"github.com/omorsi45/hazard/internal/index"
)

func TestResolvePackage(t *testing.T) {
	g := fixtureIndex().Graph
	owner := fileOwners(g)
	cases := []struct {
		name string
		test index.Test
		want string
	}{
		{"Go test by import path", index.Test{Package: "m/left", File: "left/left_test.go"}, "m/left"},
		{"Python module test through its file", index.Test{Package: "tests.test_mod", File: "tests/test_mod.py"}, "py:tests.test_mod"},
		{"Python class test through its file", index.Test{Package: "tests.test_mod.TestK", File: "tests/test_mod.py"}, "py:tests.test_mod"},
		{"Python class test through the module name", index.Test{Package: "tests.test_mod.TestK.Inner", File: "elsewhere.py"}, "py:tests.test_mod"},
		{"unknown package and file", index.Test{Package: "ghost", File: "ghost/ghost_test.go"}, ""},
	}
	for _, tc := range cases {
		if got := resolvePackage(g, owner, tc.test); got != tc.want {
			t.Errorf("%s: resolvePackage = %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := resolvePackage(nil, nil, index.Test{Package: "m/left"}); got != "" {
		t.Errorf("nil graph: resolvePackage = %q, want empty", got)
	}
}

func TestIndexWithoutGraphMapsNothing(t *testing.T) {
	in := graphInput(changeSet(cf("core/core.go", "M", change.KindSource, hunk(1, 1))))
	in.Index.Graph = nil
	if got := mustL1(t, in).FullReason; got != "unmapped_change:core/core.go" {
		t.Fatalf("FullReason = %q, want unmapped_change:core/core.go", got)
	}
	in = graphInput(changeSet(cf("README.md", "M", change.KindDocs, hunk(1, 1))))
	in.Index.Graph = nil
	r := mustL1(t, in)
	if r.FullReason != "" || len(r.Certain) != 0 || r.PackageCount != 0 || r.RawCandidates != len(r.AllTests) {
		t.Fatalf("docs-only change without a graph: %+v", r)
	}
}

func TestDuplicateInventoryIDsCountOnce(t *testing.T) {
	in := graphInput(changeSet(cf("README.md", "M", change.KindDocs, hunk(1, 1))))
	in.Index.Tests = append(in.Index.Tests, in.Index.Tests[0])
	r := mustL1(t, in)
	if len(r.AllTests) != 10 || r.RawCandidates != 10 {
		t.Fatalf("AllTests %d RawCandidates %d, want 10 and 10", len(r.AllTests), r.RawCandidates)
	}
}
