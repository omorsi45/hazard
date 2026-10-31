package mutate

import (
	"bytes"
	"math"
	"math/big"
	"path"
	"regexp"
	"strconv"
	"strings"
)

type cfgFormat int

const (
	cfgNone cfgFormat = iota
	cfgYAML
	cfgTOML
	cfgINI
	cfgEnv
	cfgProperties
	cfgJSON
)

var (
	intRe   = regexp.MustCompile(`^[+-]?[0-9]+$`)
	floatRe = regexp.MustCompile(`^[+-]?(?:(?:[0-9]+\.[0-9]*|\.[0-9]+)(?:[eE][+-]?[0-9]+)?|[0-9]+[eE][+-]?[0-9]+)$`)
	infNaN  = regexp.MustCompile(`^[+-]?\.(?:inf|nan)$`)
	envKey  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*$`)
)

// cfgFormatOf picks the parser from the file extension.
func cfgFormatOf(file string) cfgFormat {
	switch strings.ToLower(path.Ext(strings.ReplaceAll(file, `\`, "/"))) {
	case ".yaml", ".yml":
		return cfgYAML
	case ".toml":
		return cfgTOML
	case ".ini":
		return cfgINI
	case ".env":
		return cfgEnv
	case ".properties":
		return cfgProperties
	case ".json":
		return cfgJSON
	}
	return cfgNone
}

// cfgScanner reads a configuration file line by line.
type cfgScanner struct {
	format cfgFormat
	b      *builder

	blockIndent int    // yaml: indent of the key owning a block scalar, -1 when none
	multiDelim  string // toml: delimiter that closes a multi-line string
	openQuote   byte   // yaml, env: quote that closes a multi-line quoted value
	depth       int    // yaml, toml: unclosed flow brackets
	continued   bool   // properties: the previous line ended with a backslash
}

// ConfigMutants returns the cfg mutants of one configuration file. The format
// follows the extension (.yaml, .yml, .toml, .ini, .env, .properties, .json);
// other files yield none. Each `key: value`, `key = value` or `"key": value`
// line with a scalar value yields mutants that replace only the value bytes:
// integers become n+1 and (when n is not 0) 0, floats are doubled, booleans
// flip, and strings, quoted or bare, become "". Comments, keys, list markers,
// section headers, collections, block scalars and other multi-line constructs
// are left alone. Parsing is line based, so it is a heuristic: a mutant that
// breaks the file is discarded by the driver's baseline checks.
func ConfigMutants(file string, src []byte) []Mutant {
	format := cfgFormatOf(file)
	if format == cfgNone {
		return nil
	}
	s := &cfgScanner{format: format, b: newBuilder(file, src), blockIndent: -1}
	for offset := 0; offset < len(src); {
		end := len(src)
		if i := bytes.IndexByte(src[offset:], '\n'); i >= 0 {
			end = offset + i
		}
		s.line(offset, strings.TrimSuffix(string(src[offset:end]), "\r"))
		offset = end + 1
	}
	if s.b.err != nil {
		// Every span is cut from the source being scanned, so this is a bug.
		panic(s.b.err)
	}
	return s.b.sorted()
}

// line handles one line, text, which starts at byte offset in the file and has
// no line terminator.
func (s *cfgScanner) line(offset int, text string) {
	switch {
	case s.continued:
		s.continued = hasContinuation(text)
		return
	case s.multiDelim != "":
		if strings.Contains(text, s.multiDelim) {
			s.multiDelim = ""
		}
		return
	case s.openQuote != 0:
		if closesQuote(text, s.openQuote, s.format == cfgYAML) {
			s.openQuote = 0
		}
		return
	case s.depth > 0:
		s.depth = max(0, s.depth+bracketDelta(text))
		return
	case s.blockIndent >= 0:
		if strings.TrimSpace(text) == "" || indentOf(text) > s.blockIndent {
			return
		}
		s.blockIndent = -1
	}
	t := strings.TrimLeft(text, " \t")
	if t == "" {
		return
	}
	start, indent := offset+len(text)-len(t), indentOf(text)
	switch s.format {
	case cfgYAML:
		s.yaml(start, indent, t)
	case cfgTOML:
		if t[0] != '#' && t[0] != '[' {
			s.keyValue(start, indent, t, "=")
		}
	case cfgINI:
		if !strings.ContainsRune(";#[", rune(t[0])) {
			s.keyValue(start, indent, t, "=:")
		}
	case cfgEnv:
		s.env(start, indent, t)
	case cfgProperties:
		s.properties(start, indent, t)
	case cfgJSON:
		s.json(start, t)
	}
}

func (s *cfgScanner) yaml(start, indent int, t string) {
	switch {
	case strings.ContainsRune("#%?[{&*!|>@`", rune(t[0])):
		return
	case t == "-" || strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "-\t"):
		return // list marker
	case t == "---" || strings.HasPrefix(t, "--- ") || t == "..." || strings.HasPrefix(t, "... "):
		return // document markers
	}
	s.keyValue(start, indent, t, ":")
}

