package change

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// fileLevel is the symbol of a range that touches no declaration.
const fileLevel = "__file__"

// Extract computes the ChangeSet of repoDir.
//
// With opts.Head empty the head is the working tree: staged, unstaged and
// untracked non-ignored files, with Dirty set when any exists. An untracked file
// is an addition with every line added. With opts.Head set the head is that
// immutable revision and the working tree is ignored. HeadSHA is the resolved
// head commit (HEAD for the working tree).
//
// With opts.Base empty the base is the merge base of the head commit with
// origin/<DefaultBranch>, else <DefaultBranch> ("main" when empty). When neither
// yields a trustworthy merge base, including a shallow clone whose history is
// cut at it, the error wraps ErrNoMergeBase. An explicit Base is used as given
// and failing to resolve it is an ordinary error.
//
// Removed lines are attributed to declarations of the base contents and added
// lines to declarations of the head contents (see Hunk). Every git failure is
// returned wrapped with its argv and stderr; none becomes an empty change.
func Extract(ctx context.Context, repoDir string, opts Options) (cs *ChangeSet, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, ref := range []string{opts.Base, opts.Head} {
		if strings.HasPrefix(ref, "-") {
			return nil, fmt.Errorf("invalid ref %q: a ref must not start with '-'", ref)
		}
	}
	workTree := opts.Head == ""

	root, indexPath, shallow, headSHA, baseSHA, err := resolveRefs(ctx, repoDir, opts)
	if err != nil {
		return nil, err
	}
	if baseSHA == "" {
		if baseSHA, err = defaultBase(ctx, root, headSHA, opts.DefaultBranch, shallow); err != nil {
			return nil, err
		}
	}

	// Diffing the working tree makes git refresh stat data in the index it
	// reads. Give it a private copy so the user's index is never rewritten.
	var env []string
	if workTree {
		idx, cleanup, perr := privateIndex(indexPath)
		if perr != nil {
			return nil, perr
		}
		defer func() {
			if cerr := cleanup(); cerr != nil && err == nil {
				cs, err = nil, cerr
			}
		}()
		env = []string{"GIT_INDEX_FILE=" + idx}
	}

	diffArgs := []string{
		"diff", "--no-color", "--no-ext-diff", "--no-textconv", "--ignore-submodules=none",
		"-M", "--no-abbrev", "--raw", "-p", "-z", "-U0", "--src-prefix=a/", "--dst-prefix=b/", baseSHA,
	}
	if !workTree {
		diffArgs = append(diffArgs, headSHA)
	}
	out, err := runGitEnv(ctx, root, env, nil, append(diffArgs, "--")...)
	if err != nil {
		return nil, err
	}
	entries, err := parseDiff(out)
	if err != nil {
		return nil, fmt.Errorf("git diff %s: %w", baseSHA, err)
	}

	var st statusReport
	if workTree {
		statusOut, err := runGitEnv(ctx, root, env, nil, "status", "--porcelain=v1", "-z", "-uall", "--no-renames", "--ignore-submodules=none")
		if err != nil {
			return nil, err
		}
		if st, err = parseStatus(statusOut); err != nil {
			return nil, fmt.Errorf("git status: %w", err)
		}
		if len(st.unmerged) > 0 {
			return nil, fmt.Errorf("unmerged paths: %s; resolve the conflicts first", strings.Join(st.unmerged, ", "))
		}
	}

	files, err := buildFiles(ctx, root, entries, st.untracked, workTree, opts)
	if err != nil {
		return nil, err
	}
	cs = &ChangeSet{
		BaseSHA:      baseSHA,
		HeadSHA:      headSHA,
		Files:        files,
		TriggersFull: matchTriggers(files, opts.FullSuiteTriggers),
		Dirty:        workTree && st.dirty,
	}
	for _, f := range files {
		if f.Binary {
			continue
		}
		for _, h := range f.Hunks {
			cs.LinesAdded += h.NewLines
			cs.LinesRemoved += h.OldLines
		}
	}
	return cs, nil
}

