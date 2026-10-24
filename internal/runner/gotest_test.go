package runner

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func goTarget(pkg, name string) Target {
	return Target{ID: "go:" + pkg + ":" + name, Language: "go", Package: pkg, Name: name}
}

func goRequest(targets ...Target) Request {
	return Request{RepoDir: "/repo", Targets: targets}
}

func invocationAt(t *testing.T, got []Invocation, i int) Invocation {
	t.Helper()
	if i >= len(got) {
		t.Fatalf("want at least %d invocations, got %+v", i+1, got)
	}
	return got[i]
}

func argAfter(t *testing.T, argv []string, flag string) string {
	t.Helper()
	i := slices.Index(argv, flag)
	if i < 0 || i+1 >= len(argv) {
		t.Fatalf("%q not found in %q", flag, argv)
	}
	return argv[i+1]
}

func TestGoInvocationsGroupsByPackageSortedAndDeduplicated(t *testing.T) {
	req := goRequest(
		goTarget("example.com/m/b", "TestZ"),
		goTarget("example.com/m/a", "TestB"),
		goTarget("example.com/m/a", "TestA"),
		goTarget("example.com/m/b", "TestZ"),
		goTarget("example.com/m/a", "TestA"),
	)
	got, widened, err := GoInvocations(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(widened) != 0 {
		t.Fatalf("widened: %q", widened)
	}
	want := []Invocation{
		{Argv: []string{"go", "test", "-json", "-run", "^(TestA|TestB)$", "example.com/m/a"}, Dir: "/repo", Package: "example.com/m/a"},
		{Argv: []string{"go", "test", "-json", "-run", "^(TestZ)$", "example.com/m/b"}, Dir: "/repo", Package: "example.com/m/b"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("invocations:\n got %+v\nwant %+v", got, want)
	}
}

func TestGoInvocationsIsDeterministic(t *testing.T) {
	a := goRequest(goTarget("p/b", "TestB"), goTarget("p/a", "TestA"), goTarget("p/b", "TestA"))
	b := goRequest(goTarget("p/b", "TestA"), goTarget("p/a", "TestA"), goTarget("p/b", "TestB"))
	ga, _, err := GoInvocations(a)
	if err != nil {
		t.Fatal(err)
	}
	gb, _, err := GoInvocations(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(ga) != 2 || !reflect.DeepEqual(ga, gb) {
		t.Fatalf("target order changed the plan:\n%+v\n%+v", ga, gb)
	}
}

func TestGoInvocationsAnchorsAndQuotesNames(t *testing.T) {
	got, _, err := GoInvocations(goRequest(goTarget("p/a", "TestFoo"), goTarget("p/a", "TestA.B+C")))
	if err != nil {
		t.Fatal(err)
	}
	pattern := argAfter(t, invocationAt(t, got, 0).Argv, "-run")
	if pattern != `^(TestA\.B\+C|TestFoo)$` {
		t.Fatalf("pattern %q is not anchored and quoted", pattern)
	}
}

func TestGoInvocationsSubtestWidensToParent(t *testing.T) {
	got, widened, err := GoInvocations(goRequest(
		goTarget("p/a", "TestParent/sub_name"),
		goTarget("p/a", "TestParent"),
		goTarget("p/a", "TestOther/x/y"),
	))
	if err != nil {
		t.Fatal(err)
	}
	if len(widened) != 0 {
		t.Fatalf("a subtest widening to its parent is not a widened package: %q", widened)
	}
	if pattern := argAfter(t, invocationAt(t, got, 0).Argv, "-run"); pattern != "^(TestOther|TestParent)$" {
		t.Fatalf("pattern %q", pattern)
	}
}

// longNameTarget returns a name whose length is exactly n.
func longNameTarget(pkg string, n int) Target {
	return goTarget(pkg, "Test"+strings.Repeat("a", n-len("Test")))
}

func TestGoInvocationsPatternLengthBoundary(t *testing.T) {
	// "^(" + a + "|" + b + ")$" is len(a)+len(b)+5 characters.
	for _, tc := range []struct {
		name        string
		second      int
		wantLen     int
		wantWidened bool
	}{
		{"exactly 8000 characters stays anchored", 3995, 8000, false},
		{"8001 characters runs the whole package", 3996, 8001, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, widened, err := GoInvocations(goRequest(
				longNameTarget("p/big", 4000),
				longNameTarget("p/big", tc.second),
				goTarget("p/small", "TestOne"),
			))
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 2 {
				t.Fatalf("invocations: %+v", got)
			}
			big, small := got[0], got[1]
			if big.Package != "p/big" || small.Package != "p/small" {
				t.Fatalf("order: %q, %q", big.Package, small.Package)
			}
			if pattern := argAfter(t, small.Argv, "-run"); pattern != "^(TestOne)$" {
				t.Fatalf("a short package must stay narrowed: %q", pattern)
			}
			if !tc.wantWidened {
				if n := len(argAfter(t, big.Argv, "-run")); n != tc.wantLen {
					t.Fatalf("pattern length %d, want %d", n, tc.wantLen)
				}
				if big.Whole || len(widened) != 0 {
					t.Fatalf("package widened at the limit: %+v %q", big, widened)
				}
				return
			}
			if slices.Contains(big.Argv, "-run") {
				t.Fatalf("widened package still has -run: %q", big.Argv)
			}
			if !big.Whole || !reflect.DeepEqual(widened, []string{"p/big"}) {
				t.Fatalf("whole=%v widened=%q", big.Whole, widened)
			}
			wantArgv := []string{"go", "test", "-json", "p/big"}
			if !reflect.DeepEqual(big.Argv, wantArgv) {
				t.Fatalf("argv %q, want %q", big.Argv, wantArgv)
			}
			if small.Whole {
				t.Fatal("unrelated package marked whole")
			}
		})
	}
}

func TestGoInvocationsWidenedPackagesAreSorted(t *testing.T) {
	_, widened, err := GoInvocations(goRequest(
		longNameTarget("p/z", 5000), longNameTarget("p/z", 4999),
		longNameTarget("p/a", 5000), longNameTarget("p/a", 4999),
	))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(widened, []string{"p/a", "p/z"}) {
		t.Fatalf("widened %q", widened)
	}
}

func TestGoInvocationsFullSuite(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  Request
		want []string
	}{
		{"default pattern", Request{RepoDir: "/repo", Full: true}, []string{"go", "test", "-json", "./..."}},
		{"custom patterns", Request{RepoDir: "/repo", Full: true, GoPackages: []string{"./a/...", "./b"}}, []string{"go", "test", "-json", "./a/...", "./b"}},
		{"targets are ignored", Request{RepoDir: "/repo", Full: true, Targets: []Target{goTarget("p/a", "TestA")}}, []string{"go", "test", "-json", "./..."}},
		{"tags and passthrough precede packages", Request{RepoDir: "/repo", Full: true, GoTags: []string{"integration", "slow"}, Passthrough: []string{"-race", "-count=1"}}, []string{"go", "test", "-json", "-tags=integration,slow", "-race", "-count=1", "./..."}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, widened, err := GoInvocations(tc.req)
			if err != nil {
				t.Fatal(err)
			}
			want := []Invocation{{Argv: tc.want, Dir: "/repo", Whole: true}}
			if !reflect.DeepEqual(got, want) || len(widened) != 0 {
				t.Fatalf("got %+v widened %q, want %+v", got, widened, want)
			}
		})
	}
}

