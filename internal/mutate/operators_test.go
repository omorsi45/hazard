package mutate

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
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
	checkMutants(t, src, []string{"const", "ret", "stmt"}, []spot{
		{ctx: "x()", within: "x()", op: "stmt", orig: "x()", mut: "", line: 5},
	})
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

// pkg builds a file whose first declaration starts on line 3.
func pkg(decls string) string { return "package p\n\n" + decls + "\n" }

func TestConstInt(t *testing.T) {
	for _, tc := range []struct {
		lit  string
		muts []string
	}{
		{"5", []string{"6", "4", "0"}},
		{"0", []string{"1", "-1"}},
		{"1", []string{"2", "0"}}, // n-1 and 0 coincide: one mutant
		{"42", []string{"43", "41", "0"}},
		{"123456789012345678901234567890", []string{"123456789012345678901234567891", "123456789012345678901234567889", "0"}},
		{"9223372036854775807", []string{"9223372036854775808", "9223372036854775806", "0"}},
	} {
		t.Run(tc.lit, func(t *testing.T) {
			var spots []spot
			for _, m := range tc.muts {
				spots = append(spots, spot{ctx: "x := " + tc.lit, within: tc.lit, op: "const", orig: tc.lit, mut: m, line: 4})
			}
			checkMutants(t, body("x := "+tc.lit), []string{"const"}, spots)
		})
	}
}

func TestConstUnaryMinus(t *testing.T) {
	checkMutants(t, body("x := -5"), []string{"const"}, []spot{
		{ctx: "-5", within: "5", op: "const", orig: "5", mut: "6", line: 4},
		{ctx: "-5", within: "5", op: "const", orig: "5", mut: "4", line: 4},
		{ctx: "-5", within: "5", op: "const", orig: "5", mut: "0", line: 4},
	})
}

func TestConstIgnoresNonDecimalLiterals(t *testing.T) {
	for _, lit := range []string{"0x10", "0X1F", "0755", "0b11", "0o17", "1_000", "1.5", ".5", "1e3", "2i", "'a'", "00"} {
		t.Run(lit, func(t *testing.T) {
			checkMutants(t, body("x := "+lit), []string{"const"}, nil)
		})
	}
}

func TestConstString(t *testing.T) {
	for _, lit := range []string{
		`"hello"`,
		"`raw`",
		`"a\"b\n"`,
		"\"h\u00e9llo \u2713\"",
		`" "`,
	} {
		t.Run(lit, func(t *testing.T) {
			checkMutants(t, body("x := "+lit), []string{"const"}, []spot{
				{ctx: "x := " + lit, within: lit, op: "const", orig: lit, mut: `""`, line: 4},
			})
		})
	}
	for _, lit := range []string{`""`, "``"} {
		t.Run("empty "+lit, func(t *testing.T) {
			checkMutants(t, body("x := "+lit), []string{"const"}, nil)
		})
	}
}

func TestConstSkipsDeclarationsTagsAndImports(t *testing.T) {
	src := `package p

import "fmt"
import (
	"os"
	_ "embed"
)

type T struct {
	A int ` + "`json:\"a\"`" + `
	B string "plain tag"
}

const (
	name = "n"
	size = 8
	limit = 10
)

const lone = "l"

var buf [size]byte
var arr [4]int
var expr [2 * 8]int
`
	checkMutants(t, src, []string{"const"}, []spot{
		{ctx: "limit = 10", within: "10", op: "const", orig: "10", mut: "11", line: 17},
		{ctx: "limit = 10", within: "10", op: "const", orig: "10", mut: "9", line: 17},
		{ctx: "limit = 10", within: "10", op: "const", orig: "10", mut: "0", line: 17},
	})
}

func TestConstArrayElementsAreMutatedButLengthIsNot(t *testing.T) {
	checkMutants(t, body("var x [4]int\n\ty := [...]int{7}"), []string{"const"}, []spot{
		{ctx: "{7}", within: "7", op: "const", orig: "7", mut: "8", line: 5},
		{ctx: "{7}", within: "7", op: "const", orig: "7", mut: "6", line: 5},
		{ctx: "{7}", within: "7", op: "const", orig: "7", mut: "0", line: 5},
	})
}

