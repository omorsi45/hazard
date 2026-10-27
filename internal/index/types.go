// Package index builds and publishes the test inventory, dependency graph and
// optional per-test coverage that selection reads instead of running a suite.
package index

import (
	"time"

	"github.com/omorsi45/hazard/internal/graph"
)

// SchemaVersion is the manifest schema version written by Publish.
const SchemaVersion = 1

// Test describes one discovered test.
type Test struct {
	ID, Language, Package, Name, File, RunnerID string
	Line                                        int
	Unstable                                    bool
	Purpose                                     string   // Go: first doc-comment sentence, else name split on CamelCase; Python: docstring first line, else name
	Asserts                                     []string // up to 5 static assertion targets, deterministic order
}

// Command records one external command the build ran.
type Command struct {
	Argv    []string
	Dir     string
	Exit    int
	Seconds float64
}

// Block is a covered line range of a source file.
type Block struct {
	File      string
	StartLine int
	EndLine   int
}

// TestCoverage is the coverage outcome of one test.
type TestCoverage struct {
	Status string // "covered", "failed" or "not_run"
	Blocks []Block
}

// Coverage is per-test coverage collected at one tree.
type Coverage struct {
	HeadSHA, ContentDigest string
	Granularity            string                  // "go-top-level-test", "python-test-context" or both, sorted and joined with ","
	Tests                  map[string]TestCoverage // by test ID
	FileDigests            map[string]string       // sha256 of each instrumented file at collection time
}

// Manifest is the schema-versioned index published under .hazard/index.
type Manifest struct {
	Schema        int
	RepoRoot      string // absolute, forward slashes
	HeadSHA       string
	Dirty         bool
	ContentDigest string // sha256 over sorted "path\x00sha256\n" of Files
	ConfigDigest  string
	CreatedAt     time.Time
	Tools         map[string]string // "go","python","pytest","coverage" versions when used
	Files         map[string]string // every enabled-language source/test/config file considered (rel path -> sha256)
	Graph         *graph.Graph
	Tests         []Test // sorted by ID
	Coverage      *Coverage
	Commands      []Command
	Diagnostics   []string
	Complete      bool // false => engine must run the full suite
}

// Options configures Build.
type Options struct {
	Coverage bool
	Python   string // interpreter path, "" => "python" on PATH
}