func TestGoInvocationsPreserveTagsAndPassthroughBeforePackage(t *testing.T) {
	req := goRequest(goTarget("p/a", "TestA"))
	req.GoTags = []string{"integration"}
	req.Passthrough = []string{"-race", "-count=2", "-v", "-timeout", "30s"}
	got, _, err := GoInvocations(req)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"go", "test", "-json", "-tags=integration", "-race", "-count=2", "-v", "-timeout", "30s", "-run", "^(TestA)$", "p/a"}
	if argv := invocationAt(t, got, 0).Argv; !reflect.DeepEqual(argv, want) {
		t.Fatalf("argv %q, want %q", argv, want)
	}
}

func TestGoInvocationsRejectsInvalidRequests(t *testing.T) {
	ok := goTarget("p/a", "TestA")
	for _, tc := range []struct {
		name string
		req  Request
	}{
		{"no targets in a narrowed run", goRequest()},
		{"empty package", goRequest(Target{Language: "go", Name: "TestA"})},
		{"empty name", goRequest(Target{Language: "go", Package: "p/a"})},
		{"subtest without parent", goRequest(goTarget("p/a", "/sub"))},
		{"package that looks like a flag", goRequest(goTarget("-exec=evil", "TestA"))},
		{"package with whitespace", goRequest(goTarget("p/a b", "TestA"))},
		{"python target in a go run", goRequest(Target{Language: "python", Package: "p/a", Name: "TestA"})},
		{"empty tag", Request{RepoDir: "/repo", Targets: []Target{ok}, GoTags: []string{""}}},
		{"tag with comma", Request{RepoDir: "/repo", Targets: []Target{ok}, GoTags: []string{"a,b"}}},
		{"package pattern that looks like a flag", Request{RepoDir: "/repo", Full: true, GoPackages: []string{"-race"}}},
		{"empty package pattern", Request{RepoDir: "/repo", Full: true, GoPackages: []string{""}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, widened, err := GoInvocations(tc.req)
			if err == nil {
				t.Fatalf("accepted: %+v widened %q", got, widened)
			}
			if errors.Is(err, ErrPassthroughNarrows) {
				t.Fatalf("wrong error: %v", err)
			}
		})
	}
}

func TestGoInvocationsRejectsNarrowingPassthrough(t *testing.T) {
	for _, full := range []bool{false, true} {
		req := goRequest(goTarget("p/a", "TestA"))
		req.Full = full
		req.Passthrough = []string{"-run", "X"}
		if _, _, err := GoInvocations(req); !errors.Is(err, ErrPassthroughNarrows) {
			t.Errorf("full=%v: got %v, want ErrPassthroughNarrows", full, err)
		}
	}
}
