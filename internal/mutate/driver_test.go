package mutate

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/omorsi45/hazard/internal/graph"
)

func testGraph() *graph.Graph {
	return &graph.Graph{Packages: map[string]*graph.Package{
		"m/calc":     {ID: "m/calc", Language: "go", Dir: "calc", Files: []string{"calc/calc.go"}, TestFiles: []string{"calc/calc_test.go"}},
		"m/app":      {ID: "m/app", Language: "go", Dir: "app", Files: []string{"app/app.go"}, Imports: []string{"m/calc"}},
		"m/far":      {ID: "m/far", Language: "go", Dir: "far", Files: []string{"far/far.go"}, Imports: []string{"m/app"}},
		"m/cfgcheck": {ID: "m/cfgcheck", Language: "go", Dir: "cfgcheck", Files: []string{"cfgcheck/cfgcheck.go"}},
	}}
}

func TestClassifyKillersGoFile(t *testing.T) {
	killers := []string{"go:m/app:TestRun", "go:m/calc:TestMax", "go:m/cfgcheck:TestLimit", "go:m/far:TestFar"}
	unreachable, beyond := classifyKillers(testGraph(), "calc/calc.go", killers, 1)
	if want := []string{"go:m/cfgcheck:TestLimit"}; !reflect.DeepEqual(unreachable, want) {
		t.Errorf("unreachable = %v, want %v", unreachable, want)
	}
	if want := []string{"go:m/far:TestFar"}; !reflect.DeepEqual(beyond, want) {
		t.Errorf("beyond = %v, want %v (2 reverse hops with MaxHops 1)", beyond, want)
	}
	// MaxHops 2 brings the far package inside the bound; hops == MaxHops is within it.
	_, beyond = classifyKillers(testGraph(), "calc/calc.go", killers, 2)
	if len(beyond) != 0 {
		t.Errorf("beyond with MaxHops 2 = %v, want none", beyond)
	}
}

func TestClassifyKillersNonGoFileIsAllUnreachable(t *testing.T) {
	killers := []string{"go:m/app:TestRun", "go:m/calc:TestMax"}
	unreachable, beyond := classifyKillers(testGraph(), "config/limits.yaml", killers, 3)
	if !reflect.DeepEqual(unreachable, killers) || len(beyond) != 0 {
		t.Errorf("unreachable=%v beyond=%v, want every killer unreachable", unreachable, beyond)
	}
}

func TestClassifyKillersUnknownPackageIsUnreachable(t *testing.T) {
	killers := []string{"go:m/app:TestRun"}
	unreachable, _ := classifyKillers(testGraph(), "elsewhere/x.go", killers, 3)
	if !reflect.DeepEqual(unreachable, killers) {
		t.Errorf("unreachable = %v, want %v", unreachable, killers)
	}
}

func TestCandidateMutantsSelection(t *testing.T) {
	files := map[string]string{
		"calc/calc.go":            "package calc\n\nfunc Max(a, b int) int {\n\tif a > b {\n\t\treturn a\n\t}\n\treturn b\n}\n",
		"calc/calc_test.go":       "package calc\n\nfunc helper(a, b int) bool { return a > b }\n",
		"config/limits.yaml":      "limit: 10\nname: abc\n",
		"README.md":               "limit: 5\n",
		"calc/testdata/broken.go": "package broken\nfunc (",
		"vendor/x/x.go":           "package x\nfunc F(a, b int) bool { return a > b }\n",
	}
	tracked := []string{"README.md", "calc/calc.go", "calc/calc_test.go", "calc/testdata/broken.go", "config/limits.yaml", "vendor/x/x.go"}
	read := func(rel string) ([]byte, error) {
		s, ok := files[rel]
		if !ok {
			return nil, errors.New("missing " + rel)
		}
		return []byte(s), nil
	}
	ids := func(ms []Mutant) (files []string, ops map[string]int) {
		ops = map[string]int{}
		seen := map[string]bool{}
		for _, m := range ms {
			if !seen[m.File] {
				seen[m.File] = true
				files = append(files, m.File)
			}
			ops[m.Operator]++
		}
		return files, ops
	}

	got, err := candidateMutants(tracked, []string{"**/*.go", "config/*.yaml", "*.md"}, []string{"cmp", "cfg"}, read)
	if err != nil {
		t.Fatal(err)
	}
	fs, ops := ids(got)
	if len(fs) != 2 {
		t.Fatalf("mutated files = %v, want calc/calc.go and config/limits.yaml only (no tests, testdata, vendor, markdown)", fs)
	}
	if ops["cmp"] == 0 || ops["cfg"] == 0 {
		t.Errorf("operator counts = %v, want cmp and cfg mutants", ops)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].ID >= got[i].ID {
			t.Fatalf("candidates are not ordered by ID at %d: %s then %s", i, got[i-1].ID, got[i].ID)
		}
	}

	// cfg not selected: the YAML file contributes nothing.
	got, err = candidateMutants(tracked, []string{"**/*.go", "config/*.yaml"}, []string{"cmp"}, read)
	if err != nil {
		t.Fatal(err)
	}
	if fs, ops = ids(got); len(fs) != 1 || fs[0] != "calc/calc.go" || ops["cfg"] != 0 {
		t.Errorf("cmp only: files=%v ops=%v", fs, ops)
	}

	// The glob restricts the files.
	got, err = candidateMutants(tracked, []string{"config/*.yaml"}, []string{"cmp", "cfg"}, read)
	if err != nil {
		t.Fatal(err)
	}
	if fs, _ = ids(got); len(fs) != 1 || fs[0] != "config/limits.yaml" {
		t.Errorf("config glob: files=%v", fs)
	}
}

func TestCandidateMutantsErrors(t *testing.T) {
	read := func(string) ([]byte, error) { return []byte("package p\nfunc ("), nil }
	if _, err := candidateMutants([]string{"a.go"}, []string{"*.go"}, nil, read); err == nil || !strings.Contains(err.Error(), "operator") {
		t.Errorf("empty operators: err = %v, want an error naming the operators", err)
	}
	if _, err := candidateMutants([]string{"a.go"}, []string{"*.go"}, []string{"cmp"}, read); err == nil {
		t.Errorf("an unparsable matched Go file must be an error, not silently skipped")
	}
	failing := func(string) ([]byte, error) { return nil, errors.New("boom") }
	if _, err := candidateMutants([]string{"a.go"}, []string{"*.go"}, []string{"cmp"}, failing); err == nil {
		t.Errorf("a read failure must be an error")
	}
}

func TestLimitCandidates(t *testing.T) {
	ms := []Mutant{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	if got := limitCandidates(ms, 2); len(got) != 2 || got[1].ID != "b" {
		t.Errorf("limit 2 = %v", got)
	}
	for _, n := range []int{0, -1, 3, 10} {
		if got := limitCandidates(ms, n); len(got) != 3 {
			t.Errorf("limit %d kept %d, want all 3", n, len(got))
		}
	}
}
