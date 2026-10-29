// Package redact builds the change block and the test descriptors that leave
// the machine for the external predictor, from the privacy configuration.
//
// Levels: "none" sends paths, symbol names and, when send_hunk_text is set,
// the hunk text, plus test purposes and assertion targets. "structure" sends
// paths, symbol names, kinds, status, counts and a one-line hunk summary that
// is generated locally from counts, declared symbol names and token-class
// categories only; it never carries source text, literals, comments or
// identifiers other than declared symbol names. "hash" replaces every path,
// symbol, package and test name with a truncated sha256 token. Any other
// value fails closed and is treated as "hash".
package redact

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"strings"
	"unicode"

	"github.com/omorsi45/hazard/internal/change"
	"github.com/omorsi45/hazard/internal/config"
	"github.com/omorsi45/hazard/internal/impact"
)

// Privacy levels of privacy.redact.
const (
	levelNone      = "none"
	levelStructure = "structure"
	levelHash      = "hash"
)

const (
	// fileSymbol is the symbol of a hunk that touches no declaration. It is
	// not a name from the source, so it is never hashed.
	fileSymbol = "__file__"
	// shaLength is the length of the abbreviated commit ids of the block.
	shaLength = 7
	// unreachableHops is the hop count of a test no changed package reaches.
	unreachableHops = 99
	// tokenBytes is the number of sha256 bytes kept in a hash token.
	tokenBytes = 8
)

// Change is the change block of a Jev request.
type Change struct {
	Base         string `json:"base"`
	Head         string `json:"head"`
	LinesAdded   int    `json:"lines_added"`
	LinesRemoved int    `json:"lines_removed"`
	Files        []File `json:"files"`
}

// File is one changed file of the change block.
type File struct {
	Path        string   `json:"path"`
	Kind        string   `json:"kind"`
	Status      string   `json:"status"`
	Package     string   `json:"package,omitempty"`
	HunkSummary string   `json:"hunk_summary"`
	Symbols     []string `json:"symbols"`
	HunkText    string   `json:"hunk_text,omitempty"`
}

// Test is the descriptor of one candidate test.
type Test struct {
	Name               string   `json:"name"`
	Package            string   `json:"package,omitempty"`
	Purpose            string   `json:"purpose,omitempty"`
	Asserts            []string `json:"asserts,omitempty"`
	HopsFromChange     int      `json:"hops_from_change"`
	CoversChangedLines int      `json:"covers_changed_lines"`
}

// level returns the effective privacy level of p. An unknown value fails
// closed to the most restrictive level.
func level(p config.Privacy) string {
	switch p.Redact {
	case levelNone, levelStructure:
		return p.Redact
	default:
		return levelHash
	}
}

// token returns the stable pseudonym of a name.
func token(name string) string {
	sum := sha256.Sum256([]byte(name))
	return "h_" + hex.EncodeToString(sum[:tokenBytes])
}

// shown returns name, or its token when hide is set. An empty name stays
// empty so that an absent package stays absent.
func shown(name string, hide bool) string {
	if hide && name != "" {
		return token(name)
	}
	return name
}

func abbreviate(sha string) string {
	if len(sha) > shaLength {
		return sha[:shaLength]
	}
	return sha
}

// BuildChange returns the change block permitted by p.
func BuildChange(ch *change.ChangeSet, p config.Privacy) Change {
	out := Change{Files: []File{}}
	if ch == nil {
		return out
	}
	lvl := level(p)
	out.Base = abbreviate(ch.BaseSHA)
	out.Head = abbreviate(ch.HeadSHA)
	out.LinesAdded = ch.LinesAdded
	out.LinesRemoved = ch.LinesRemoved
	for _, f := range ch.Files {
		out.Files = append(out.Files, buildFile(f, p, lvl))
	}
	return out
}

