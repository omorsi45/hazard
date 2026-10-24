package index

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	_ "modernc.org/sqlite" // pure Go driver for the coverage.py data file

	"github.com/omorsi45/hazard/internal/config"
)

const pythonCoverageGranularity = "python-test-context"

// pyLines maps coverage context -> repo-relative file -> sorted executed lines.
type pyLines map[string]map[string][]int

// decodeNumbits returns the line numbers set in a coverage.py numbits blob:
// bit i of byte j is line j*8+i.
func decodeNumbits(b []byte) []int {
	var lines []int
	for j, octet := range b {
		for i := 0; i < 8; i++ {
			if octet&(1<<i) != 0 {
				lines = append(lines, j*8+i)
			}
		}
	}
	return lines
}

// readCoverageDB reads the file, context and line_bits tables of a coverage.py
// data file. Paths are made repo-relative with forward slashes; files outside
// root and installed packages are ignored.
func readCoverageDB(dbPath, root string) (pyLines, error) {
	if _, err := os.Stat(dbPath); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT file.path, context.context, line_bits.numbits FROM line_bits JOIN file ON file.id = line_bits.file_id JOIN context ON context.id = line_bits.context_id`)
	if err != nil {
		return nil, fmt.Errorf("read coverage data: %w", err)
	}
	defer rows.Close()
	data := pyLines{}
	for rows.Next() {
		var file string
		var context sql.NullString
		var bits []byte
		if err := rows.Scan(&file, &context, &bits); err != nil {
			return nil, fmt.Errorf("read coverage data: %w", err)
		}
		rel, ok := repoRelativePython(file, root)
		if !ok {
			continue
		}
		if data[context.String] == nil {
			data[context.String] = map[string][]int{}
		}
		data[context.String][rel] = mergeLines(data[context.String][rel], decodeNumbits(bits))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read coverage data: %w", err)
	}
	return data, nil
}

// repoRelativePython converts a measured file path to a repo-relative one.
func repoRelativePython(file, root string) (string, bool) {
	p := filepath.ToSlash(file)
	rootSlash := strings.TrimSuffix(filepath.ToSlash(root), "/")
	switch {
	case len(p) > len(rootSlash) && strings.EqualFold(p[:len(rootSlash)], rootSlash) && p[len(rootSlash)] == '/':
		p = p[len(rootSlash)+1:]
	case filepath.IsAbs(file) || strings.HasPrefix(p, "/") || regexp.MustCompile(`^[A-Za-z]:/`).MatchString(p):
		return "", false
	}
	p = path.Clean(p)
	if p == ".." || strings.HasPrefix(p, "../") || strings.Contains(p, "site-packages") {
		return "", false
	}
	for _, segment := range strings.Split(p, "/") {
		if strings.HasPrefix(segment, ".") {
			return "", false
		}
	}
	return p, true
}

func mergeLines(a, b []int) []int {
	seen := map[int]bool{}
	var out []int
	for _, l := range append(append([]int{}, a...), b...) {
		if !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	sort.Ints(out)
	return out
}

// attributePython maps coverage contexts to the collected Python tests. Lines
// run outside any test function (imports, fixtures, conftest) and lines of
// contexts that match no collected test cannot be tied to a test, so they are
// attributed to every test that ran: a changed module-level line may affect any
// of them. A test without a context did not run and is "not_run" (unknown).
func attributePython(data pyLines, tests []Test) (map[string]TestCoverage, []string) {
	byKey := map[string][]int{}
	for i, t := range tests {
		name, _, _ := strings.Cut(t.Name, "[")
		key := t.Package + "." + name
		byKey[key] = append(byKey[key], i)
	}
	shared := map[string][]int{}
	own := make([]map[string][]int, len(tests))
	ran := make([]bool, len(tests))
	var diags []string
	contexts := make([]string, 0, len(data))
	for c := range data {
		contexts = append(contexts, c)
	}
	sort.Strings(contexts)
	for _, c := range contexts {
		var matched []int
		if c != "" {
			for key, idx := range byKey {
				if key == c || strings.HasSuffix(key, "."+c) || strings.HasSuffix(c, "."+key) {
					matched = append(matched, idx...)
				}
			}
		}
		if len(matched) == 0 {
			if c != "" {
				diags = append(diags, fmt.Sprintf("python coverage: context %q matches no collected test; its lines count for every test", c))
			}
			for file, lines := range data[c] {
				shared[file] = mergeLines(shared[file], lines)
			}
			continue
		}
		for _, i := range matched {
			ran[i] = true
			if own[i] == nil {
				own[i] = map[string][]int{}
			}
			for file, lines := range data[c] {
				own[i][file] = mergeLines(own[i][file], lines)
			}
		}
	}
	out := map[string]TestCoverage{}
	for i, t := range tests {
		if !ran[i] {
			out[t.ID] = TestCoverage{Status: "not_run"}
			continue
		}
		lines := map[string][]int{}
		for file, ls := range shared {
			lines[file] = mergeLines(lines[file], ls)
		}
		for file, ls := range own[i] {
			lines[file] = mergeLines(lines[file], ls)
		}
		out[t.ID] = TestCoverage{Status: "covered", Blocks: lineBlocks(lines)}
	}
	return out, diags
}

// lineBlocks turns executed lines into sorted blocks of consecutive lines.
func lineBlocks(lines map[string][]int) []Block {
	files := make([]string, 0, len(lines))
	for f := range lines {
		files = append(files, f)
	}
	sort.Strings(files)
	var blocks []Block
	for _, f := range files {
		ls := lines[f]
		for i := 0; i < len(ls); {
			j := i
			for j+1 < len(ls) && ls[j+1] == ls[j]+1 {
				j++
			}
			blocks = append(blocks, Block{File: f, StartLine: ls[i], EndLine: ls[j]})
			i = j + 1
		}
	}
	return blocks
}

var coverageVersion = regexp.MustCompile(`version (\S+)`)

// collectPythonCoverage runs the pytest suite under coverage.py with one
// dynamic context per test function. A failing run makes coverage unavailable.
func collectPythonCoverage(ctx context.Context, root, python string, cfg config.Config, m *Manifest) (goCoverageResult, error) {
	var res goCoverageResult
	var tests []Test
	for _, t := range m.Tests {
		if t.Language == "python" {
			tests = append(tests, t)
		}
	}
	var testpaths []string
	for _, tp := range cfg.Languages.Python.Testpaths {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(tp))); err == nil {
			testpaths = append(testpaths, tp)
		}
	}
	if len(tests) == 0 || len(testpaths) == 0 {
		return res, nil
	}
	unavailable := func(reason string) (goCoverageResult, error) {
		res.Diagnostics = append(res.Diagnostics, "python coverage unavailable: "+reason)
		return res, nil
	}
	version, err := runCommand(ctx, root, pythonEnv, python, "-m", "coverage", "--version")
	if err != nil {
		return res, err
	}
	if v := coverageVersion.FindStringSubmatch(string(version.Stdout)); version.Command.Exit == 0 && v != nil {
		m.Tools["coverage"] = v[1]
	} else {
		return unavailable("coverage.py is not installed for " + python)
	}
	tmp, err := os.MkdirTemp("", "hazard-pycover-*")
	if err != nil {
		return res, err
	}
	defer os.RemoveAll(tmp)
	dbPath := filepath.Join(tmp, "coverage.db")
	rc := filepath.Join(tmp, "coveragerc")
	rcText := "[run]\ndata_file = " + filepath.ToSlash(dbPath) + "\ndynamic_context = test_function\nrelative_files = True\n"
	if err := os.WriteFile(rc, []byte(rcText), 0o600); err != nil {
		return res, err
	}
	argv := append([]string{python, "-m", "coverage", "run", "--rcfile=" + rc, "-m", "pytest", "-q", "-p", "no:cacheprovider"}, testpaths...)
	run, err := runCommand(ctx, root, pythonEnv, argv...)
	res.Commands = append(res.Commands, run.Command)
	if err != nil {
		return res, err
	}
	if run.Command.Exit != 0 {
		return unavailable(fmt.Sprintf("the pytest run exited with status %d: %s", run.Command.Exit, tail(string(run.Stdout)+string(run.Stderr))))
	}
	data, err := readCoverageDB(dbPath, root)
	if err != nil {
		return unavailable("cannot read the coverage data file: " + err.Error())
	}
	perTest, diags := attributePython(data, tests)
	res.Diagnostics = append(res.Diagnostics, diags...)
	cov := &Coverage{
		HeadSHA:       m.HeadSHA,
		ContentDigest: m.ContentDigest,
		Granularity:   pythonCoverageGranularity,
		Tests:         perTest,
		FileDigests:   map[string]string{},
	}
	for _, files := range data {
		for rel := range files {
			digest, ok := m.Files[rel]
			if !ok {
				content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
				if err != nil {
					return res, fmt.Errorf("hash instrumented file %s: %w", rel, err)
				}
				digest = sha(content)
			}
			cov.FileDigests[rel] = digest
		}
	}
	res.Coverage = cov
	return res, nil
}