func TestConstLocalConstants(t *testing.T) {
	checkMutants(t, body("const s = \"v\"\n\tconst n = 3"), []string{"const"}, []spot{
		{ctx: "n = 3", within: "3", op: "const", orig: "3", mut: "4", line: 5},
		{ctx: "n = 3", within: "3", op: "const", orig: "3", mut: "2", line: 5},
		{ctx: "n = 3", within: "3", op: "const", orig: "3", mut: "0", line: 5},
	})
}

func TestConstRawStringsAndLineNumbers(t *testing.T) {
	src := body("s := `one\ntwo`\n\tn := 3")
	checkMutants(t, src, []string{"const"}, []spot{
		{ctx: "`one\ntwo`", within: "`one\ntwo`", op: "const", orig: "`one\ntwo`", mut: `""`, line: 4},
		{ctx: "n := 3", within: "3", op: "const", orig: "3", mut: "4", line: 6},
		{ctx: "n := 3", within: "3", op: "const", orig: "3", mut: "2", line: 6},
		{ctx: "n := 3", within: "3", op: "const", orig: "3", mut: "0", line: 6},
	})
}

func TestConstRawStringWithCRLF(t *testing.T) {
	// The parser drops carriage returns from a raw string's value; the mutant
	// must still cover the exact bytes of the file.
	src := "package p\r\n\r\nfunc f() {\r\n\ts := `a\r\nb`\r\n\tn := 3\r\n}\r\n"
	checkMutants(t, src, []string{"const"}, []spot{
		{ctx: "`a\r\nb`", within: "`a\r\nb`", op: "const", orig: "`a\r\nb`", mut: `""`, line: 4},
		{ctx: "n := 3", within: "3", op: "const", orig: "3", mut: "4", line: 6},
		{ctx: "n := 3", within: "3", op: "const", orig: "3", mut: "2", line: 6},
		{ctx: "n := 3", within: "3", op: "const", orig: "3", mut: "0", line: 6},
	})
}

func TestParseFilterDropsMutantsThatBreakSyntax(t *testing.T) {
	// "a-0" -> "a--1" is an increment followed by a stray literal: the
	// candidate must be discarded, the valid "a-1" kept.
	checkMutants(t, body("x := a-0"), []string{"const"}, []spot{
		{ctx: "a-0", within: "0", op: "const", orig: "0", mut: "1", line: 4},
	})
}

func TestRetZeroValues(t *testing.T) {
	cases := []struct{ name, typ, zero string }{
		{"bool", "bool", "false"},
		{"string", "string", `""`},
		{"error", "error", "nil"},
		{"pointer", "*T", "nil"},
		{"slice", "[]int", "nil"},
		{"map", "map[string]int", "nil"},
		{"chan", "chan int", "nil"},
		{"receive chan", "<-chan int", "nil"},
		{"func", "func()", "nil"},
		{"empty interface", "interface{}", "nil"},
		{"method interface", "interface{ M() }", "nil"},
		{"any", "any", "nil"},
	}
	for _, n := range []string{"int", "int8", "int16", "int32", "int64", "uint", "uint8", "uint16", "uint32", "uint64", "uintptr", "float32", "float64", "complex64", "complex128", "byte", "rune"} {
		cases = append(cases, struct{ name, typ, zero string }{n, n, "0"})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checkMutants(t, pkg("func f() "+tc.typ+" {\n\treturn x\n}"), []string{"ret"}, []spot{
				{ctx: "return x", within: "x", op: "ret", orig: "x", mut: tc.zero, line: 4},
			})
		})
	}
}

