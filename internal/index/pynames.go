package index

import (
	"bytes"
	"strings"
	"unicode"
	"unicode/utf8"
)

// pyClass is a top-level class being read: its methods are listed when the
// class ends, and only if it turned out to have no __init__ (pytest does not
// collect a class that has one).
type pyClass struct {
	name       string
	bodyIndent int // column of the class body, -1 until its first statement
	hasInit    bool
	methods    []string
}

// PythonTestNames returns the test function names a pytest default collection
// would find in one module's source: top-level `def test*` functions and
// `def test*` methods of top-level `class Test*` classes without an
// __init__, as "name" or "Class::name", in source order and without
// duplicates. Nested definitions are not tests, and comments and string
// literals (docstrings included) are not code. The result is derived from the
// text alone, so it ignores parametrization and anything pytest computes at
// import time.
func PythonTestNames(src []byte) []string {
	s := &pyScanner{text: normalizePythonSource(src)}
	var names []string
	seen := map[string]bool{}
	add := func(name string) {
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	var open *pyClass
	closeClass := func() {
		if open != nil && !open.hasInit {
			for _, method := range open.methods {
				add(open.name + "::" + method)
			}
		}
		open = nil
	}
	for {
		indent, start, ok := s.nextStatement()
		if !ok {
			break
		}
		kind, name := pyDefinition(s.text[start:])
		s.skipStatement()
		switch {
		case indent == 0:
			closeClass()
			switch {
			case kind == "def" && strings.HasPrefix(name, "test"):
				add(name)
			case kind == "class" && strings.HasPrefix(name, "Test"):
				open = &pyClass{name: name, bodyIndent: -1}
			}
		case open != nil:
			if open.bodyIndent < 0 {
				open.bodyIndent = indent
			}
			if indent != open.bodyIndent || kind != "def" {
				continue
			}
			if name == "__init__" {
				open.hasInit = true
			} else if strings.HasPrefix(name, "test") {
				open.methods = append(open.methods, name)
			}
		}
	}
	closeClass()
	return names
}

// normalizePythonSource drops a UTF-8 byte order mark and turns every line
// ending into "\n".
func normalizePythonSource(src []byte) string {
	text := string(bytes.TrimPrefix(src, []byte("\xef\xbb\xbf")))
	text = strings.ReplaceAll(text, "\r\n", "\n")
	return strings.ReplaceAll(text, "\r", "\n")
}

// pyScanner walks the logical lines of Python source.
type pyScanner struct {
	text string
	pos  int
}

// nextStatement skips blank and comment-only lines and returns the indentation
// column and the offset of the first token of the next logical line. ok is
// false at the end of the input.
func (s *pyScanner) nextStatement() (indent, start int, ok bool) {
	for s.pos < len(s.text) {
		col, p := 0, s.pos
	blanks:
		for p < len(s.text) {
			switch s.text[p] {
			case ' ':
				col++
			case '\t':
				col = (col/8 + 1) * 8
			case '\f':
				col = 0
			default:
				break blanks
			}
			p++
		}
		s.pos = p
		switch {
		case p >= len(s.text):
			return 0, 0, false
		case s.text[p] == '\n':
			s.pos++
		case s.text[p] == '#':
			s.skipComment()
		default:
			return col, p, true
		}
	}
	return 0, 0, false
}

func (s *pyScanner) skipComment() {
	for s.pos < len(s.text) && s.text[s.pos] != '\n' {
		s.pos++
	}
}

// skipStatement advances past the logical line that starts at pos: it ends at
// the first newline outside brackets, strings and a backslash continuation.
func (s *pyScanner) skipStatement() {
	depth := 0
	for s.pos < len(s.text) {
		switch c := s.text[s.pos]; c {
		case '\n':
			s.pos++
			if depth == 0 {
				return
			}
		case '#':
			s.skipComment()
		case '\\':
			s.pos = min(s.pos+2, len(s.text))
		case '\'', '"':
			s.skipString()
		case '(', '[', '{':
			depth++
			s.pos++
		case ')', ']', '}':
			depth = max(depth-1, 0)
			s.pos++
		default:
			s.pos++
		}
	}
}

// skipString advances past the string literal that opens at pos. A quote
// preceded by a backslash does not end it, raw strings included. A
// single-quoted string also ends at an unescaped newline, which is left for
// skipStatement so an unterminated string cannot swallow the rest of the file.
func (s *pyScanner) skipString() {
	quote := s.text[s.pos]
	if triple := strings.Repeat(string(quote), 3); strings.HasPrefix(s.text[s.pos:], triple) {
		s.pos += 3
		for s.pos < len(s.text) {
			switch {
			case s.text[s.pos] == '\\':
				s.pos = min(s.pos+2, len(s.text))
			case strings.HasPrefix(s.text[s.pos:], triple):
				s.pos += 3
				return
			default:
				s.pos++
			}
		}
		return
	}
	s.pos++
	for s.pos < len(s.text) {
		switch s.text[s.pos] {
		case '\\':
			s.pos = min(s.pos+2, len(s.text))
		case '\n':
			return
		case quote:
			s.pos++
			return
		default:
			s.pos++
		}
	}
}

// pyDefinition reports whether a logical line starts with `def name`,
// `async def name` or `class name`, returning "def" or "class" and the name.
func pyDefinition(line string) (kind, name string) {
	word, rest := pyWord(line)
	if word == "async" {
		word, rest = pyWord(strings.TrimLeft(rest, " \t"))
		if word != "def" {
			return "", ""
		}
	}
	if word != "def" && word != "class" {
		return "", ""
	}
	name, _ = pyWord(strings.TrimLeft(rest, " \t"))
	if name == "" {
		return "", ""
	}
	return word, name
}

// pyWord returns the identifier at the start of text (Unicode letters, digits,
// marks and underscore; it cannot start with a digit) and the remaining text.
func pyWord(text string) (word, rest string) {
	end := 0
	for end < len(text) {
		r, size := utf8.DecodeRuneInString(text[end:])
		isStart := unicode.IsLetter(r) || r == '_'
		isPart := isStart || unicode.IsDigit(r) || unicode.IsMark(r)
		if end == 0 && !isStart || !isPart {
			break
		}
		end += size
	}
	return text[:end], text[end:]
}
