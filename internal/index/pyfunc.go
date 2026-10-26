package index

import (
	"regexp"
	"strings"
)

const (
	maxAssertTargets = 5
	maxTargetRunes   = 80
)

// pyStmt is one logical Python statement.
type pyStmt struct {
	Indent int
	Line   int    // 1-based first physical line
	Raw    string // comments removed, string literals kept, newlines folded to spaces
	Mask   string // same length as Raw with string literal characters replaced by '_'
	Doc    string // literal content when the statement is a single string literal
	IsStr  bool
}

// pyFunc is what a constrained source scan learns about one test function.
type pyFunc struct {
	Line    int
	Purpose string // first docstring line, "" when there is none
	Asserts []string
}

var pyStringPrefix = regexp.MustCompile(`^[rRuUbBfF]{0,2}$`)

// lexPython splits Python source into logical statements: physical lines are
// joined while brackets are open or after a backslash, comments are dropped
// and string literals are masked in Mask while kept in Raw.
func lexPython(src []byte) []pyStmt {
	var stmts []pyStmt
	var raw, mask []byte
	var lits [][3]int // quote start, end, quote length within raw
	var litContent []string
	started := false
	indent, startLine, line, depth := 0, 0, 1, 0
	finish := func() {
		for len(raw) > 0 && raw[len(raw)-1] == ' ' {
			raw, mask = raw[:len(raw)-1], mask[:len(mask)-1]
		}
		if len(raw) > 0 {
			st := pyStmt{Indent: indent, Line: startLine, Raw: string(raw), Mask: string(mask)}
			if len(lits) == 1 && lits[0][1] == len(raw) && pyStringPrefix.MatchString(string(raw[:lits[0][0]])) {
				st.IsStr, st.Doc = true, litContent[0]
			}
			stmts = append(stmts, st)
		}
		raw, mask, lits, litContent, started, depth = raw[:0], mask[:0], lits[:0], litContent[:0], false, 0
	}
	for i := 0; i < len(src); {
		if !started {
			j, ind := i, 0
			for j < len(src) && (src[j] == ' ' || src[j] == '\t' || src[j] == '\f') {
				if src[j] == '\t' {
					ind += 8 - ind%8
				} else {
					ind++
				}
				j++
			}
			switch {
			case j >= len(src):
				return stmts
			case src[j] == '\r' || src[j] == '\n' || src[j] == '#':
				for j < len(src) && src[j] != '\n' {
					j++
				}
				if j < len(src) {
					j++
					line++
				}
				i = j
				continue
			}
			started, indent, startLine, i = true, ind, line, j
		}
		c := src[i]
		switch {
		case c == '#':
			for i < len(src) && src[i] != '\n' && src[i] != '\r' {
				i++
			}
		case c == '"' || c == '\'':
			end, _ := pyStringEnd(src, i)
			qlen := 1
			if end-i >= 6 && src[i+1] == c && src[i+2] == c {
				qlen = 3
			}
			start := len(raw)
			for _, b := range src[i:end] {
				switch b {
				case '\n':
					line++
					raw, mask = append(raw, ' '), append(mask, ' ')
				case '\r':
					raw, mask = append(raw, ' '), append(mask, ' ')
				default:
					raw, mask = append(raw, b), append(mask, '_')
				}
			}
			contentEnd := end - qlen
			if contentEnd < i+qlen {
				contentEnd = end
			}
			lits = append(lits, [3]int{start, len(raw), qlen})
			litContent = append(litContent, string(src[i+qlen:contentEnd]))
			i = end
		case c == '\\' && i+1 < len(src) && (src[i+1] == '\n' || src[i+1] == '\r'):
			i += 2
			if src[i-1] == '\r' && i < len(src) && src[i] == '\n' {
				i++
			}
			line++
			raw, mask = append(raw, ' '), append(mask, ' ')
		case c == '\n' || c == '\r':
			i++
			if c == '\r' && i < len(src) && src[i] == '\n' {
				i++
			}
			line++
			if depth > 0 {
				raw, mask = append(raw, ' '), append(mask, ' ')
			} else {
				finish()
			}
		default:
			switch c {
			case '(', '[', '{':
				depth++
			case ')', ']', '}':
				if depth > 0 {
					depth--
				}
			}
			raw, mask = append(raw, c), append(mask, c)
			i++
		}
	}
	finish()
	return stmts
}