func TestRetMultipleResults(t *testing.T) {
	t.Run("value and error", func(t *testing.T) {
		checkMutants(t, pkg("func f() (int, error) {\n\treturn a, err\n}"), []string{"ret"}, []spot{
			{ctx: "return a, err", within: "a", op: "ret", orig: "a", mut: "0", line: 4},
			{ctx: "return a, err", within: "err", op: "ret", orig: "err", mut: "nil", line: 4},
		})
	})
	t.Run("nil error is already zero", func(t *testing.T) {
		checkMutants(t, pkg("func f() (int, error) {\n\treturn a, nil\n}"), []string{"ret"}, []spot{
			{ctx: "return a, nil", within: "a", op: "ret", orig: "a", mut: "0", line: 4},
		})
	})
	t.Run("zero value is already zero", func(t *testing.T) {
		checkMutants(t, pkg("func f() (int, error) {\n\treturn 0, err\n}"), []string{"ret"}, []spot{
			{ctx: "return 0, err", within: "err", op: "ret", orig: "err", mut: "nil", line: 4},
		})
	})
	t.Run("grouped names", func(t *testing.T) {
		checkMutants(t, pkg("func f() (x, y int, s string) {\n\treturn 1, 2, \"z\"\n}"), []string{"ret"}, []spot{
			{ctx: `return 1, 2, "z"`, within: "1", op: "ret", orig: "1", mut: "0", line: 4},
			{ctx: `return 1, 2, "z"`, within: "2", op: "ret", orig: "2", mut: "0", line: 4},
			{ctx: `return 1, 2, "z"`, within: `"z"`, op: "ret", orig: `"z"`, mut: `""`, line: 4},
		})
	})
	t.Run("tuple call", func(t *testing.T) {
		checkMutants(t, pkg("func f() (int, error) {\n\treturn g()\n}"), []string{"ret"}, nil)
	})
	t.Run("bare return with named results", func(t *testing.T) {
		checkMutants(t, pkg("func f() (n int, err error) {\n\tn = 1\n\treturn\n}"), []string{"ret"}, nil)
	})
	t.Run("explicit return with named results", func(t *testing.T) {
		checkMutants(t, pkg("func f() (n int, err error) {\n\treturn 2, e\n}"), []string{"ret"}, []spot{
			{ctx: "return 2, e", within: "2", op: "ret", orig: "2", mut: "0", line: 4},
			{ctx: "return 2, e", within: "e", nth: 1, op: "ret", orig: "e", mut: "nil", line: 4},
		})
	})
}

func TestRetSkipsIneligibleResultTypes(t *testing.T) {
	for _, tc := range []struct{ name, results, ret string }{
		{"struct", "T", "x"},
		{"empty struct", "struct{}", "x"},
		{"array", "[3]int", "x"},
		{"qualified type", "time.Duration", "x"},
		{"named type", "Kind", "x"},
		{"one ineligible among eligible", "(int, T)", "x, y"},
		{"ineligible first", "(T, error)", "x, y"},
		{"array of pointers", "[N]*int", "x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkMutants(t, pkg("func f() "+tc.results+" {\n\treturn "+tc.ret+"\n}"), []string{"ret"}, nil)
		})
	}
	t.Run("type parameter", func(t *testing.T) {
		checkMutants(t, pkg("func f[T any]() T {\n\tvar z T\n\treturn z\n}"), []string{"ret"}, nil)
	})
	t.Run("no results", func(t *testing.T) {
		checkMutants(t, pkg("func f() {\n\treturn\n}"), []string{"ret"}, nil)
	})
}

func TestRetMethodsAndAlreadyZeroValues(t *testing.T) {
	checkMutants(t, pkg("type S struct{}\n\nfunc (s S) f() int {\n\treturn 1\n}"), []string{"ret"}, []spot{
		{ctx: "return 1", within: "1", op: "ret", orig: "1", mut: "0", line: 6},
	})
	for _, tc := range []struct{ typ, expr string }{
		{"int", "0"},
		{"int", "0x0"},
		{"float64", "0.0"},
		{"bool", "false"},
		{"string", `""`},
		{"string", "``"},
		{"error", "nil"},
		{"error", "(nil)"},
		{"*T", "nil"},
	} {
		t.Run(tc.typ+" "+tc.expr, func(t *testing.T) {
			checkMutants(t, pkg("func f() "+tc.typ+" {\n\treturn "+tc.expr+"\n}"), []string{"ret"}, nil)
		})
	}
}

func TestRetFunctionLiteralsAreScoped(t *testing.T) {
	t.Run("inner returns belong to the literal", func(t *testing.T) {
		src := pkg("func outer() string {\n\tg := func() int {\n\t\treturn 7\n\t}\n\treturn \"s\" + g2()\n}")
		checkMutants(t, src, []string{"ret"}, []spot{
			{ctx: "return 7", within: "7", op: "ret", orig: "7", mut: "0", line: 5},
			{ctx: `return "s" + g2()`, within: `"s" + g2()`, op: "ret", orig: `"s" + g2()`, mut: `""`, line: 7},
		})
	})
	t.Run("ineligible literal inside eligible function", func(t *testing.T) {
		src := pkg("func outer() int {\n\th := func() T {\n\t\treturn T{}\n\t}\n\treturn 1\n}")
		checkMutants(t, src, []string{"ret"}, []spot{
			{ctx: "return 1", within: "1", op: "ret", orig: "1", mut: "0", line: 7},
		})
	})
	t.Run("literal assigned at package level", func(t *testing.T) {
		checkMutants(t, pkg("var g = func() bool {\n\treturn ok\n}"), []string{"ret"}, []spot{
			{ctx: "return ok", within: "ok", op: "ret", orig: "ok", mut: "false", line: 4},
		})
	})
}

