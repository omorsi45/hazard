package mutate

import (
	"fmt"
	"go/parser"
	"go/token"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// hdr and body build a complete Go file whose statements start on line 4.
const hdr = "package p\n\nfunc f(a, b, c, d int, ok bool, k string, m map[string]bool) {\n"

func body(s string) string { return hdr + s + "\n}\n" }

const testFile = "pkg/f.go"

// spot describes one expected mutant. The byte offset is located inside the
// source: the nth occurrence (0 based) of within, inside the unique occurrence
// of ctx.
type spot struct {
	ctx, within string
	nth         int
	op, orig    string
	mut         string
	line        int
}

type view struct {
	Off  int
	Op   string
	Orig string
	Mut  string
	Line int
}

func (v view) String() string {
	return fmt.Sprintf("%s@%d(line %d) %q -> %q", v.Op, v.Off, v.Line, v.Orig, v.Mut)
}

func locate(t *testing.T, src, ctx, within string, nth int) int {
	t.Helper()
	if n := strings.Count(src, ctx); n != 1 {
		t.Fatalf("context %q occurs %d times in the source, want exactly 1", ctx, n)
	}
	base := strings.Index(src, ctx)
	pos, idx := 0, -1
	for i := 0; i <= nth; i++ {
		j := strings.Index(ctx[pos:], within)
		if j < 0 {
			t.Fatalf("occurrence %d of %q not found inside context %q", nth, within, ctx)
		}
		idx = pos + j
		pos = idx + 1
	}
	return base + idx
}

func sortViews(vs []view) {
	slices.SortFunc(vs, func(x, y view) int {
		if x.Off != y.Off {
			return x.Off - y.Off
		}
		if c := strings.Compare(x.Op, y.Op); c != 0 {
			return c
		}
		return strings.Compare(x.Mut, y.Mut)
	})
}

func viewList(vs []view) string {
	if len(vs) == 0 {
		return "  (none)"
	}
	var sb strings.Builder
	for _, v := range vs {
		sb.WriteString("  " + v.String() + "\n")
	}
	return sb.String()
}

// checkMutants asserts that GoMutants returns exactly the described spots, in
// sorted order, and that every returned mutant is internally consistent,
// applies cleanly and still parses.
func checkMutants(t *testing.T, src string, ops []string, spots []spot) []Mutant {
	t.Helper()
	got, err := GoMutants(testFile, []byte(src), ops)
	if err != nil {
		t.Fatalf("GoMutants: %v", err)
	}
	var want, have []view
	for _, s := range spots {
		want = append(want, view{locate(t, src, s.ctx, s.within, s.nth), s.op, s.orig, s.mut, s.line})
	}
	sortViews(want)
	for _, m := range got {
		have = append(have, view{m.Offset, m.Operator, m.Original, m.Mutated, m.Line})
	}
	if !reflect.DeepEqual(have, want) {
		t.Fatalf("mutants differ\ngot:\n%swant:\n%s", viewList(have), viewList(want))
	}
	for _, m := range got {
		if m.File != testFile {
			t.Errorf("%v: File = %q, want %q", m, m.File, testFile)
		}
		if m.ID != makeID(testFile, m.Offset, m.Operator, m.Mutated) {
			t.Errorf("%v: ID = %q does not match its components", m, m.ID)
		}
		if m.Length != len(m.Original) {
			t.Errorf("%v: Length = %d, want %d", m, m.Length, len(m.Original))
		}
		if src[m.Offset:m.Offset+m.Length] != m.Original {
			t.Errorf("%v: Original %q is not the source at Offset", m, m.Original)
		}
		out, err := Apply([]byte(src), m)
		if err != nil {
			t.Errorf("%v: Apply: %v", m, err)
			continue
		}
		if _, err := parser.ParseFile(token.NewFileSet(), testFile, out, 0); err != nil {
			t.Errorf("%v: mutated source does not parse: %v", m, err)
		}
	}
	return got
}

func TestCmpOperators(t *testing.T) {
	for _, tc := range []struct{ expr, orig, mut string }{
		{"a > b", ">", ">="},
		{"a >= b", ">=", ">"},
		{"a < b", "<", "<="},
		{"a <= b", "<=", "<"},
		{"a == b", "==", "!="},
		{"a != b", "!=", "=="},
	} {
		t.Run(tc.orig, func(t *testing.T) {
			src := body("x := " + tc.expr)
			checkMutants(t, src, []string{"cmp"}, []spot{
				{ctx: tc.expr, within: tc.orig, op: "cmp", orig: tc.orig, mut: tc.mut, line: 4},
			})
		})
	}
}

func TestCmpMultipleExpressions(t *testing.T) {
	src := body("x := a < b && b <= c\n\ty := (a >= d) == (c != d)")
	checkMutants(t, src, []string{"cmp"}, []spot{
		{ctx: "a < b", within: "<", op: "cmp", orig: "<", mut: "<=", line: 4},
		{ctx: "b <= c", within: "<=", op: "cmp", orig: "<=", mut: "<", line: 4},
		{ctx: "a >= d", within: ">=", op: "cmp", orig: ">=", mut: ">", line: 5},
		{ctx: ") == (", within: "==", op: "cmp", orig: "==", mut: "!=", line: 5},
		{ctx: "c != d", within: "!=", op: "cmp", orig: "!=", mut: "==", line: 5},
	})
}

func TestCmpOffsetsAreBytes(t *testing.T) {
	src := body("// h\u00e9llo \u2713 caf\u00e9\n\tx := a == b")
	checkMutants(t, src, []string{"cmp"}, []spot{
		{ctx: "a == b", within: "==", op: "cmp", orig: "==", mut: "!=", line: 5},
	})
}

func TestCmpAndLogicIgnoreOtherOperators(t *testing.T) {
	src := body("x := a << 1\n\ty := b >> 2\n\tc <<= 1\n\tx, y = y, x\n\tc += 1\n\tch <- 1\n\tz := <-ch\n\tw := a &^ b\n\tv := a & b | c\n\tu := f[int](a)\n\ts := []int{1}")
	checkMutants(t, src, []string{"cmp", "logic"}, nil)
}

func TestLogicBinary(t *testing.T) {
	checkMutants(t, body("x := p && q"), []string{"logic"}, []spot{
		{ctx: "p && q", within: "&&", op: "logic", orig: "&&", mut: "||", line: 4},
	})
	checkMutants(t, body("x := p || q"), []string{"logic"}, []spot{
		{ctx: "p || q", within: "||", op: "logic", orig: "||", mut: "&&", line: 4},
	})
	checkMutants(t, body("x := p && q && r"), []string{"logic"}, []spot{
		{ctx: "p && q && r", within: "&&", nth: 0, op: "logic", orig: "&&", mut: "||", line: 4},
		{ctx: "p && q && r", within: "&&", nth: 1, op: "logic", orig: "&&", mut: "||", line: 4},
	})
}

func TestLogicNotRemoval(t *testing.T) {
	checkMutants(t, body("x := !p"), []string{"logic"}, []spot{
		{ctx: "!p", within: "!", op: "logic", orig: "!", mut: "", line: 4},
	})
	checkMutants(t, body("x := !!p"), []string{"logic"}, []spot{
		{ctx: "!!p", within: "!", nth: 0, op: "logic", orig: "!", mut: "", line: 4},
		{ctx: "!!p", within: "!", nth: 1, op: "logic", orig: "!", mut: "", line: 4},
	})
	checkMutants(t, body("x := !(p && q)"), []string{"logic"}, []spot{
		{ctx: "!(p && q)", within: "!", op: "logic", orig: "!", mut: "", line: 4},
		{ctx: "!(p && q)", within: "&&", op: "logic", orig: "&&", mut: "||", line: 4},
	})
	// "!=" is a comparison, not a negation.
	checkMutants(t, body("x := p != q"), []string{"logic"}, nil)
}

func TestLogicIfNegation(t *testing.T) {
	t.Run("plain condition", func(t *testing.T) {
		checkMutants(t, body("if a > b {\n\t\tx()\n\t}"), []string{"logic"}, []spot{
			{ctx: "a > b", within: "a > b", op: "logic", orig: "a > b", mut: "!(a > b)", line: 4},
		})
	})
	t.Run("already negated", func(t *testing.T) {
		checkMutants(t, body("if !ok {\n\t\tx()\n\t}"), []string{"logic"}, []spot{
			{ctx: "if !ok {", within: "!", op: "logic", orig: "!", mut: "", line: 4},
		})
	})
	t.Run("negation in parentheses", func(t *testing.T) {
		checkMutants(t, body("if (!ok) {\n\t\tx()\n\t}"), []string{"logic"}, []spot{
			{ctx: "if (!ok) {", within: "!", op: "logic", orig: "!", mut: "", line: 4},
		})
	})
	t.Run("negated group", func(t *testing.T) {
		checkMutants(t, body("if !(p && q) {\n\t\tx()\n\t}"), []string{"logic"}, []spot{
			{ctx: "if !(p && q) {", within: "!", op: "logic", orig: "!", mut: "", line: 4},
			{ctx: "if !(p && q) {", within: "&&", op: "logic", orig: "&&", mut: "||", line: 4},
		})
	})
	t.Run("else if chain", func(t *testing.T) {
		checkMutants(t, body("if p {\n\t\tx()\n\t} else if q {\n\t\ty()\n\t}"), []string{"logic"}, []spot{
			{ctx: "if p {", within: "p", op: "logic", orig: "p", mut: "!(p)", line: 4},
			{ctx: "else if q {", within: "q", op: "logic", orig: "q", mut: "!(q)", line: 6},
		})
	})
	t.Run("init statement", func(t *testing.T) {
		checkMutants(t, body("if v, ok := m[k]; ok {\n\t\tx(v)\n\t}"), []string{"logic"}, []spot{
			{ctx: "; ok {", within: "ok", op: "logic", orig: "ok", mut: "!(ok)", line: 4},
		})
	})
	t.Run("multi-line condition", func(t *testing.T) {
		checkMutants(t, body("if p &&\n\t\tq {\n\t\tx()\n\t}"), []string{"logic"}, []spot{
			{ctx: "p &&\n\t\tq", within: "p", op: "logic", orig: "p &&\n\t\tq", mut: "!(p &&\n\t\tq)", line: 4},
			{ctx: "p &&\n\t\tq", within: "&&", op: "logic", orig: "&&", mut: "||", line: 4},
		})
	})
	t.Run("only if conditions", func(t *testing.T) {
		src := body("for p && q {\n\t\tx()\n\t}\n\tswitch r {\n\tcase s:\n\t}")
		checkMutants(t, src, []string{"logic"}, []spot{
			{ctx: "for p && q {", within: "&&", op: "logic", orig: "&&", mut: "||", line: 4},
		})
	})
	t.Run("same offset ordered by replacement", func(t *testing.T) {
		checkMutants(t, body("if !p && q {\n\t\tx()\n\t}"), []string{"logic"}, []spot{
			{ctx: "if !p && q {", within: "!", op: "logic", orig: "!", mut: "", line: 4},
			{ctx: "if !p && q {", within: "!p && q", op: "logic", orig: "!p && q", mut: "!(!p && q)", line: 4},
			{ctx: "if !p && q {", within: "&&", op: "logic", orig: "&&", mut: "||", line: 4},
		})
	})
}

func TestOperatorSelection(t *testing.T) {
	src := body("if a > b && p {\n\t\tx()\n\t}")
	cmpSpot := spot{ctx: "a > b", within: ">", op: "cmp", orig: ">", mut: ">=", line: 4}
	andSpot := spot{ctx: "&& p", within: "&&", op: "logic", orig: "&&", mut: "||", line: 4}
	wrapSpot := spot{ctx: "a > b && p", within: "a", op: "logic", orig: "a > b && p", mut: "!(a > b && p)", line: 4}

	checkMutants(t, src, []string{"cmp"}, []spot{cmpSpot})
	checkMutants(t, src, []string{"logic"}, []spot{wrapSpot, andSpot})
	checkMutants(t, src, []string{"cmp", "logic"}, []spot{wrapSpot, cmpSpot, andSpot})
	checkMutants(t, src, []string{"logic", "cmp"}, []spot{wrapSpot, cmpSpot, andSpot})
	checkMutants(t, src, []string{"const", "ret", "stmt"}, nil)
	checkMutants(t, src, nil, nil)
	checkMutants(t, src, []string{}, nil)
	// cfg is produced by ConfigMutants, never by GoMutants.
	checkMutants(t, src, []string{"cfg"}, nil)
}

func TestGoMutantsRejectsUnknownOperator(t *testing.T) {
	got, err := GoMutants(testFile, []byte(body("x := a > b")), []string{"cmp", "bogus"})
	if err == nil || !strings.Contains(err.Error(), `"bogus"`) {
		t.Fatalf("err = %v, want an error naming the operator", err)
	}
	if got != nil {
		t.Fatalf("got %v alongside an error", got)
	}
}

func TestGoMutantsRejectsUnparsableSource(t *testing.T) {
	got, err := GoMutants(testFile, []byte("package p\n\nfunc f( {\n"), []string{"cmp"})
	if err == nil {
		t.Fatalf("got %v, want a parse error", got)
	}
	if got != nil {
		t.Fatalf("got %v alongside an error", got)
	}
}

func TestGoMutantsSkipsTestFiles(t *testing.T) {
	src := []byte(body("x := a > b"))
	for _, name := range []string{"pkg/f_test.go", "f_test.go", `pkg\f_test.go`} {
		got, err := GoMutants(name, src, []string{"cmp", "logic", "const", "ret", "stmt"})
		if err != nil || got != nil {
			t.Errorf("GoMutants(%q) = %v, %v, want no mutants", name, got, err)
		}
	}
	got, err := GoMutants("pkg/test.go", src, []string{"cmp"})
	if err != nil || len(got) != 1 {
		t.Errorf("GoMutants(pkg/test.go) = %v, %v, want one mutant", got, err)
	}
}

func TestGoMutantsSkipsGeneratedFiles(t *testing.T) {
	generated := "// Code generated by stringer; DO NOT EDIT.\n\n" + body("x := a > b")
	got, err := GoMutants(testFile, []byte(generated), []string{"cmp", "logic", "const", "ret", "stmt"})
	if err != nil || got != nil {
		t.Errorf("generated file: got %v, %v, want no mutants", got, err)
	}
	withLicense := "// Copyright 2026 Example.\n\n// Code generated by protoc-gen-go. DO NOT EDIT.\n// source: x.proto\n\n" + body("x := a > b")
	got, err = GoMutants(testFile, []byte(withLicense), []string{"cmp"})
	if err != nil || got != nil {
		t.Errorf("generated file with license header: got %v, %v, want no mutants", got, err)
	}
	// The marker only counts before the package clause.
	late := body("// Code generated by hand. DO NOT EDIT.\n\tx := a > b")
	got, err = GoMutants(testFile, []byte(late), []string{"cmp"})
	if err != nil || len(got) != 1 {
		t.Errorf("marker after the package clause: got %v, %v, want one mutant", got, err)
	}
}

func TestGoMutantsDeterministic(t *testing.T) {
	src := []byte(body("if a > b && p || !q {\n\t\tx := a == b\n\t}\n\ty := a < b"))
	ops := []string{"cmp", "logic"}
	first, err := GoMutants(testFile, src, ops)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) == 0 {
		t.Fatal("expected mutants")
	}
	for i := 0; i < 5; i++ {
		again, err := GoMutants(testFile, src, ops)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(first, again) {
			t.Fatalf("run %d differs from the first run", i)
		}
	}
	seen := map[string]bool{}
	for i, m := range first {
		if seen[m.ID] {
			t.Errorf("duplicate ID %s", m.ID)
		}
		seen[m.ID] = true
		if i > 0 {
			p := first[i-1]
			if p.Offset > m.Offset || (p.Offset == m.Offset && (p.Operator > m.Operator || (p.Operator == m.Operator && p.Mutated > m.Mutated))) {
				t.Errorf("mutants %d and %d are out of order: %v then %v", i-1, i, p, m)
			}
		}
	}
}