func buildFile(f change.ChangedFile, p config.Privacy, lvl string) File {
	hidePaths := lvl == levelHash || !p.SendPaths
	hideSymbols := lvl == levelHash || !p.SendSymbolNames

	symbols := []string{}
	var declared []string
	seen := map[string]bool{}
	for _, h := range f.Hunks {
		for _, s := range h.Symbols {
			if seen[s] {
				continue
			}
			seen[s] = true
			name := s
			if s != fileSymbol {
				name = shown(s, hideSymbols)
				declared = append(declared, name)
			}
			symbols = append(symbols, name)
		}
	}

	file := File{
		Path:        shown(f.Path, hidePaths),
		Kind:        f.Kind.String(),
		Status:      f.Status,
		Package:     shown(f.Package, hidePaths),
		HunkSummary: summarize(f, declared),
		Symbols:     symbols,
	}
	if lvl == levelNone && p.SendHunkText {
		file.HunkText = hunkText(f.Hunks)
	}
	return file
}

// hunkText joins the hunk texts of a file, each behind its position header.
func hunkText(hunks []change.Hunk) string {
	var b strings.Builder
	for _, h := range hunks {
		if h.Text == "" {
			continue
		}
		fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n", h.OldStart, h.OldLines, h.NewStart, h.NewLines)
		b.WriteString(h.Text)
		if !strings.HasSuffix(h.Text, "\n") {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// BuildTest returns the descriptor of c permitted by p.
func BuildTest(c impact.Candidate, p config.Privacy) Test {
	lvl := level(p)
	hideSymbols := lvl == levelHash || !p.SendSymbolNames
	t := Test{
		Package:            shown(c.Package, lvl == levelHash || !p.SendPaths),
		HopsFromChange:     hops(c),
		CoversChangedLines: coveredLines(c),
	}
	switch {
	case hideSymbols:
		// The purpose and the assertion targets name symbols, so they go with
		// the name.
		t.Name = token(c.TestName)
	case lvl == levelNone:
		t.Name = c.TestName
		t.Purpose = c.Purpose
		t.Asserts = append([]string(nil), c.Asserts...)
	default:
		t.Name = withoutParameters(c.TestName)
		t.Purpose = words(c.TestName)
	}
	return t
}

func hops(c impact.Candidate) int {
	if !c.Features.Reachable {
		return unreachableHops
	}
	return c.Features.Hops
}

// coveredLines estimates the changed lines the test covers. Unknown coverage
// is zero lines, which claims nothing.
func coveredLines(c impact.Candidate) int {
	f := c.Features
	if !f.CoverageKnown {
		return 0
	}
	return int(math.Round(f.CoverageOverlap * float64(f.LinesAdded+f.LinesRemoved)))
}

// withoutParameters cuts a test name at its first parameter or subtest
// separator. Both carry values that come from the test source.
func withoutParameters(name string) string {
	if i := strings.IndexAny(name, "[/"); i >= 0 {
		return name[:i]
	}
	return name
}

// words splits a test name into lower-case words. A leading Test or test_
// marker is dropped unless it is the whole name, and parameters and subtests
// are cut off first.
func words(name string) string {
	runes := []rune(trimTestMarker(withoutParameters(name)))
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			out = append(out, string(cur))
			cur = cur[:0]
		}
	}
	for i, r := range runes {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			flush()
			continue
		}
		if len(cur) > 0 {
			prev := cur[len(cur)-1]
			acronymEnd := unicode.IsUpper(r) && unicode.IsUpper(prev) && i+1 < len(runes) && unicode.IsLower(runes[i+1])
			if unicode.IsDigit(r) != unicode.IsDigit(prev) || (unicode.IsUpper(r) && unicode.IsLower(prev)) || acronymEnd {
				flush()
			}
		}
		cur = append(cur, r)
	}
	flush()
	return strings.ToLower(strings.Join(out, " "))
}

// trimTestMarker drops the Test, test_ or test prefix of a test function name
// when something follows it that starts a new word.
func trimTestMarker(name string) string {
	for _, prefix := range []string{"Test", "test"} {
		rest, ok := strings.CutPrefix(name, prefix)
		if !ok || rest == "" {
			continue
		}
		first := []rune(rest)[0]
		if first == '_' || unicode.IsUpper(first) || unicode.IsDigit(first) {
			return rest
		}
	}
	return name
}