// privateIndex copies the index at indexPath into a temporary directory and
// returns the copy's path. A missing index is an empty index, so the copy is
// then simply absent. cleanup removes the directory.
func privateIndex(indexPath string) (path string, cleanup func() error, err error) {
	dir, err := os.MkdirTemp("", "hazard-index-")
	if err != nil {
		return "", nil, err
	}
	cleanup = func() error { return os.RemoveAll(dir) }
	path = filepath.Join(dir, "index")
	data, err := os.ReadFile(indexPath)
	if errors.Is(err, fs.ErrNotExist) {
		return path, cleanup, nil
	}
	if err == nil {
		err = os.WriteFile(path, data, 0o600)
	}
	if err != nil {
		if cerr := cleanup(); cerr != nil {
			err = errors.Join(err, cerr)
		}
		return "", nil, err
	}
	return path, cleanup, nil
}

// resolveRefs asks git, in one process, for the work tree root, the index file,
// whether the repository is shallow, the head commit and (when explicit) the
// base commit. baseSHA is empty when opts.Base is empty.
func resolveRefs(ctx context.Context, repoDir string, opts Options) (root, indexPath string, shallow bool, headSHA, baseSHA string, err error) {
	headRev := opts.Head
	if headRev == "" {
		headRev = "HEAD"
	}
	args := []string{"rev-parse", "--show-toplevel", "--is-shallow-repository", "--git-path", "index", headRev + "^{commit}"}
	want := 4
	if opts.Base != "" {
		args = append(args, opts.Base+"^{commit}")
		want++
	}
	out, err := runGit(ctx, repoDir, nil, args...)
	if err != nil {
		return "", "", false, "", "", err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != want {
		return "", "", false, "", "", fmt.Errorf("git rev-parse printed %d lines, want %d: %q", len(lines), want, stderrTail(string(out)))
	}
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	root, shallow, indexPath, headSHA = filepath.FromSlash(lines[0]), lines[1] == "true", filepath.FromSlash(lines[2]), lines[3]
	if !filepath.IsAbs(indexPath) {
		indexPath = filepath.Join(repoDir, indexPath)
	}
	if opts.Base != "" {
		baseSHA = lines[4]
	}
	return root, indexPath, shallow, headSHA, baseSHA, nil
}

// defaultBase returns the merge base of headSHA with origin/<branch>, else with
// <branch>. In a shallow repository a merge base that is itself a cut-off commit
// is rejected: the history beneath it is unknown, and trusting it could turn a
// real change into an empty one.
func defaultBase(ctx context.Context, root, headSHA, branch string, shallow bool) (string, error) {
	if branch == "" {
		branch = "main"
	}
	var cutoffs map[string]bool
	var why []string
	for _, ref := range []string{"refs/remotes/origin/" + branch, "refs/heads/" + branch} {
		out, err := runGit(ctx, root, nil, "merge-base", headSHA, ref)
		if err != nil {
			var exit *exec.ExitError
			if ctx.Err() != nil || !errors.As(err, &exit) {
				return "", err
			}
			why = append(why, ref+": no merge base")
			continue
		}
		sha := strings.TrimSpace(string(out))
		if shallow {
			if cutoffs == nil {
				if cutoffs, err = shallowCutoffs(ctx, root); err != nil {
					return "", err
				}
			}
			if cutoffs[sha] {
				why = append(why, fmt.Sprintf("%s: merge base %.12s is a shallow-clone cut-off", ref, sha))
				continue
			}
		}
		return sha, nil
	}
	return "", fmt.Errorf("%w: HEAD has none with origin/%s or %s (%s); fetch the full history or pass an explicit base",
		ErrNoMergeBase, branch, branch, strings.Join(why, "; "))
}

// shallowCutoffs returns the commits whose parents were cut off.
func shallowCutoffs(ctx context.Context, root string) (map[string]bool, error) {
	out, err := runGit(ctx, root, nil, "rev-parse", "--git-path", "shallow")
	if err != nil {
		return nil, err
	}
	p := strings.TrimSpace(string(out))
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, filepath.FromSlash(p))
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, sha := range strings.Fields(string(data)) {
		set[sha] = true
	}
	return set, nil
}