// pyStringEnd returns the index just past the string literal starting at
// src[start]. An unterminated literal ends at end of file, or at the newline for
// a single-quoted one.
func pyStringEnd(src []byte, start int) (end int, closed bool) {
	quote := src[start]
	triple := start+2 < len(src) && src[start+1] == quote && src[start+2] == quote
	i := start + 1
	if triple {
		i = start + 3
	}
	for i < len(src) {
		switch {
		case src[i] == '\\':
			i += 2
		case triple && src[i] == quote && i+2 < len(src) && src[i+1] == quote && src[i+2] == quote:
			return i + 3, true
		case !triple && src[i] == quote:
			return i + 1, true
		case !triple && (src[i] == '\n' || src[i] == '\r'):
			return i, false
		default:
			i++
		}
	}
	return len(src), false
}

// findPyFunc locates classes[0].classes[1]...name in stmts and returns what the
// source says about it. The last definition wins, like Python.
func findPyFunc(stmts []pyStmt, classes []string, name string) (pyFunc, bool) {
	lo, hi := 0, len(stmts)
	for _, class := range classes {
		header := regexp.MustCompile(`^class\s+` + regexp.QuoteMeta(class) + `\b`)
		idx := lastChild(stmts, lo, hi, header)
		if idx < 0 {
			return pyFunc{}, false
		}
		lo, hi = idx+1, blockEnd(stmts, idx, hi)
	}
	def := regexp.MustCompile(`^(?:async\s+)?def\s+` + regexp.QuoteMeta(name) + `\s*\(`)
	idx := lastChild(stmts, lo, hi, def)
	if idx < 0 {
		return pyFunc{}, false
	}
	end := blockEnd(stmts, idx, len(stmts))
	fn := pyFunc{Line: stmts[idx].Line}
	body := stmts[idx+1 : end]
	if len(body) > 0 && body[0].IsStr {
		for _, line := range strings.Split(body[0].Doc, "\n") {
			if text := strings.Join(strings.Fields(line), " "); text != "" {
				fn.Purpose = text
				break
			}
		}
	}
	seen := map[string]bool{}
	for _, st := range body {
		if len(fn.Asserts) >= maxAssertTargets {
			break
		}
		if target, ok := assertTarget(st); ok && !seen[target] {
			seen[target] = true
			fn.Asserts = append(fn.Asserts, target)
		}
	}
	return fn, true
}

// lastChild returns the last statement in [lo,hi) matching re that is a direct
// child of the block, or -1.
func lastChild(stmts []pyStmt, lo, hi int, re *regexp.Regexp) int {
	if lo >= hi {
		return -1
	}
	childIndent := stmts[lo].Indent
	found := -1
	for i := lo; i < hi; i++ {
		if stmts[i].Indent == childIndent && re.MatchString(stmts[i].Mask) {
			found = i
		}
	}
	return found
}

// blockEnd returns the index of the first statement after header that is not
// indented deeper than it, capped at limit.
func blockEnd(stmts []pyStmt, header, limit int) int {
	end := header + 1
	for end < limit && stmts[end].Indent > stmts[header].Indent {
		end++
	}
	return end
}

// assertTarget returns the asserted expression of an assert statement without
// its message.
func assertTarget(st pyStmt) (string, bool) {
	const keyword = "assert"
	if !strings.HasPrefix(st.Mask, keyword) || len(st.Mask) == len(keyword) || (st.Mask[len(keyword)] != ' ' && st.Mask[len(keyword)] != '(') {
		return "", false
	}
	raw, mask := st.Raw[len(keyword):], st.Mask[len(keyword):]
	depth, cut := 0, len(raw)
	for i := 0; i < len(mask) && cut == len(raw); i++ {
		switch mask[i] {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case ',':
			if depth == 0 {
				cut = i
			}
		}
	}
	text := strings.Join(strings.Fields(raw[:cut]), " ")
	if runes := []rune(text); len(runes) > maxTargetRunes {
		text = string(runes[:maxTargetRunes])
	}
	return text, text != ""
}
