package graph

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// goListFields limits the JSON output to what LoadGo reads. Without a field
// list go list also computes staleness for every dependency, which is slow and
// unused here.
const goListFields = "Dir,ImportPath,GoFiles,CgoFiles,TestGoFiles,XTestGoFiles,Imports,TestImports,XTestImports,Module,Error,DepsErrors,DepsErrorCount"

// GoListArgs returns the go command arguments LoadGo runs.
func GoListArgs(patterns, tags []string) []string {
	args := []string{"list", "-json=" + goListFields}
	if len(tags) > 0 {
		args = append(args, "-tags", strings.Join(tags, ","))
	}
	return append(args, patterns...)
}

type goListModule struct {
	Path string
	Main bool
}

type goListError struct{ Err string }

type goListPackage struct {
	Dir            string
	ImportPath     string
	GoFiles        []string
	CgoFiles       []string
	TestGoFiles    []string
	XTestGoFiles   []string
	Imports        []string
	TestImports    []string
	XTestImports   []string
	Module         *goListModule
	Error          *goListError
	DepsErrors     []*goListError
	DepsErrorCount int
}

// LoadGo loads the main-module Go package graph with go list -json. Any
// package error, dependency error or nonzero exit is an error: a partial graph
// would silently hide dependencies.
func LoadGo(ctx context.Context, repoDir string, patterns, tags []string) (*Graph, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "go", GoListArgs(patterns, tags)...)
	cmd.Dir = repoDir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("go list: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var listed []goListPackage
	decoder := json.NewDecoder(&stdout)
	for {
		var p goListPackage
		if err := decoder.Decode(&p); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("decode go list output: %w", err)
		}
		listed = append(listed, p)
	}
	modulePath := ""
	for _, p := range listed {
		if p.Module != nil && p.Module.Main {
			modulePath = p.Module.Path
			break
		}
	}
	local := func(importPath string) bool {
		return modulePath != "" && (importPath == modulePath || strings.HasPrefix(importPath, modulePath+"/"))
	}
	root, err := resolvedDir(repoDir)
	if err != nil {
		return nil, err
	}
	g := &Graph{Packages: map[string]*Package{}}
	for _, p := range listed {
		isLocal := (p.Module != nil && p.Module.Main) || local(p.ImportPath)
		if !isLocal {
			continue
		}
		if p.Error != nil {
			return nil, fmt.Errorf("package %s: %s", p.ImportPath, p.Error.Err)
		}
		if len(p.DepsErrors) > 0 || p.DepsErrorCount > 0 {
			return nil, fmt.Errorf("package %s: dependency error", p.ImportPath)
		}
		rel, err := relativeTo(root, p.Dir)
		if err != nil {
			return nil, fmt.Errorf("package %s: %w", p.ImportPath, err)
		}
		pkg := &Package{ID: p.ImportPath, Language: "go", Dir: rel}
		for _, name := range append(append([]string{}, p.GoFiles...), p.CgoFiles...) {
			pkg.Files = append(pkg.Files, joinRel(rel, name))
		}
		for _, name := range append(append([]string{}, p.TestGoFiles...), p.XTestGoFiles...) {
			pkg.TestFiles = append(pkg.TestFiles, joinRel(rel, name))
		}
		sort.Strings(pkg.Files)
		sort.Strings(pkg.TestFiles)
		external := map[string]bool{}
		classify := func(imports []string, self string) []string {
			seen := map[string]bool{}
			var localImports []string
			for _, imp := range imports {
				switch {
				case local(imp):
					if imp != self && !seen[imp] {
						seen[imp] = true
						localImports = append(localImports, imp)
					}
				default:
					external[imp] = true
				}
			}
			sort.Strings(localImports)
			return localImports
		}
		pkg.Imports = classify(p.Imports, p.ImportPath)
		pkg.TestImports = classify(append(append([]string{}, p.TestImports...), p.XTestImports...), p.ImportPath)
		for imp := range external {
			pkg.External = append(pkg.External, imp)
		}
		sort.Strings(pkg.External)
		g.Packages[pkg.ID] = pkg
	}
	return g, nil
}

func resolvedDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	return abs, nil
}

// relativeTo returns dir relative to root with forward slashes ("." for root).
func relativeTo(root, dir string) (string, error) {
	resolved, err := resolvedDir(dir)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil {
		return "", err
	}
	rel = filepath.ToSlash(rel)
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("directory %s is outside the repository", dir)
	}
	return rel, nil
}

func joinRel(dir, name string) string {
	if dir == "." {
		return name
	}
	return path.Join(dir, name)
}
