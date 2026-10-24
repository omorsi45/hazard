package graph

import (
	"regexp"
	"strings"
)

// pyImport is one import statement found by scanPython.
type pyImport struct {
	From   bool     // "from X import ..." form
	Level  int      // number of leading dots of a relative import
	Module string   // dotted module, "" for "from . import x"
	Names  []string // imported names of the from form, "*" for star
}

var (
	pyDynamicCall = regexp.MustCompile(`(?:\b__import__|\bimport_module)\s*\(`)
	pyImportStmt  = regexp.MustCompile(`^import\s+(.+)$`)
	pyFromStmt    = regexp.MustCompile(`^from\s+(\.*)\s*([\w.\s]*?)\s*\bimport\b\s*(.*)$`)
	pyStartsImp   = regexp.MustCompile(`^(?:import|from)\b`)
)

// scanPython returns the import statements of a Python source file and whether
// it uses dynamic imports. ok is false when the source could not be scanned
// reliably (unbalanced brackets or an unterminated string at end of file).
func scanPython(src []byte) (imports []pyImport, dynamic bool, ok bool) {
	lines, ok := logicalLines(src)
	for _, line := range lines {
		if pyDynamicCall.MatchString(line) {
			dynamic = true
		}
		for _, stmt := range strings.Split(line, ";") {
			stmt = strings.TrimSpace(stmt)
			if !pyStartsImp.MatchString(stmt) {
				stmt = afterHeader(stmt)
			}
			if imp, found := parseImport(stmt); found {
				imports = append(imports, imp...)
			}
		}
	}
	return imports, dynamic, ok
}

// afterHeader returns the statement following the top-level colon of a
// one-line compound statement ("try: import a"), or "" when there is none.
func afterHeader(stmt string) string {
	depth := 0
	for i := 0; i < len(stmt); i++ {
		switch stmt[i] {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case ':':
			if depth == 0 {
				rest := strings.TrimSpace(stmt[i+1:])
				if pyStartsImp.MatchString(rest) {
					return rest
				}
			}
		}
	}
	return ""
}

func parseImport(stmt string) ([]pyImport, bool) {
	if m := pyImportStmt.FindStringSubmatch(stmt); m != nil {
		var out []pyImport
		for _, item := range strings.Split(m[1], ",") {
			name := importedName(item)
			if name != "" {
				out = append(out, pyImport{Module: name})
			}
		}
		return out, len(out) > 0
	}
	if m := pyFromStmt.FindStringSubmatch(stmt); m != nil {
		imp := pyImport{From: true, Level: len(m[1]), Module: strings.Join(strings.Fields(m[2]), "")}
		rest := strings.Trim(strings.TrimSpace(m[3]), "()")
		for _, item := range strings.Split(rest, ",") {
			if name := importedName(item); name != "" {
				imp.Names = append(imp.Names, name)
			}
		}
		if imp.Level == 0 && imp.Module == "" {
			return nil, false
		}
		return []pyImport{imp}, true
	}
	return nil, false
}

// importedName returns the name part of "name [as alias]" without whitespace.
func importedName(item string) string {
	fields := strings.Fields(strings.Trim(strings.TrimSpace(item), "()"))
	if len(fields) == 0 {
		return ""
	}
	if len(fields) >= 3 && fields[len(fields)-2] == "as" {
		fields = fields[:len(fields)-2]
	}
	return strings.Join(fields, "")
}

// logicalLines returns the code of src with comments removed and string
// literals replaced by an empty literal. Physical lines are joined while
// brackets are open or after a backslash continuation. ok is false when a
// string is unterminated at end of file or brackets are unbalanced there.
func logicalLines(src []byte) (lines []string, ok bool) {
	var line []byte
	depth := 0
	ok = true
	flush := func() {
		if text := strings.TrimSpace(string(line)); text != "" {
			lines = append(lines, text)
		}
		line = line[:0]
	}
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == '#':
			for i < len(src) && src[i] != '\n' && src[i] != '\r' {
				i++
			}
		case c == '"' || c == '\'':
			end, closed := stringEnd(src, i)
			if !closed {
				ok = false
			}
			line = append(line, '"', '"')
			i = end
		case c == '\\' && i+1 < len(src) && (src[i+1] == '\n' || src[i+1] == '\r'):
			i += 2
			if src[i-1] == '\r' && i < len(src) && src[i] == '\n' {
				i++
			}
			line = append(line, ' ')
		case c == '\n' || c == '\r':
			i++
			if depth > 0 {
				line = append(line, ' ')
			} else {
				flush()
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
			line = append(line, c)
			i++
		}
	}
	if depth > 0 {
		ok = false
	}
	flush()
	return lines, ok
}

// stringEnd returns the index just past the string literal starting at src[start]
// and whether it was terminated. A single-quoted string ends at an unescaped
// newline without consuming it.
func stringEnd(src []byte, start int) (end int, closed bool) {
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