// work is one changed path while it is being built.
type work struct {
	file     ChangedFile
	entry    diffEntry // zero for a plain untracked file
	hasOld   bool      // the base has contents for the file
	hasNew   bool      // the head has contents for the file
	oldPath  string    // path of the base-side contents
	disk     bool      // the head-side contents are the file on disk
	newSrc   []byte    // preloaded head-side contents (untracked files)
	newKnown bool      // newSrc is valid
}

func langOf(path string) string {
	switch filepath.Ext(path) {
	case ".go":
		return "go"
	case ".py":
		return "python"
	}
	return ""
}

func isRegularMode(mode string) bool { return mode == "100644" || mode == "100755" }

func isZeroOID(oid string) bool { return strings.Trim(oid, "0") == "" }

func buildFiles(ctx context.Context, root string, entries []diffEntry, untracked []string, workTree bool, opts Options) ([]ChangedFile, error) {
	var works []*work
	byPath := map[string]*work{}
	for _, e := range entries {
		w := &work{entry: e, oldPath: e.path, disk: workTree}
		w.file = ChangedFile{Path: e.path, Language: langOf(e.path), Binary: e.binary, Hunks: e.hunks}
		switch e.status {
		case 'A':
			w.file.Status, w.hasNew = "A", true
		case 'D':
			w.file.Status, w.hasOld = "D", true
		case 'R':
			w.file.Status, w.file.OldPath, w.hasOld, w.hasNew, w.oldPath = "R", e.oldPath, true, true, e.oldPath
		default: // 'M' and 'T'
			w.file.Status, w.hasOld, w.hasNew = "M", true, true
		}
		works = append(works, w)
		byPath[e.path] = w
	}

	for _, rel := range untracked {
		lang := langOf(rel)
		info, err := scanUntracked(root, rel, lang != "")
		if err != nil {
			return nil, err
		}
		var hunks []Hunk
		if info.lines > 0 && !info.binary {
			hunks = []Hunk{{NewStart: 1, NewLines: info.lines}}
		}
		if w := byPath[rel]; w != nil {
			// Removed from the index but still on disk: the whole file was
			// removed and the whole file added again.
			if w.file.Status != "D" {
				return nil, fmt.Errorf("path %q is both tracked and untracked", rel)
			}
			w.file.Status, w.hasNew, w.disk = "M", true, true
			w.file.Binary = w.file.Binary || info.binary
			w.file.Hunks = append(w.file.Hunks, hunks...)
			w.newSrc, w.newKnown = info.content, info.regular && lang != ""
			continue
		}
		w := &work{hasNew: true, disk: true, newSrc: info.content, newKnown: info.regular && lang != ""}
		w.file = ChangedFile{Path: rel, Status: "A", Language: lang, Binary: info.binary, Hunks: hunks}
		works = append(works, w)
		byPath[rel] = w
	}

	// Fetch every needed blob with one git process.
	needOld := func(w *work) bool {
		return w.hasOld && langOf(w.oldPath) != "" && isRegularMode(w.entry.oldMode) && !isZeroOID(w.entry.oldOID)
	}
	needNewBlob := func(w *work) bool {
		return !w.disk && w.hasNew && w.file.Language != "" && isRegularMode(w.entry.newMode) && !isZeroOID(w.entry.newOID)
	}
	want := map[string]bool{}
	for _, w := range works {
		if needOld(w) {
			want[w.entry.oldOID] = true
		}
		if needNewBlob(w) {
			want[w.entry.newOID] = true
		}
	}
	blobs, err := readBlobs(ctx, root, want)
	if err != nil {
		return nil, err
	}

	files := make([]ChangedFile, 0, len(works))
	for _, w := range works {
		f := w.file
		if f.Binary {
			f.Hunks = nil
		}
		oldLang := langOf(w.oldPath)

		var oldSrc, newSrc []byte
		oldOK, newOK := false, false
		if needOld(w) {
			oldSrc, oldOK = blobs[w.entry.oldOID]
		}
		switch {
		case w.newKnown:
			newSrc, newOK = w.newSrc, true
		case needNewBlob(w):
			newSrc, newOK = blobs[w.entry.newOID]
		case w.disk && w.hasNew && f.Language != "" && (w.entry.newMode == "" || isRegularMode(w.entry.newMode)):
			data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f.Path)))
			newSrc, newOK = data, err == nil
		}

		if len(f.Hunks) > 0 {
			oldSide := newSide(oldLang, oldSrc, oldOK)
			curSide := newSide(f.Language, newSrc, newOK)
			hunks := make([]Hunk, len(f.Hunks))
			for i, h := range f.Hunks {
				h.Symbols = attribute(h, oldSide, curSide, w.hasOld, w.hasNew)
				hunks[i] = h
			}
			f.Hunks = hunks
		}

		classSrc := newSrc
		if !w.hasNew || classSrc == nil {
			classSrc = oldSrc
		}
		f.Kind = Classify(f.Path, classSrc, opts.GeneratedGlobs)
		files = append(files, f)
	}
	sort.SliceStable(files, func(i, j int) bool {
		if files[i].Path != files[j].Path {
			return files[i].Path < files[j].Path
		}
		return files[i].OldPath < files[j].OldPath
	})
	return files, nil
}

