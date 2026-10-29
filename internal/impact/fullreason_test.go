package impact

import (
	"reflect"
	"testing"

	"github.com/omorsi45/hazard/internal/change"
	"github.com/omorsi45/hazard/internal/graph"
)

func TestFullReasons(t *testing.T) {
	dynamicIndex := func(ids ...string) func(*Input) {
		return func(in *Input) {
			for _, id := range ids {
				in.Index.Graph.Packages[id] = &graph.Package{ID: id, Language: "python", Dir: "dyn", Files: []string{"dyn/" + id + ".py"}, Dynamic: true}
			}
		}
	}
	renamed := func(newPath, oldPath string) change.ChangedFile {
		f := cf(newPath, "R", change.KindSource, hunk(1, 1))
		f.OldPath = oldPath
		return f
	}
	cases := []struct {
		name   string
		files  []change.ChangedFile
		edit   func(*Input)
		want   string
		wantID bool // AllTests must be filled
	}{
		{
			name:  "nil index",
			files: []change.ChangedFile{cf("core/core.go", "M", change.KindSource, hunk(3, 1))},
			edit:  func(in *Input) { in.Index = nil },
			want:  "no_index",
		},
		{
			name:   "incomplete index",
			files:  []change.ChangedFile{cf("core/core.go", "M", change.KindSource, hunk(3, 1))},
			edit:   func(in *Input) { in.Index.Complete = false },
			want:   "index_incomplete",
			wantID: true,
		},
		{
			name:   "full trigger",
			files:  []change.ChangedFile{cf("go.mod", "M", change.KindBuild, hunk(1, 1))},
			edit:   func(in *Input) { in.Change.TriggersFull = []string{"go.mod:go.mod", "Makefile:Makefile"} },
			want:   "trigger:go.mod:go.mod",
			wantID: true,
		},
		{
			name:  "missing index beats a trigger",
			files: []change.ChangedFile{cf("go.mod", "M", change.KindBuild, hunk(1, 1))},
			edit: func(in *Input) {
				in.Index = nil
				in.Change.TriggersFull = []string{"go.mod:go.mod"}
			},
			want: "no_index",
		},
		{
			name:   "incomplete index beats a trigger",
			files:  []change.ChangedFile{cf("go.mod", "M", change.KindBuild, hunk(1, 1))},
			edit:   func(in *Input) { in.Index.Complete = false; in.Change.TriggersFull = []string{"go.mod:go.mod"} },
			want:   "index_incomplete",
			wantID: true,
		},
		{
			name:   "unmapped Go file",
			files:  []change.ChangedFile{cf("newpkg/x.go", "A", change.KindSource, hunk(0, 0))},
			want:   "unmapped_change:newpkg/x.go",
			wantID: true,
		},
		{
			name:   "unmapped Python file",
			files:  []change.ChangedFile{cf("lib/other.py", "M", change.KindSource, hunk(2, 1))},
			want:   "unmapped_change:lib/other.py",
			wantID: true,
		},
		{
			name: "first unmapped file in path order",
			files: []change.ChangedFile{
				cf("zeta/z.go", "M", change.KindSource, hunk(1, 1)),
				cf("alpha/a.go", "M", change.KindSource, hunk(1, 1)),
				cf("core/core.go", "M", change.KindSource, hunk(1, 1)),
			},
			want: "unmapped_change:alpha/a.go",
		},
		{
			name:  "deleted file of a removed package",
			files: []change.ChangedFile{cf("gone/gone.go", "D", change.KindSource, hunk(1, 5))},
			want:  "unmapped_change:gone/gone.go",
		},
		{
			name:  "renamed file mapped through its old path only",
			files: []change.ChangedFile{renamed("elsewhere/core.go", "core/core.go")},
			want:  "",
		},
		{
			name:  "renamed file mapped through its new path only",
			files: []change.ChangedFile{renamed("core/new.go", "elsewhere/old.go")},
			want:  "",
		},
		{
			name:  "renamed file mapped through neither path",
			files: []change.ChangedFile{renamed("elsewhere/new.go", "elsewhere/old.go")},
			want:  "unmapped_change:elsewhere/new.go",
		},
		{
			name: "renamed away from an enabled language",
			files: []change.ChangedFile{func() change.ChangedFile {
				f := cf("notes/core.txt", "R", change.KindDocs, hunk(1, 1))
				f.OldPath = "elsewhere/core.go"
				return f
			}()},
			want: "unmapped_change:notes/core.txt",
		},
		{
			name: "files without a language never need a package",
			files: []change.ChangedFile{
				cf("README.md", "M", change.KindDocs, hunk(1, 1)),
				cf("config/app.yaml", "M", change.KindConfig, hunk(1, 1)),
				cf("data/blob.bin", "M", change.KindFixture),
			},
			want: "",
		},
		{
			name:  "dynamic import with a changed Python file",
			files: []change.ChangedFile{cf("app/mod.py", "M", change.KindSource, hunk(2, 1))},
			edit:  dynamicIndex("py:z.dyn", "py:a.dyn"),
			want:  "python_dynamic_import:py:a.dyn",
		},
		{
			name:  "dynamic import without a changed Python file",
			files: []change.ChangedFile{cf("core/core.go", "M", change.KindSource, hunk(2, 1))},
			edit:  dynamicIndex("py:a.dyn"),
			want:  "",
		},
		{
			name: "unmapped change is reported before a dynamic import",
			files: []change.ChangedFile{
				cf("app/mod.py", "M", change.KindSource, hunk(2, 1)),
				cf("newpkg/x.go", "M", change.KindSource, hunk(2, 1)),
			},
			edit: dynamicIndex("py:a.dyn"),
			want: "unmapped_change:newpkg/x.go",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := graphInput(changeSet(tc.files...))
			if tc.edit != nil {
				tc.edit(&in)
			}
			r := mustL1(t, in)
			if r.FullReason != tc.want {
				t.Fatalf("FullReason = %q, want %q", r.FullReason, tc.want)
			}
			if tc.want == "" {
				return
			}
			if len(r.Certain) != 0 || len(r.Candidates) != 0 {
				t.Fatalf("a full-suite directive carries no selection: certain %v candidates %v", r.Certain, candidateIDs(r))
			}
			if tc.wantID {
				if len(r.AllTests) != len(fixtureIndex().Tests) {
					t.Fatalf("AllTests = %v, want the full inventory", r.AllTests)
				}
			}
		})
	}
}

