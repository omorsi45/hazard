package graph

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// UnresolvedError lists modules whose imports could not be resolved or scanned
// reliably. LoadPython still returns the graph with those modules marked
// Dynamic, so callers treat them as unknown dependencies.
type UnresolvedError struct {
	Modules map[string]string // module ID -> reason
}

func (e *UnresolvedError) Error() string {
	ids := make([]string, 0, len(e.Modules))
	for id := range e.Modules {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = id + ": " + e.Modules[id]
	}
	return "unresolved Python imports: " + strings.Join(parts, "; ")
}

var pyIdentifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type pyFile struct {
	rel      string // repo-relative path
	module   string // dotted module name relative to its root
	isInit   bool
	isTest   bool
	id       string
	pkgScope string // dotted package used to resolve relative imports
}

// LoadPython builds the Python module graph with a constrained import parser.
// A module is any .py file under the roots, named relative to the longest root
// containing it. Dynamic imports, unresolvable relative imports and sources that
// cannot be scanned reliably mark the module Dynamic; the last two are also
// reported through an *UnresolvedError returned together with the graph.
func LoadPython(repoDir string, roots []string) (*Graph, error) {
	roots, err := cleanRoots(repoDir, roots)
	if err != nil {
		return nil, err
	}
	files, err := collectPython(repoDir, roots)
	if err != nil {
		return nil, err
	}
	g := &Graph{Packages: map[string]*Package{}}
	byModule := map[string]string{}
	byFile := map[string]string{}
	for i := range files {
		f := &files[i]
		f.id = "py:" + f.module
		byModule[f.module] = f.id
		byFile[f.rel] = f.id
		p := g.Packages[f.id]
		if p == nil {
			p = &Package{ID: f.id, Language: "python", Dir: path.Dir(f.rel)}
			g.Packages[f.id] = p
		}
		if f.isTest {
			p.TestFiles = append(p.TestFiles, f.rel)
		} else {
			p.Files = append(p.Files, f.rel)
		}
		if f.isInit {
			f.pkgScope = f.module
		} else {
			f.pkgScope = parentModule(f.module)
		}
	}
	unresolved := &UnresolvedError{Modules: map[string]string{}}
	edges := map[string]map[string]bool{}
	testEdges := map[string]map[string]bool{}
	external := map[string]map[string]bool{}
	for _, f := range files {
		src, err := os.ReadFile(filepath.Join(repoDir, filepath.FromSlash(f.rel)))
		if err != nil {
			return nil, err
		}
		p := g.Packages[f.id]
		imports, dynamic, ok := scanPython(src)
		if dynamic {
			p.Dynamic = true
		}
		if !ok {
			p.Dynamic = true
			unresolved.Modules[f.id] = f.rel + ": source could not be scanned reliably"
		}
		target := edges
		if f.isTest {
			target = testEdges
		}
		if target[f.id] == nil {
			target[f.id] = map[string]bool{}
		}
		if external[f.id] == nil {
			external[f.id] = map[string]bool{}
		}
		r := &pyResolver{byModule: byModule, byFile: byFile, dir: path.Dir(f.rel), out: target[f.id]}
		for _, imp := range imports {
			if reason := r.resolve(f, imp); reason != "" {
				p.Dynamic = true
				unresolved.Modules[f.id] = f.rel + ": " + reason
			} else if r.external != "" {
				external[f.id][r.external] = true
			}
		}
	}
	for id, p := range g.Packages {
		p.Imports = sortedKeys(edges[id], id)
		p.TestImports = sortedKeys(testEdges[id], id)
		p.External = sortedKeys(external[id], "")
		sort.Strings(p.Files)
		sort.Strings(p.TestFiles)
	}
	if len(unresolved.Modules) > 0 {
		return g, unresolved
	}
	return g, nil
}

type pyResolver struct {
	byModule map[string]string
	byFile   map[string]string
	dir      string
	out      map[string]bool
	external string
}

// resolve records the local modules imp depends on. It returns a reason when a
// relative import cannot be resolved. r.external is set for a non-local
// absolute import.
func (r *pyResolver) resolve(f pyFile, imp pyImport) string {
	r.external = ""
	if imp.Level > 0 {
		return r.resolveRelative(f, imp)
	}
	found := r.addWithParents(imp.Module)
	if imp.From {
		for _, name := range imp.Names {
			if name != "*" && r.addWithParents(imp.Module+"."+name) {
				found = true
			}
		}
	}
	// A directory without __init__.py is on sys.path next to its tests, so a
	// sibling module can satisfy the import too.
	if r.addSibling(imp.Module) {
		found = true
	}
	if imp.From {
		for _, name := range imp.Names {
			if name != "*" && r.addSibling(imp.Module+"."+name) {
				found = true
			}
		}
	}
	if !found {
		r.external = imp.Module
	}
	return ""
}