func (s *cfgScanner) env(start, indent int, t string) {
	if t[0] == '#' {
		return
	}
	if rest, ok := strings.CutPrefix(t, "export "); ok {
		t = strings.TrimLeft(rest, " \t")
		start += len(rest) - len(t) + len("export ")
	}
	i := strings.IndexByte(t, '=')
	if i <= 0 || !envKey.MatchString(t[:i]) {
		return
	}
	s.afterSeparator(start, indent, t, i)
}

func (s *cfgScanner) properties(start, indent int, t string) {
	if t[0] == '#' || t[0] == '!' {
		return
	}
	if hasContinuation(t) {
		s.continued = true // a logical line spanning several lines is left alone
		return
	}
	s.keyValue(start, indent, t, "=:")
}

func (s *cfgScanner) json(start int, t string) {
	if t[0] != '"' {
		return
	}
	end, ok := scanQuoted(t, '"', false)
	if !ok {
		return
	}
	rest := strings.TrimLeft(t[end:], " \t")
	if rest == "" || rest[0] != ':' {
		return
	}
	v := strings.TrimLeft(rest[1:], " \t")
	s.value(start+len(t)-len(v), v, 0)
}

// keyValue splits t at the first separator that is one of seps and handles the
// value after it.
func (s *cfgScanner) keyValue(start, indent int, t, seps string) {
	i, ok := s.splitKey(t, seps)
	if !ok {
		return
	}
	s.afterSeparator(start, indent, t, i)
}

// afterSeparator handles the value following the separator at t[i].
func (s *cfgScanner) afterSeparator(start, indent int, t string, i int) {
	v := strings.TrimLeft(t[i+1:], " \t")
	s.value(start+len(t)-len(v), v, indent)
}

// splitKey returns the index of the key/value separator in t. A separator
// inside a quoted key or after a comment marker does not count, and yaml needs
// whitespace or the line end after its colon.
func (s *cfgScanner) splitKey(t, seps string) (int, bool) {
	f := s.format
	for i := 0; i < len(t); i++ {
		c := t[i]
		switch {
		case i == 0 && (f == cfgYAML || f == cfgTOML) && (c == '"' || c == '\''):
			end, ok := scanQuoted(t, c, f == cfgYAML)
			if !ok {
				return 0, false
			}
			i = end - 1
		case f == cfgProperties && c == '\\':
			i++ // escaped character
		case strings.IndexByte(seps, c) >= 0:
			if f == cfgYAML && i+1 < len(t) && t[i+1] != ' ' && t[i+1] != '\t' {
				continue
			}
			return i, i > 0
		case i > 0 && f != cfgProperties && isCommentMarker(f, c) && (t[i-1] == ' ' || t[i-1] == '\t'):
			return 0, false
		}
	}
	return 0, false
}

// isCommentMarker reports whether c starts an inline comment in format f.
func isCommentMarker(f cfgFormat, c byte) bool {
	switch f {
	case cfgYAML, cfgTOML, cfgEnv:
		return c == '#'
	case cfgINI:
		return c == '#' || c == ';'
	}
	return false
}

// value handles the text after a separator: v starts at byte offset in the
// file with no leading whitespace. indent is the indentation of the owning
// line, used for yaml block scalars.
func (s *cfgScanner) value(offset int, v string, indent int) {
	v = strings.TrimRight(v, " \t")
	if v == "" {
		return
	}
	f := s.format
	if f != cfgProperties && f != cfgJSON && isCommentMarker(f, v[0]) {
		return // the value is empty and a comment follows
	}
	switch {
	case f == cfgYAML && strings.ContainsRune("|>", rune(v[0])):
		s.blockIndent = indent
		return
	case f == cfgYAML && strings.ContainsRune("&*!@`%?", rune(v[0])):
		return // anchors, aliases, tags and other YAML syntax
	case (f == cfgYAML || f == cfgTOML) && (v[0] == '[' || v[0] == '{'):
		s.depth = max(0, bracketDelta(v))
		return
	case f == cfgTOML && (strings.HasPrefix(v, `"""`) || strings.HasPrefix(v, "'''")):
		if !strings.Contains(v[3:], v[:3]) {
			s.multiDelim = v[:3]
		}
		return
	}
	if f != cfgProperties && (v[0] == '"' || (v[0] == '\'' && f != cfgJSON)) {
		end, ok := scanQuoted(v, v[0], f == cfgYAML)
		if !ok {
			if f == cfgYAML || f == cfgEnv {
				s.openQuote = v[0]
			}
			return
		}
		if end > 2 && tailAllowed(f, v[end:]) {
			s.b.add("cfg", offset, end, `""`)
		}
		return
	}
	tok := v
	switch f {
	case cfgProperties:
	case cfgJSON:
		tok = strings.TrimRight(strings.TrimSuffix(tok, ","), " \t")
	default:
		tok = strings.TrimRight(stripComment(f, tok), " \t")
	}
	if tok == "" {
		return
	}
	for _, m := range bareMutations(f, tok) {
		s.b.add("cfg", offset, len(tok), m)
	}
}

