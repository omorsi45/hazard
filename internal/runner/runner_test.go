package runner

import (
	"errors"
	"strings"
	"testing"
)

func TestValidatePassthroughAccepts(t *testing.T) {
	for _, tc := range []struct {
		lang string
		args []string
	}{
		{"go", nil},
		{"go", []string{"-race"}},
		{"go", []string{"-count=1"}},
		{"go", []string{"-count", "2"}},
		{"go", []string{"-v", "-timeout", "30s"}},
		{"go", []string{"-timeout=30s", "--race"}},
		{"go", []string{"-tags", "integration", "-p", "4"}},
		{"go", []string{"-ldflags", "-s -w"}},
		{"go", []string{"-args", "-update"}},
		{"python", nil},
		{"python", []string{"-x", "-q", "-v"}},
		{"python", []string{"-vv", "-xs"}},
		{"python", []string{"-p", "no:cacheprovider"}},
		{"python", []string{"-pno:cacheprovider"}},
		{"python", []string{"--maxfail=2"}},
		{"python", []string{"--maxfail", "2", "--tb", "short"}},
		{"python", []string{"-W", "error", "-o", "console_output_style=classic"}},
		{"python", []string{"-rA", "-n", "4"}},
	} {
		if err := ValidatePassthrough(tc.lang, tc.args); err != nil {
			t.Errorf("%s %q rejected: %v", tc.lang, tc.args, err)
		}
	}
}

func TestValidatePassthroughRejectsNarrowing(t *testing.T) {
	for _, tc := range []struct {
		lang string
		args []string
		bad  string
	}{
		{"go", []string{"-run", "X"}, "-run"},
		{"go", []string{"-run=X"}, "-run=X"},
		{"go", []string{"--run=X"}, "--run=X"},
		{"go", []string{"-skip", "X"}, "-skip"},
		{"go", []string{"-skip=X"}, "-skip=X"},
		{"go", []string{"-list", "."}, "-list"},
		{"go", []string{"-list=."}, "-list=."},
		{"go", []string{"-bench", "."}, "-bench"},
		{"go", []string{"-bench=."}, "-bench=."},
		{"go", []string{"-test.run=X"}, "-test.run=X"},
		{"go", []string{"-test.skip=X"}, "-test.skip=X"},
		{"go", []string{"-args", "-test.run=X"}, "-test.run=X"},
		{"go", []string{"-race", "./pkg"}, "./pkg"},
		{"go", []string{"example.com/m/pkg"}, "example.com/m/pkg"},
		{"go", []string{"-count", "1", "./..."}, "./..."},
		{"go", []string{"--", "./pkg"}, "./pkg"},
		{"go", []string{""}, `""`},
		{"python", []string{"-k", "name"}, "-k"},
		{"python", []string{"-kname"}, "-kname"},
		{"python", []string{"-xk", "name"}, "-xk"},
		{"python", []string{"-m", "slow"}, "-m"},
		{"python", []string{"-mslow"}, "-mslow"},
		{"python", []string{"-vm", "slow"}, "-vm"},
		{"python", []string{"--deselect", "tests/a.py::t"}, "--deselect"},
		{"python", []string{"--deselect=tests/a.py::t"}, "--deselect=tests/a.py::t"},
		{"python", []string{"--lf"}, "--lf"},
		{"python", []string{"--ff"}, "--ff"},
		{"python", []string{"--last-failed"}, "--last-failed"},
		{"python", []string{"--failed-first"}, "--failed-first"},
		{"python", []string{"-x", "tests/test_core.py"}, "tests/test_core.py"},
		{"python", []string{"tests/test_core.py::test_add"}, "tests/test_core.py::test_add"},
		{"python", []string{"--maxfail=2", "tests"}, "tests"},
		{"python", []string{"--", "tests"}, "tests"},
		{"python", []string{"@args.txt"}, "@args.txt"},
	} {
		err := ValidatePassthrough(tc.lang, tc.args)
		if !errors.Is(err, ErrPassthroughNarrows) {
			t.Errorf("%s %q: got %v, want ErrPassthroughNarrows", tc.lang, tc.args, err)
			continue
		}
		if !strings.Contains(err.Error(), tc.bad) {
			t.Errorf("%s %q: error %q does not name offending argument %s", tc.lang, tc.args, err, tc.bad)
		}
	}
}

func TestValidatePassthroughUnsupportedLanguage(t *testing.T) {
	err := ValidatePassthrough("ruby", []string{"-x"})
	if err == nil || errors.Is(err, ErrPassthroughNarrows) {
		t.Fatalf("unsupported language: got %v, want a distinct error", err)
	}
}