func TestRetNestedReturnsAndMultiLineExpressions(t *testing.T) {
	src := pkg("func f() int {\n\tif ok {\n\t\treturn 1\n\t}\n\tswitch {\n\tcase ok:\n\t\treturn 2\n\t}\n\treturn 3\n}")
	checkMutants(t, src, []string{"ret"}, []spot{
		{ctx: "return 1", within: "1", op: "ret", orig: "1", mut: "0", line: 5},
		{ctx: "return 2", within: "2", op: "ret", orig: "2", mut: "0", line: 9},
		{ctx: "return 3", within: "3", op: "ret", orig: "3", mut: "0", line: 11},
	})
	checkMutants(t, pkg("func f() int {\n\treturn a +\n\t\tb\n}"), []string{"ret"}, []spot{
		{ctx: "return a +\n\t\tb", within: "a +\n\t\tb", op: "ret", orig: "a +\n\t\tb", mut: "0", line: 4},
	})
}

func TestConstAndRetAtTheSameOffsetAreOrderedByOperator(t *testing.T) {
	checkMutants(t, pkg("func f() int {\n\treturn 5\n}"), []string{"const", "ret"}, []spot{
		{ctx: "return 5", within: "5", op: "const", orig: "5", mut: "0", line: 4},
		{ctx: "return 5", within: "5", op: "const", orig: "5", mut: "4", line: 4},
		{ctx: "return 5", within: "5", op: "const", orig: "5", mut: "6", line: 4},
		{ctx: "return 5", within: "5", op: "ret", orig: "5", mut: "0", line: 4},
	})
}

func TestStmtDeletesCallsIncDecAndPlainAssignments(t *testing.T) {
	stmts := []string{"x()", "c++", "d--", "a = 1", "a, b = b, a", "m[k] = true", "s.f = c", "*p = 3", "obj.method(a, b)"}
	var spots []spot
	for i, s := range stmts {
		spots = append(spots, spot{ctx: s, within: s, op: "stmt", orig: s, mut: "", line: 4 + i})
	}
	checkMutants(t, body(strings.Join(stmts, "\n\t")), []string{"stmt"}, spots)
}

func TestStmtNeverDeletesOtherStatements(t *testing.T) {
	src := body(`v := 1
	var w int
	c += 1
	c -= 1
	defer x()
	go y()
	ch <- 1
	<-ch
	if ok {
	}
	for {
		break
	}
	switch {
	case ok:
		fallthrough
	default:
	}
L:
	for {
		continue L
	}
M:
	z()
	goto L`)
	checkMutants(t, src, []string{"stmt"}, nil)
}

func TestStmtSameLine(t *testing.T) {
	checkMutants(t, body("x(); y()"), []string{"stmt"}, []spot{
		{ctx: "x(); y()", within: "x()", op: "stmt", orig: "x()", mut: "", line: 4},
		{ctx: "x(); y()", within: "y()", op: "stmt", orig: "y()", mut: "", line: 4},
	})
}

func TestStmtMultiLineAndTrailingComment(t *testing.T) {
	checkMutants(t, body("x(a,\n\t\tb)\n\ty() // note"), []string{"stmt"}, []spot{
		{ctx: "x(a,\n\t\tb)", within: "x(a,\n\t\tb)", op: "stmt", orig: "x(a,\n\t\tb)", mut: "", line: 4},
		{ctx: "y()", within: "y()", op: "stmt", orig: "y()", mut: "", line: 6},
	})
}