// tailAllowed reports whether rest, the text after a quoted value, holds only
// what may follow it: nothing, a comment, or for json a comma.
func tailAllowed(f cfgFormat, rest string) bool {
	r := strings.TrimLeft(rest, " \t")
	switch {
	case r == "":
		return true
	case f == cfgJSON:
		return strings.TrimRight(r, " \t") == ","
	}
	return len(r) != len(rest) && isCommentMarker(f, r[0])
}

// stripComment removes a trailing comment: a marker preceded by whitespace.
func stripComment(f cfgFormat, tok string) string {
	for i := 1; i < len(tok); i++ {
		if isCommentMarker(f, tok[i]) && (tok[i-1] == ' ' || tok[i-1] == '\t') {
			return tok[:i]
		}
	}
	return tok
}

// bareMutations returns the replacements for an unquoted scalar.
func bareMutations(f cfgFormat, tok string) []string {
	switch {
	case strings.EqualFold(tok, "true"), strings.EqualFold(tok, "false"):
		return []string{flipBool(tok)}
	case intRe.MatchString(tok):
		n, ok := new(big.Int).SetString(tok, 10)
		if !ok {
			return nil
		}
		muts := []string{new(big.Int).Add(n, big.NewInt(1)).String()}
		if n.Sign() != 0 {
			muts = append(muts, "0")
		}
		return muts
	case floatRe.MatchString(tok):
		if d, ok := doubled(tok); ok {
			return []string{d}
		}
		return nil
	}
	switch f {
	case cfgYAML:
		if lower := strings.ToLower(tok); lower == "~" || lower == "null" || infNaN.MatchString(lower) {
			return nil
		}
		return []string{`""`}
	case cfgINI, cfgEnv, cfgProperties:
		return []string{`""`}
	}
	return nil // json and toml: other bare values (null, dates, hex, ...) are not strings
}

// flipBool returns the opposite boolean, keeping the case style of tok.
func flipBool(tok string) string {
	flipped := "true"
	if strings.EqualFold(tok, "true") {
		flipped = "false"
	}
	switch {
	case tok == strings.ToUpper(tok):
		return strings.ToUpper(flipped)
	case tok[0] >= 'A' && tok[0] <= 'Z':
		return strings.ToUpper(flipped[:1]) + flipped[1:]
	}
	return flipped
}

// doubled returns tok*2 formatted so it still reads as a float.
func doubled(tok string) (string, bool) {
	v, err := strconv.ParseFloat(tok, 64)
	if err != nil {
		return "", false
	}
	d := v * 2
	if math.IsInf(d, 0) || d == v {
		return "", false
	}
	format := byte('f')
	if strings.ContainsAny(tok, "eE") {
		format = 'g'
	}
	out := strconv.FormatFloat(d, format, -1, 64)
	if !strings.ContainsAny(out, ".eE") {
		out += ".0"
	}
	return out, true
}

// scanQuoted returns the index just past the quote that closes the quoted token
// at the start of v. Double quotes honor backslash escapes; with yamlSingle a
// doubled single quote is an escaped quote.
func scanQuoted(v string, q byte, yamlSingle bool) (int, bool) {
	for i := 1; i < len(v); i++ {
		switch {
		case q == '"' && v[i] == '\\':
			i++
		case v[i] == q:
			if q == '\'' && yamlSingle && i+1 < len(v) && v[i+1] == '\'' {
				i++
				continue
			}
			return i + 1, true
		}
	}
	return 0, false
}

// closesQuote reports whether text contains the quote ending a multi-line
// quoted value.
func closesQuote(text string, q byte, yamlSingle bool) bool {
	for i := 0; i < len(text); i++ {
		switch {
		case q == '"' && text[i] == '\\':
			i++
		case text[i] == q:
			if q == '\'' && yamlSingle && i+1 < len(text) && text[i+1] == '\'' {
				i++
				continue
			}
			return true
		}
	}
	return false
}

// bracketDelta is the number of flow brackets text opens minus those it
// closes, ignoring quoted text and trailing comments.
func bracketDelta(text string) int {
	delta := 0
	var quote byte
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case quote != 0:
			if c == '\\' && quote == '"' {
				i++
			} else if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '#' && i > 0 && (text[i-1] == ' ' || text[i-1] == '\t'):
			return delta
		case c == '[' || c == '{':
			delta++
		case c == ']' || c == '}':
			delta--
		}
	}
	return delta
}

// hasContinuation reports whether a properties line ends with an unescaped
// backslash.
func hasContinuation(text string) bool {
	n := len(text) - len(strings.TrimRight(text, `\`))
	return n%2 == 1
}

func indentOf(text string) int { return len(text) - len(strings.TrimLeft(text, " \t")) }