func (r *pyResolver) resolveRelative(f pyFile, imp pyImport) string {
	scope := strings.Split(f.pkgScope, ".")
	if f.pkgScope == "" {
		scope = nil
	}
	up := imp.Level - 1
	if up >= len(scope) {
		return fmt.Sprintf("relative import beyond the top-level package (level %d)", imp.Level)
	}
	base := strings.Join(scope[:len(scope)-up], ".")
	target := base
	if imp.Module != "" {
		target = base + "." + imp.Module
	}
	found := false
	if _, ok := r.byModule[target]; ok {
		found = r.addWithParents(target)
	}
	for _, name := range imp.Names {
		if name != "*" && r.addWithParents(target+"."+name) {
			found = true
		}
	}
	if !found {
		return fmt.Sprintf("relative import of %q does not resolve to a local module", strings.Repeat(".", imp.Level)+imp.Module)
	}
	return ""
}

// addWithParents adds name and every existing parent package, because importing
// a submodule executes the parents first. It reports whether name itself exists.
func (r *pyResolver) addWithParents(name string) bool {
	if name == "" {
		return false
	}
	parts := strings.Split(name, ".")
	for i := 1; i <= len(parts); i++ {
		if id, ok := r.byModule[strings.Join(parts[:i], ".")]; ok {
			r.out[id] = true
		}
	}
	_, ok := r.byModule[name]
	return ok
}

func (r *pyResolver) addSibling(dotted string) bool {
	if dotted == "" {
		return false
	}
	base := path.Join(r.dir, strings.ReplaceAll(dotted, ".", "/"))
	for _, candidate := range []string{base + ".py", base + "/__init__.py"} {
		if id, ok := r.byFile[candidate]; ok {
			r.out[id] = true
			return true
		}
	}
	return false
}

func parentModule(module string) string {
	if i := strings.LastIndexByte(module, '.'); i >= 0 {
		return module[:i]
	}
	return ""
}

func sortedKeys(set map[string]bool, omit string) []string {
	var out []string
	for k := range set {
		if k != omit {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func cleanRoots(repoDir string, roots []string) ([]string, error) {
	if len(roots) == 0 {
		roots = []string{"."}
	}
	seen := map[string]bool{}
	var out []string
	for _, root := range roots {
		clean := path.Clean(filepath.ToSlash(root))
		if filepath.IsAbs(root) || strings.HasPrefix(clean, "/") || clean == ".." || strings.HasPrefix(clean, "../") {
			return nil, fmt.Errorf("python root %q must be a relative path inside the repository", root)
		}
		info, err := os.Stat(filepath.Join(repoDir, filepath.FromSlash(clean)))
		if err != nil {
			return nil, fmt.Errorf("python root %q: %w", root, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("python root %q is not a directory", root)
		}
		if !seen[clean] {
			seen[clean] = true
			out = append(out, clean)
		}
	}
	// Longest root first so a file is named relative to its most specific root.
	sort.SliceStable(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out, nil
}

var skipPythonDirs = map[string]bool{"__pycache__": true, "node_modules": true, "site-packages": true}

func collectPython(repoDir string, roots []string) ([]pyFile, error) {
	seen := map[string]bool{}
	var files []pyFile
	for _, root := range roots {
		start := filepath.Join(repoDir, filepath.FromSlash(root))
		err := filepath.WalkDir(start, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				name := d.Name()
				if p != start && (strings.HasPrefix(name, ".") || skipPythonDirs[name]) {
					return filepath.SkipDir
				}
				if _, statErr := os.Stat(filepath.Join(p, "pyvenv.cfg")); statErr == nil {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(p, ".py") {
				return nil
			}
			rel, err := filepath.Rel(repoDir, p)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if seen[rel] {
				return nil
			}
			seen[rel] = true
			if f, ok := newPyFile(rel, roots); ok {
				files = append(files, f)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })
	return files, nil
}

// newPyFile names rel relative to the longest root containing it. Files whose
// path is not a valid dotted module name cannot be imported and are skipped.
func newPyFile(rel string, roots []string) (pyFile, bool) {
	sub := rel
	for _, root := range roots {
		if root == "." {
			continue
		}
		if strings.HasPrefix(rel, root+"/") {
			sub = strings.TrimPrefix(rel, root+"/")
			break
		}
	}
	segments := strings.Split(strings.TrimSuffix(sub, ".py"), "/")
	for _, s := range segments {
		if !pyIdentifier.MatchString(s) {
			return pyFile{}, false
		}
	}
	f := pyFile{rel: rel}
	if segments[len(segments)-1] == "__init__" {
		f.isInit = true
		segments = segments[:len(segments)-1]
		if len(segments) == 0 {
			return pyFile{}, false
		}
	}
	f.module = strings.Join(segments, ".")
	base := path.Base(rel)
	f.isTest = strings.HasPrefix(base, "test_") || strings.HasSuffix(base, "_test.py")
	return f, true
}
