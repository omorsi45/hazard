package mutate

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func TestApply(t *testing.T) {
	tests := []struct {
		name string
		src  string
		m    Mutant
		want string
	}{
		{"replace", "a > b", Mutant{Offset: 2, Length: 1, Original: ">", Mutated: ">="}, "a >= b"},
		{"delete", "x++\ny()\n", Mutant{Offset: 0, Length: 3, Original: "x++", Mutated: ""}, "\ny()\n"},
		{"wrap", "if a && b {", Mutant{Offset: 3, Length: 6, Original: "a && b", Mutated: "!(a && b)"}, "if !(a && b) {"},
		{"at end", "ab", Mutant{Offset: 1, Length: 1, Original: "b", Mutated: "cd"}, "acd"},
		{"empty span", "ab", Mutant{Offset: 1, Length: 0, Original: "", Mutated: "X"}, "aXb"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := []byte(tt.src)
			got, err := Apply(src, tt.m)
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if string(got) != tt.want {
				t.Fatalf("Apply = %q, want %q", got, tt.want)
			}
			if string(src) != tt.src {
				t.Fatalf("Apply modified its input: %q", src)
			}
		})
	}
}

func TestApplyDoesNotAliasInput(t *testing.T) {
	src := []byte("a > b")
	got, err := Apply(src, Mutant{Offset: 2, Length: 1, Original: ">", Mutated: "<"})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "a < b" {
		t.Fatalf("Apply = %q, want %q", got, "a < b")
	}
	got[0] = 'Z'
	if string(src) != "a > b" {
		t.Fatalf("result shares memory with src: %q", src)
	}
}

func TestApplyRejectsMismatch(t *testing.T) {
	tests := []struct {
		name string
		src  string
		m    Mutant
	}{
		{"wrong bytes", "a > b", Mutant{Offset: 2, Length: 1, Original: "<", Mutated: "<="}},
		{"shifted offset", "a > b", Mutant{Offset: 1, Length: 1, Original: ">", Mutated: ">="}},
		{"length longer than original", "a > b", Mutant{Offset: 2, Length: 2, Original: ">", Mutated: ">="}},
		{"length shorter than original", "a >= b", Mutant{Offset: 2, Length: 1, Original: ">=", Mutated: ">"}},
		{"past end", "a > b", Mutant{Offset: 4, Length: 3, Original: "b", Mutated: "c"}},
		{"offset past end", "ab", Mutant{Offset: 5, Length: 0, Original: "", Mutated: "c"}},
		{"negative offset", "ab", Mutant{Offset: -1, Length: 1, Original: "a", Mutated: "c"}},
		{"negative length", "ab", Mutant{Offset: 1, Length: -1, Original: "", Mutated: "c"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Apply([]byte(tt.src), tt.m)
			if err == nil {
				t.Fatalf("Apply succeeded with %q, want an error", got)
			}
			if got != nil {
				t.Fatalf("Apply returned %q alongside an error", got)
			}
		})
	}
}

