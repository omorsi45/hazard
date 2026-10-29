package change

import (
	"bytes"
	"strings"
)

// kindRule maps a group of globs to a kind. Rules are tried in order, which
// is the precedence of the architecture contract after the generated check.
type kindRule struct {
	kind  FileKind
	globs []string
}

var kindRules = []kindRule{
	{KindTest, []string{"*_test.go", "test_*.py", "*_test.py", "**/tests/**"}},
	{KindLock, []string{"go.sum", "uv.lock", "*.lock", "poetry.lock", "package-lock.json"}},
	{KindBuild, []string{
		"go.mod", "Makefile", "Dockerfile*", "pyproject.toml", "setup.py", "setup.cfg",
		".github/workflows/**", ".gitlab-ci.yml",
	}},
	{KindFixture, []string{"**/testdata/**", "**/fixtures/**", "conftest.py"}},
	{KindDocs, []string{"*.md", "*.rst", "*.txt", "docs/**", "LICENSE"}},
	{KindConfig, []string{"*.yaml", "*.yml", "*.json", "*.toml", "*.ini", "*.cfg", "*.env", "*.properties"}},
}

// Classify returns the kind of the slash-separated path. head is the file
// contents used to recognise a generated header; it may be nil. Precedence is
// generated, test, lock, build, fixture, docs, config, source.
//
// A Go file is generated when a leading comment line is
// "// Code generated ... DO NOT EDIT." and a Python file when its first line
// starts with "# Generated"; other file types are generated only through a
// glob in generatedGlobs.
func Classify(path string, head []byte, generatedGlobs []string) FileKind {
	if matchAny(generatedGlobs, path) || hasGeneratedHeader(path, head) {
		return KindGenerated
	}
	for _, rule := range kindRules {
		if matchAny(rule.globs, path) {
			return rule.kind
		}
	}
	return KindSource
}

func matchAny(globs []string, path string) bool {
	for _, g := range globs {
		if MatchGlob(g, path) {
			return true
		}
	}
	return false
}

func hasGeneratedHeader(path string, src []byte) bool {
	src = bytes.TrimPrefix(src, []byte("\xef\xbb\xbf"))
	switch {
	case strings.HasSuffix(path, ".go"):
		return hasGoGeneratedHeader(src)
	case strings.HasSuffix(path, ".py"):
		line, _, _ := bytes.Cut(src, []byte("\n"))
		return bytes.HasPrefix(line, []byte("# Generated"))
	}
	return false
}

// hasGoGeneratedHeader applies the Go convention: a line comment of the form
// "// Code generated ... DO NOT EDIT." before the first non-comment text.
func hasGoGeneratedHeader(src []byte) bool {
	const prefix, suffix = "// Code generated ", " DO NOT EDIT."
	inBlock := false
	for len(src) > 0 {
		var raw []byte
		raw, src, _ = bytes.Cut(src, []byte("\n"))
		line := strings.TrimRight(string(raw), "\r")
		trimmed := strings.TrimSpace(line)
		switch {
		case inBlock:
			inBlock = !strings.Contains(trimmed, "*/")
		case trimmed == "":
		case strings.HasPrefix(line, "//"):
			if len(line) >= len(prefix)+len(suffix) && strings.HasPrefix(line, prefix) && strings.HasSuffix(line, suffix) {
				return true
			}
		case strings.HasPrefix(trimmed, "//"):
		case strings.HasPrefix(trimmed, "/*"):
			inBlock = !strings.Contains(trimmed[2:], "*/")
		default:
			return false
		}
	}
	return false
}