// untrackedInfo describes an untracked path.
type untrackedInfo struct {
	regular bool
	binary  bool
	lines   int
	content []byte // only when requested
}

// scanUntracked inspects an untracked path the way git would diff it against
// nothing: a NUL byte in the first 8000 bytes makes it binary, otherwise every
// line counts as added. A trailing "/" is a nested repository and a path that
// is not a regular file (a symlink) is listed without contents.
func scanUntracked(root, rel string, keep bool) (info untrackedInfo, err error) {
	if strings.HasSuffix(rel, "/") {
		return info, nil
	}
	full := filepath.Join(root, filepath.FromSlash(rel))
	fi, err := os.Lstat(full)
	if err != nil {
		return info, err
	}
	if !fi.Mode().IsRegular() {
		return info, nil
	}
	f, err := os.Open(full)
	if err != nil {
		return info, err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	info.regular = true
	r := bufio.NewReaderSize(f, 64<<10)
	head, err := r.Peek(8000)
	if err != nil && !errors.Is(err, io.EOF) {
		return info, err
	}
	if bytes.IndexByte(head, 0) >= 0 {
		info.binary = true
		return info, nil
	}
	if keep {
		if info.content, err = io.ReadAll(r); err != nil {
			return info, err
		}
		info.lines = bytes.Count(info.content, []byte("\n"))
		if n := len(info.content); n > 0 && info.content[n-1] != '\n' {
			info.lines++
		}
		return info, nil
	}
	buf := make([]byte, 64<<10)
	var last byte
	total := 0
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			info.lines += bytes.Count(buf[:n], []byte("\n"))
			last = buf[n-1]
			total += n
		}
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				return info, rerr
			}
			break
		}
	}
	if total > 0 && last != '\n' {
		info.lines++
	}
	return info, nil
}