func TestParseOperators(t *testing.T) {
	all := []string{"cmp", "logic", "const", "ret", "stmt", "cfg"}
	tests := []struct {
		in   string
		want []string
	}{
		{"cmp", []string{"cmp"}},
		{"cmp,logic,const,ret,stmt,cfg", all},
		{"cfg,stmt,ret,const,logic,cmp", all},
		{"stmt,cmp", []string{"cmp", "stmt"}},
		{"cmp,cmp,cmp", []string{"cmp"}},
		{"stmt,cmp,stmt,cmp", []string{"cmp", "stmt"}},
		{" cmp , logic ", []string{"cmp", "logic"}},
		{"cfg", []string{"cfg"}},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseOperators(tt.in)
			if err != nil {
				t.Fatalf("ParseOperators(%q): %v", tt.in, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ParseOperators(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseOperatorsRejects(t *testing.T) {
	tests := []struct {
		in      string
		wantMsg string
	}{
		{"", "empty"},
		{"   ", "empty"},
		{"cmp,,logic", "empty"},
		{",cmp", "empty"},
		{"cmp,", "empty"},
		{"bogus", `"bogus"`},
		{"cmp,bogus", `"bogus"`},
		{"CMP", `"CMP"`},
		{"cmp logic", `"cmp logic"`},
		{"cmp;logic", `"cmp;logic"`},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseOperators(tt.in)
			if err == nil {
				t.Fatalf("ParseOperators(%q) = %v, want an error", tt.in, got)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Fatalf("error %q does not mention %q", err, tt.wantMsg)
			}
			if got != nil {
				t.Fatalf("ParseOperators(%q) returned %v alongside an error", tt.in, got)
			}
		})
	}
}

func TestIDKnownAnswers(t *testing.T) {
	// Independent vectors: sha256 over file, offset, operator and mutated
	// joined by NUL bytes, first 16 hex digits (computed outside Go).
	tests := []struct {
		file     string
		offset   int
		operator string
		mutated  string
		want     string
	}{
		{"a.go", 10, "cmp", ">=", "0e2132a73d90638c"},
		{"internal/x/y.go", 0, "cfg", `""`, "1600d2be5f7ce8e5"},
	}
	for _, tt := range tests {
		if got := makeID(tt.file, tt.offset, tt.operator, tt.mutated); got != tt.want {
			t.Errorf("makeID(%q, %d, %q, %q) = %q, want %q", tt.file, tt.offset, tt.operator, tt.mutated, got, tt.want)
		}
	}
}

func TestIDDependsOnEveryComponent(t *testing.T) {
	base := makeID("a.go", 10, "cmp", ">=")
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(base) {
		t.Fatalf("ID %q is not 16 lowercase hex digits", base)
	}
	variants := map[string]string{
		"file":     makeID("b.go", 10, "cmp", ">="),
		"offset":   makeID("a.go", 11, "cmp", ">="),
		"operator": makeID("a.go", 10, "logic", ">="),
		"mutated":  makeID("a.go", 10, "cmp", ">"),
	}
	for what, id := range variants {
		if id == base {
			t.Errorf("changing the %s did not change the ID %q", what, id)
		}
	}
	// The NUL separators keep neighbouring components from bleeding into each other.
	if makeID("a.go1", 0, "cmp", "x") == makeID("a.go", 10, "cmp", "x") {
		t.Error("file and offset are not separated")
	}
	if makeID("a.go", 1, "cmp0", "x") == makeID("a.go", 10, "cmp", "x") {
		t.Error("offset and operator are not separated")
	}
}

func TestBuilderFillsDerivedFields(t *testing.T) {
	src := []byte("ab\ncd\r\nef>g\n")
	b := newBuilder(`internal\p\f.go`, src)
	b.add("cmp", 9, 1, ">=") // the '>' on line 3
	b.add("stmt", 0, 2, "")  // "ab" on line 1
	got := b.sorted()
	want := []Mutant{
		{
			ID: makeID("internal/p/f.go", 0, "stmt", ""), File: "internal/p/f.go", Line: 1,
			Operator: "stmt", Original: "ab", Mutated: "", Offset: 0, Length: 2,
		},
		{
			ID: makeID("internal/p/f.go", 9, "cmp", ">="), File: "internal/p/f.go", Line: 3,
			Operator: "cmp", Original: ">", Mutated: ">=", Offset: 9, Length: 1,
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sorted() =\n%+v\nwant\n%+v", got, want)
	}
}

func TestBuilderLineOfOffset(t *testing.T) {
	src := []byte("a\n\nbcd\nef")
	tests := []struct {
		offset int
		line   int
	}{
		{0, 1}, // a
		{1, 1}, // the newline ending line 1 still belongs to line 1
		{2, 2}, // empty line 2
		{3, 3}, // b
		{5, 3}, // d
		{6, 3}, // newline of line 3
		{7, 4}, // e
		{8, 4}, // f
	}
	for _, tt := range tests {
		b := newBuilder("f.go", src)
		b.add("cmp", tt.offset, 1, "x")
		got := b.sorted()
		if len(got) != 1 || got[0].Line != tt.line {
			t.Errorf("offset %d: got %+v, want line %d", tt.offset, got, tt.line)
		}
	}
}

func TestBuilderRecordsInvalidSpan(t *testing.T) {
	for _, tt := range []struct{ offset, length int }{{8, 5}, {-1, 1}, {3, -1}, {11, 0}} {
		b := newBuilder("f.go", []byte("0123456789"))
		b.add("cmp", tt.offset, tt.length, "x")
		if b.err == nil {
			t.Errorf("add(%d, %d) on a 10 byte file recorded no error", tt.offset, tt.length)
		}
		if got := b.sorted(); len(got) != 0 {
			t.Errorf("add(%d, %d) kept the invalid mutant %+v", tt.offset, tt.length, got)
		}
	}
	b := newBuilder("f.go", []byte("0123456789"))
	b.add("cmp", 9, 1, "x")
	if b.err != nil {
		t.Errorf("span ending exactly at EOF rejected: %v", b.err)
	}
}

func TestBuilderDeduplicatesAndOrders(t *testing.T) {
	src := []byte("0123456789")
	b := newBuilder("f.go", src)
	b.add("stmt", 4, 1, "")
	b.add("const", 4, 1, "9")
	b.add("const", 4, 1, "1")
	b.add("const", 4, 1, "9") // duplicate of the second add
	b.add("cmp", 1, 1, "x")
	b.add("cmp", 4, 1, "y")
	got := b.sorted()
	type key struct {
		off int
		op  string
		mut string
	}
	var gotKeys []key
	for _, m := range got {
		gotKeys = append(gotKeys, key{m.Offset, m.Operator, m.Mutated})
	}
	wantKeys := []key{
		{1, "cmp", "x"},
		{4, "cmp", "y"},
		{4, "const", "1"},
		{4, "const", "9"},
		{4, "stmt", ""},
	}
	if !reflect.DeepEqual(gotKeys, wantKeys) {
		t.Fatalf("order/dedup = %v, want %v", gotKeys, wantKeys)
	}
	seen := map[string]bool{}
	for _, m := range got {
		if seen[m.ID] {
			t.Fatalf("duplicate ID %s", m.ID)
		}
		seen[m.ID] = true
	}
}
