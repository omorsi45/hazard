package change

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

// diffEntry is one path of a git diff: the raw record joined with the hunks
// of its patch block(s).
type diffEntry struct {
	status           byte // 'A', 'M', 'D', 'R' or 'T'
	oldPath, path    string
	oldMode, newMode string
	oldOID, newOID   string
	binary           bool
	hunks            []Hunk
}

type patchBlock struct {
	binary bool
	hunks  []Hunk
}

// parseDiff parses the output of `git diff --raw -p -z`: NUL-terminated raw
// records, an empty field, then the patch text. Every path comes from the raw
// records, so no patch header is ever parsed for a file name. A type change is
// one raw record and two patch blocks (a deletion and an addition).
func parseDiff(out []byte) ([]diffEntry, error) {
	var entries []diffEntry
	rest := out
	for len(rest) > 0 && rest[0] == ':' {
		var meta, p1, p2 []byte
		meta, rest = cutNUL(rest)
		fields := strings.Fields(string(meta[1:]))
		if len(fields) != 5 || fields[4] == "" {
			return nil, fmt.Errorf("malformed diff record %q", meta)
		}
		status := fields[4][0]
		e := diffEntry{status: status, oldMode: fields[0], newMode: fields[1], oldOID: fields[2], newOID: fields[3]}
		if len(rest) == 0 {
			return nil, fmt.Errorf("diff record %q has no path", meta)
		}
		p1, rest = cutNUL(rest)
		e.path = string(p1)
		switch status {
		case 'R':
			if len(rest) == 0 {
				return nil, fmt.Errorf("rename record %q has no destination", meta)
			}
			p2, rest = cutNUL(rest)
			e.oldPath, e.path = e.path, string(p2)
		case 'A', 'M', 'D', 'T':
		case 'U':
			return nil, fmt.Errorf("unmerged path %q: resolve the conflict first", e.path)
		default:
			return nil, fmt.Errorf("unsupported diff status %q for %q", status, e.path)
		}
		entries = append(entries, e)
	}
	if len(entries) == 0 {
		if len(bytes.TrimSpace(out)) != 0 {
			return nil, fmt.Errorf("unexpected diff output %q", stderrTail(string(out)))
		}
		return nil, nil
	}
	if len(rest) > 0 && rest[0] == 0 {
		rest = rest[1:]
	}
	blocks, err := parsePatch(rest)
	if err != nil {
		return nil, err
	}
	next := 0
	take := func() (patchBlock, error) {
		if next >= len(blocks) {
			return patchBlock{}, fmt.Errorf("diff has %d patch blocks for %d changed paths", len(blocks), len(entries))
		}
		next++
		return blocks[next-1], nil
	}
	for i := range entries {
		b, err := take()
		if err != nil {
			return nil, err
		}
		entries[i].binary, entries[i].hunks = b.binary, b.hunks
		if entries[i].status == 'T' {
			second, err := take()
			if err != nil {
				return nil, err
			}
			entries[i].binary = entries[i].binary || second.binary
			entries[i].hunks = append(entries[i].hunks, second.hunks...)
		}
	}
	if next != len(blocks) {
		return nil, fmt.Errorf("diff has %d patch blocks for %d changed paths", len(blocks), len(entries))
	}
	return entries, nil
}

func cutNUL(b []byte) (token, rest []byte) {
	i := bytes.IndexByte(b, 0)
	if i < 0 {
		return b, nil
	}
	return b[:i], b[i+1:]
}

// parsePatch splits patch text into one block per "diff --git" header and
// reads the hunk headers and the binary marker of each. File contents inside
// hunks are prefixed with '+', '-' or ' ', so no content line can look like a
// header.
func parsePatch(patch []byte) ([]patchBlock, error) {
	var blocks []patchBlock
	for len(patch) > 0 {
		var line []byte
		line, patch, _ = bytes.Cut(patch, []byte("\n"))
		switch {
		case bytes.HasPrefix(line, []byte("diff --git ")):
			blocks = append(blocks, patchBlock{})
		case len(blocks) == 0:
			return nil, fmt.Errorf("patch text before the first diff header: %q", stderrTail(string(line)))
		case bytes.HasPrefix(line, []byte("@@ ")):
			h, err := parseHunkHeader(string(line))
			if err != nil {
				return nil, err
			}
			last := &blocks[len(blocks)-1]
			last.hunks = append(last.hunks, h)
		case bytes.HasPrefix(line, []byte("Binary files ")) && bytes.HasSuffix(line, []byte(" differ")):
			blocks[len(blocks)-1].binary = true
		}
	}
	return blocks, nil
}

// parseHunkHeader parses "@@ -a[,b] +c[,d] @@ ...". An omitted count is 1.
func parseHunkHeader(line string) (Hunk, error) {
	bad := fmt.Errorf("malformed hunk header %q", line)
	rest := strings.TrimPrefix(line, "@@ ")
	end := strings.Index(rest, " @@")
	if end < 0 {
		return Hunk{}, bad
	}
	oldPart, newPart, ok := strings.Cut(rest[:end], " ")
	if !ok || !strings.HasPrefix(oldPart, "-") || !strings.HasPrefix(newPart, "+") {
		return Hunk{}, bad
	}
	oldStart, oldLines, err := parseRange(oldPart[1:])
	if err != nil {
		return Hunk{}, bad
	}
	newStart, newLines, err := parseRange(newPart[1:])
	if err != nil {
		return Hunk{}, bad
	}
	return Hunk{OldStart: oldStart, OldLines: oldLines, NewStart: newStart, NewLines: newLines}, nil
}

func parseRange(s string) (start, lines int, err error) {
	first, count, hasCount := strings.Cut(s, ",")
	if start, err = strconv.Atoi(first); err != nil {
		return 0, 0, err
	}
	lines = 1
	if hasCount {
		if lines, err = strconv.Atoi(count); err != nil {
			return 0, 0, err
		}
	}
	return start, lines, nil
}

// statusReport is the part of `git status` Extract needs.
type statusReport struct {
	dirty     bool
	untracked []string
	unmerged  []string
}

// parseStatus parses `git status --porcelain=v1 -z --no-renames`.
func parseStatus(out []byte) (statusReport, error) {
	var r statusReport
	rest := out
	for len(rest) > 0 {
		var tok []byte
		tok, rest = cutNUL(rest)
		if len(tok) < 4 || tok[2] != ' ' {
			return r, fmt.Errorf("malformed status entry %q", tok)
		}
		x, y, path := tok[0], tok[1], string(tok[3:])
		switch {
		case x == '!' && y == '!':
			continue
		case x == 'U' || y == 'U' || (x == 'A' && y == 'A') || (x == 'D' && y == 'D'):
			r.unmerged = append(r.unmerged, path)
		case x == '?' && y == '?':
			r.untracked = append(r.untracked, path)
		}
		r.dirty = true
	}
	return r, nil
}