func TestFullReasonAllTestsSorted(t *testing.T) {
	in := graphInput(changeSet(cf("core/core.go", "M", change.KindSource, hunk(3, 1))))
	in.Index.Complete = false
	// Present the inventory out of order: AllTests must still come back sorted.
	tests := in.Index.Tests
	for i, j := 0, len(tests)-1; i < j; i, j = i+1, j-1 {
		tests[i], tests[j] = tests[j], tests[i]
	}
	r := mustL1(t, in)
	want := []string{
		"go:m/app:TestApp", "go:m/cli:TestCli", "go:m/core:TestCore", "go:m/core:TestCoreAux",
		"go:m/left:TestLeft", "go:m/right:TestRight", "go:m/solo:TestSolo", "go:m/top:TestTop",
		"py:tests.test_mod.TestK:test_b", "py:tests.test_mod:test_a",
	}
	if !reflect.DeepEqual(r.AllTests, want) {
		t.Fatalf("AllTests = %v, want %v", r.AllTests, want)
	}
}

func TestInvalidInvocation(t *testing.T) {
	good := func() Input { return graphInput(changeSet(cf("core/core.go", "M", change.KindSource, hunk(3, 1)))) }
	cases := []struct {
		name string
		edit func(*Input)
	}{
		{"nil change set", func(in *Input) { in.Change = nil }},
		{"negative max hops", func(in *Input) { in.Impact.MaxHops = -1 }},
		{"unknown mode", func(in *Input) { in.Mode = Mode(7) }},
		{"no candidate cap", func(in *Input) { in.Impact.MaxCandidates = 0 }},
		{"unknown ranking token", func(in *Input) { in.Impact.CandidateRanking = []string{"coverage_overlap", "vibes"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := good()
			tc.edit(&in)
			if r, err := L1(in); err == nil {
				t.Fatalf("L1 = %+v, want an error", r)
			}
		})
	}
}