// readBlobs reads the named objects with `git cat-file --batch`. An object git
// reports as missing is absent from the result.
func readBlobs(ctx context.Context, root string, oids map[string]bool) (map[string][]byte, error) {
	blobs := map[string][]byte{}
	if len(oids) == 0 {
		return blobs, nil
	}
	list := make([]string, 0, len(oids))
	for oid := range oids {
		list = append(list, oid)
	}
	sort.Strings(list)
	out, err := runGit(ctx, root, []byte(strings.Join(list, "\n")+"\n"), "cat-file", "--batch")
	if err != nil {
		return nil, err
	}
	for pos := 0; pos < len(out); {
		nl := bytes.IndexByte(out[pos:], '\n')
		if nl < 0 {
			return nil, fmt.Errorf("git cat-file --batch: truncated header %q", stderrTail(string(out[pos:])))
		}
		fields := strings.Fields(string(out[pos : pos+nl]))
		pos += nl + 1
		if len(fields) == 2 && fields[1] == "missing" {
			continue
		}
		if len(fields) != 3 {
			return nil, fmt.Errorf("git cat-file --batch: malformed header %q", fields)
		}
		size, err := strconv.Atoi(fields[2])
		if err != nil || size < 0 || pos+size > len(out) {
			return nil, fmt.Errorf("git cat-file --batch: bad size in header %q", fields)
		}
		if fields[1] == "blob" {
			blobs[fields[0]] = out[pos : pos+size]
		}
		pos += size + 1 // the object is followed by a newline
	}
	return blobs, nil
}

// side is the declarations of one version of a file. ok is false when the
// contents are unavailable or do not parse, which attributes the whole file.
type side struct {
	intervals []Interval
	ok        bool
}

func newSide(lang string, src []byte, available bool) side {
	if lang == "" {
		return side{ok: true}
	}
	if !available {
		return side{}
	}
	if lang == "go" {
		iv, err := GoSymbols(src)
		if err != nil {
			return side{}
		}
		return side{iv, true}
	}
	return side{PythonSymbols(src), true}
}

// touch adds the declarations overlapping lines [from, to], or the file-level
// symbol when there are none or the side is unusable.
func (s side) touch(from, to int, set map[string]struct{}) {
	if !s.ok {
		set[fileLevel] = struct{}{}
		return
	}
	found := false
	for _, iv := range s.intervals {
		if iv.Start <= to && iv.End >= from {
			set[iv.Name] = struct{}{}
			found = true
		}
	}
	if !found {
		set[fileLevel] = struct{}{}
	}
}

// containing adds the declarations that contain line, if any.
func (s side) containing(line int, set map[string]struct{}) {
	if !s.ok {
		set[fileLevel] = struct{}{}
		return
	}
	for _, iv := range s.intervals {
		if iv.Start <= line && line <= iv.End {
			set[iv.Name] = struct{}{}
		}
	}
}

// attribute names the declarations a hunk touches: the removed range against
// the base contents and the added range against the head contents. For a pure
// insertion the declaration containing the line it follows is also taken from
// the base, so an insertion at a declaration boundary reaches both neighbours.
func attribute(h Hunk, old, cur side, hasOld, hasNew bool) []string {
	set := map[string]struct{}{}
	if hasOld {
		switch {
		case h.OldLines > 0:
			old.touch(h.OldStart, h.OldStart+h.OldLines-1, set)
		case h.OldStart > 0:
			old.containing(h.OldStart, set)
		}
	}
	if hasNew && h.NewLines > 0 {
		cur.touch(h.NewStart, h.NewStart+h.NewLines-1, set)
	}
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// matchTriggers returns "<pattern>:<path>" for every pattern matching the new
// or the old path of a file, sorted and without duplicates.
func matchTriggers(files []ChangedFile, patterns []string) []string {
	seen := map[string]struct{}{}
	for _, f := range files {
		candidates := []string{f.Path}
		if f.OldPath != "" {
			candidates = append(candidates, f.OldPath)
		}
		for _, pattern := range patterns {
			for _, p := range candidates {
				if MatchGlob(pattern, p) {
					seen[pattern+":"+p] = struct{}{}
				}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
