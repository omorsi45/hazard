// Package mutate generates source mutants (seed section 11.2): single, local
// edits of Go source and configuration files whose effect on a test suite is
// measured by the driver.
package mutate

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Mutant is one candidate edit: replace Length bytes of the original file at
// Offset (which equal Original) with Mutated.
type Mutant struct {
	ID       string // first 16 hex of sha256(file \x00 offset \x00 operator \x00 mutated)
	File     string // repo-relative, forward slashes
	Line     int    // 1-based line of Offset
	Operator string // cmp|logic|const|ret|stmt|cfg
	Original string // exact replaced bytes
	Mutated  string // replacement
	Offset   int    // byte offset in the original file
	Length   int    // byte length of the replaced span
}

// allOperators lists every operator name in canonical order.
var allOperators = []string{"cmp", "logic", "const", "ret", "stmt", "cfg"}

func isOperator(name string) bool { return slices.Contains(allOperators, name) }

// builder collects the mutants of one file, filling in the derived fields.
type builder struct {
	file   string
	src    []byte
	starts []int // byte offset of the first byte of each line
	seen   map[string]bool
	out    []Mutant
	err    error // first invalid span passed to add
}

func newBuilder(file string, src []byte) *builder {
	b := &builder{
		file:   strings.ReplaceAll(file, `\`, "/"),
		src:    src,
		starts: []int{0},
		seen:   map[string]bool{},
	}
	for i, c := range src {
		if c == '\n' {
			b.starts = append(b.starts, i+1)
		}
	}
	return b
}

// add records a mutant replacing src[offset:offset+length] with mutated. A
// mutant whose ID was already recorded is dropped. A span outside the file is
// remembered in b.err and dropped.
func (b *builder) add(operator string, offset, length int, mutated string) {
	if offset < 0 || length < 0 || offset > len(b.src) || length > len(b.src)-offset {
		if b.err == nil {
			b.err = fmt.Errorf("mutate: %s: %s span [%d,+%d) is outside the %d byte file", b.file, operator, offset, length, len(b.src))
		}
		return
	}
	id := makeID(b.file, offset, operator, mutated)
	if b.seen[id] {
		return
	}
	b.seen[id] = true
	b.out = append(b.out, Mutant{
		ID:       id,
		File:     b.file,
		Line:     sort.SearchInts(b.starts, offset+1),
		Operator: operator,
		Original: string(b.src[offset : offset+length]),
		Mutated:  mutated,
		Offset:   offset,
		Length:   length,
	})
}

// sorted returns the collected mutants ordered by Offset, Operator, Mutated.
func (b *builder) sorted() []Mutant {
	out := slices.Clone(b.out)
	slices.SortFunc(out, func(x, y Mutant) int {
		return cmp.Or(
			cmp.Compare(x.Offset, y.Offset),
			cmp.Compare(x.Operator, y.Operator),
			cmp.Compare(x.Mutated, y.Mutated),
		)
	})
	return out
}

// makeID returns the stable identifier of a mutant.
func makeID(file string, offset int, operator, mutated string) string {
	h := sha256.New()
	h.Write([]byte(file))
	h.Write([]byte{0})
	h.Write([]byte(strconv.Itoa(offset)))
	h.Write([]byte{0})
	h.Write([]byte(operator))
	h.Write([]byte{0})
	h.Write([]byte(mutated))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// Apply returns a copy of src with the mutant applied. It fails unless the
// span src[Offset:Offset+Length] is exactly m.Original.
func Apply(src []byte, m Mutant) ([]byte, error) {
	if m.Offset < 0 || m.Length < 0 || m.Offset > len(src) || m.Length > len(src)-m.Offset {
		return nil, fmt.Errorf("mutate: mutant %s: span [%d,+%d) is outside the %d byte file %s", m.ID, m.Offset, m.Length, len(src), m.File)
	}
	if got := string(src[m.Offset : m.Offset+m.Length]); got != m.Original {
		return nil, fmt.Errorf("mutate: mutant %s: %s at offset %d holds %q, want %q", m.ID, m.File, m.Offset, got, m.Original)
	}
	out := make([]byte, 0, len(src)-m.Length+len(m.Mutated))
	out = append(out, src[:m.Offset]...)
	out = append(out, m.Mutated...)
	out = append(out, src[m.Offset+m.Length:]...)
	return out, nil
}

// ParseOperators validates a comma separated operator list and returns it
// deduplicated in canonical order.
func ParseOperators(s string) ([]string, error) {
	want := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		name := strings.TrimSpace(part)
		if name == "" {
			return nil, fmt.Errorf("mutate: empty operator in %q (valid: %s)", s, strings.Join(allOperators, ", "))
		}
		if !isOperator(name) {
			return nil, fmt.Errorf("mutate: unknown operator %q (valid: %s)", name, strings.Join(allOperators, ", "))
		}
		want[name] = true
	}
	var out []string
	for _, name := range allOperators {
		if want[name] {
			out = append(out, name)
		}
	}
	return out, nil
}
