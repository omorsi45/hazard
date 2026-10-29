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
		{"go", []string{"-exec", "xprog"}},
		{"go", []string{"-exec=xprog", "-count=1"}},
		{"go", []string{"-args", "-update"}},
		// After -args the flags belong to the test binary, not to go test.
		{"go", []string{"-args", "-n", "5"}},
		{"python", nil},
		{"python", []string{"-x", "-q", "-v"}},
		{"python", []string{"-vv", "-xs"}},
		{"python", []string{"-p", "no:cacheprovider"}},
		{"python", []string{"-pno:cacheprovider"}},
		{"python", []string{"--maxfail=2"}},
		{"python", []string{"--maxfail", "2", "--tb", "short"}},
		{"python", []string{"-W", "error", "-o", "console_output_style=classic"}},
		{"python", []string{"-rA", "-n", "4"}},
		{"python", []string{"-c", "pytest.ini"}},
		// Shows fixture setup but still runs every test body.
		{"python", []string{"--setup-show"}},
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
		// Arguments that build or print without running any test body.
		{"go", []string{"-c"}, "-c"},
		{"go", []string{"--c"}, "--c"},
		{"go", []string{"-race", "-c"}, "-c"},
		{"go", []string{"-n"}, "-n"},
		{"go", []string{"--n"}, "--n"},
		{"go", []string{"-count=1", "-n"}, "-n"},
		{"go", []string{"-o", "out.test"}, "-o"},
		{"go", []string{"-o=out.test"}, "-o=out.test"},
		{"go", []string{"--o", "out.test"}, "--o"},
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
		// Arguments that collect, list or print without running test bodies.
		{"python", []string{"--collect-only"}, "--collect-only"},
		{"python", []string{"--co"}, "--co"},
		{"python", []string{"-q", "--co"}, "--co"},
		{"python", []string{"--setup-only"}, "--setup-only"},
		{"python", []string{"--setup-plan"}, "--setup-plan"},
		{"python", []string{"--fixtures"}, "--fixtures"},
		{"python", []string{"--funcargs"}, "--funcargs"},
		{"python", []string{"--fixtures-per-test"}, "--fixtures-per-test"},
		{"python", []string{"--markers"}, "--markers"},
		{"python", []string{"-h"}, "-h"},
		{"python", []string{"-xh"}, "-xh"},
		{"python", []string{"--help"}, "--help"},
		{"python", []string{"--version"}, "--version"},
		{"python", []string{"-V"}, "-V"},
		{"python", []string{"-vV"}, "-vV"},
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