func TestStmtKeepsTheLastStatementOfFunctionsWithResults(t *testing.T) {
	t.Run("declaration and literal", func(t *testing.T) {
		src := pkg("func f() int {\n\tx()\n\tpanic(\"no\")\n}\n\nvar g = func() int {\n\ty()\n\tpanic(\"z\")\n}")
		checkMutants(t, src, []string{"stmt"}, []spot{
			{ctx: "x()", within: "x()", op: "stmt", orig: "x()", mut: "", line: 4},
			{ctx: "y()", within: "y()", op: "stmt", orig: "y()", mut: "", line: 9},
		})
	})
	t.Run("assignment as last statement", func(t *testing.T) {
		checkMutants(t, pkg("func f() (n int) {\n\tx()\n\tn = 1\n}"), []string{"stmt"}, []spot{
			{ctx: "x()", within: "x()", op: "stmt", orig: "x()", mut: "", line: 4},
		})
	})
	t.Run("no results", func(t *testing.T) {
		checkMutants(t, pkg("func f() {\n\tx()\n\ty()\n}"), []string{"stmt"}, []spot{
			{ctx: "x()", within: "x()", op: "stmt", orig: "x()", mut: "", line: 4},
			{ctx: "y()", within: "y()", op: "stmt", orig: "y()", mut: "", line: 5},
		})
	})
	t.Run("last statement of a nested block", func(t *testing.T) {
		checkMutants(t, pkg("func f() int {\n\tif ok {\n\t\tx()\n\t}\n\treturn 1\n}"), []string{"stmt"}, []spot{
			{ctx: "x()", within: "x()", op: "stmt", orig: "x()", mut: "", line: 5},
		})
	})
}

func TestStmtInCaseClauses(t *testing.T) {
	src := pkg("func f() {\n\tswitch k {\n\tcase \"a\":\n\t\tx()\n\tdefault:\n\t\ty()\n\t}\n\tselect {\n\tcase <-ch:\n\t\tz()\n\t}\n}")
	checkMutants(t, src, []string{"stmt"}, []spot{
		{ctx: "x()", within: "x()", op: "stmt", orig: "x()", mut: "", line: 6},
		{ctx: "y()", within: "y()", op: "stmt", orig: "y()", mut: "", line: 8},
		{ctx: "z()", within: "z()", op: "stmt", orig: "z()", mut: "", line: 12},
	})
}

// repoRoot returns the repository root, two levels above this package.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	mod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil || !strings.Contains(string(mod), "module github.com/omorsi45/hazard") {
		t.Fatalf("repository root %s has no hazard go.mod (err %v)", root, err)
	}
	return root
}

// TestRepository mutates every non-test Go file under internal/ and checks the
// mutants against the real code: they apply, they still parse, their IDs are
// unique, and a second run gives the same answer.
func TestRepository(t *testing.T) {
	root := repoRoot(t)
	ops := []string{"cmp", "logic", "const", "ret", "stmt"}
	counts := map[string]int{}
	idFile := map[string]string{}
	files, total := 0, 0
	err := filepath.WalkDir(filepath.Join(root, "internal"), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		muts, err := GoMutants(rel, src, ops)
		if err != nil {
			t.Errorf("%s: GoMutants: %v", rel, err)
			return nil
		}
		files++
		again, err := GoMutants(rel, src, ops)
		if err != nil || !reflect.DeepEqual(muts, again) {
			t.Errorf("%s: a second run gives a different answer (err %v)", rel, err)
		}
		for i, m := range muts {
			counts[m.Operator]++
			total++
			if other, dup := idFile[m.ID]; dup {
				t.Errorf("%s: mutant ID %s already used by %s", rel, m.ID, other)
			}
			idFile[m.ID] = rel
			if m.File != rel || m.Line < 1 || m.Length != len(m.Original) {
				t.Errorf("%s: inconsistent mutant %+v", rel, m)
			}
			if m.Original == m.Mutated {
				t.Errorf("%s: mutant %s replaces %q with itself at line %d", rel, m.ID, m.Original, m.Line)
			}
			if i > 0 && muts[i-1].Offset > m.Offset {
				t.Errorf("%s: mutants out of order at %d", rel, i)
			}
			out, err := Apply(src, m)
			if err != nil {
				t.Errorf("%s: Apply %+v: %v", rel, m, err)
				continue
			}
			if _, err := parser.ParseFile(token.NewFileSet(), rel, out, parser.SkipObjectResolution); err != nil {
				t.Errorf("%s: mutant %s (%s %q -> %q at line %d) does not parse: %v", rel, m.ID, m.Operator, m.Original, m.Mutated, m.Line, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files == 0 {
		t.Fatal("no Go files found under internal/")
	}
	for _, op := range ops {
		t.Logf("operator %-5s %6d mutants", op, counts[op])
		if counts[op] == 0 {
			t.Errorf("operator %s produced no mutants over the repository", op)
		}
	}
	t.Logf("total %d mutants in %d files", total, files)
}
