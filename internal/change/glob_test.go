package change

import "testing"

func TestMatchGlob(t *testing.T) {
	tests := []struct {
		pattern, path string
		want          bool
	}{
		// A pattern without "/" matches the basename at any depth.
		{"go.mod", "go.mod", true},
		{"go.mod", "sub/go.mod", true},
		{"go.mod", "a/b/c/go.mod", true},
		{"go.mod", "go.modx", false},
		{"go.mod", "xgo.mod", false},
		{"go.mod", "sub/go.mod/file", false},
		{"Dockerfile*", "Dockerfile", true},
		{"Dockerfile*", "Dockerfile.dev", true},
		{"Dockerfile*", "deploy/Dockerfile.prod", true},
		{"Dockerfile*", "MyDockerfile", false},
		{"*.md", "README.md", true},
		{"*.md", "docs/guide/intro.md", true},
		{"*.md", "README.mdx", false},
		{"Makefile", "build/Makefile", true},
		// "?" and character classes work inside a segment.
		{"file?.txt", "file1.txt", true},
		{"file?.txt", "file12.txt", false},
		{"file[ab].txt", "filea.txt", true},
		{"file[ab].txt", "filec.txt", false},
		// A pattern with "/" is anchored at the repository root.
		{".github/workflows/**", ".github/workflows/ci.yml", true},
		{".github/workflows/**", ".github/workflows/nested/deep/ci.yml", true},
		{".github/workflows/**", "sub/.github/workflows/ci.yml", false},
		{".github/workflows/**", ".github/workflows", false},
		{".github/workflows/**", ".github/other/ci.yml", false},
		{"docs/**", "docs/a.txt", true},
		{"docs/**", "other/docs/a.txt", false},
		{"docs/*.md", "docs/a.md", true},
		{"docs/*.md", "docs/sub/a.md", false},
		// "**" spans zero or more directories.
		{"**/conftest.py", "conftest.py", true},
		{"**/conftest.py", "tests/conftest.py", true},
		{"**/conftest.py", "a/b/c/conftest.py", true},
		{"**/conftest.py", "tests/conftest.pyc", false},
		{"**/testdata/**", "testdata/x.yaml", true},
		{"**/testdata/**", "pkg/testdata/x.yaml", true},
		{"**/testdata/**", "pkg/sub/testdata/deep/x.yaml", true},
		{"**/testdata/**", "pkg/testdata", false},
		{"**/testdata/**", "pkg/notestdata/x.yaml", false},
		{"**/*.pb.go", "api.pb.go", true},
		{"**/*.pb.go", "api/v1/api.pb.go", true},
		{"**/*.pb.go", "api/v1/api.go", false},
		{"**/generated.py", "generated.py", true},
		{"**/generated.py", "src/gen/generated.py", true},
		{"**/*_gen.go", "x_gen.go", true},
		{"**/*_gen.go", "a/b/x_gen.go", true},
		{"a/**/z.txt", "a/z.txt", true},
		{"a/**/z.txt", "a/b/c/z.txt", true},
		{"a/**/z.txt", "b/a/z.txt", false},
		// "*" never crosses a directory separator.
		{"a/*/z.txt", "a/b/z.txt", true},
		{"a/*/z.txt", "a/b/c/z.txt", false},
		// Paths with spaces and non-ASCII characters.
		{"*.go", "dir with space/héllo wörld.go", true},
		{"**/dir with space/**", "x/dir with space/file.txt", true},
		// Malformed patterns never match.
		{"[", "[", false},
		{"a/[/b", "a/[/b", false},
		{"", "go.mod", false},
		{"go.mod", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.pattern+"|"+tc.path, func(t *testing.T) {
			if got := MatchGlob(tc.pattern, tc.path); got != tc.want {
				t.Errorf("MatchGlob(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
			}
		})
	}
}
