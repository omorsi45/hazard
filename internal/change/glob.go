package change

import (
	"path"
	"strings"
)

// MatchGlob reports whether the slash-separated path matches pattern.
//
// A pattern without "/" is compared with the basename of path, so "go.mod"
// matches the root and nested go.mod files and "Dockerfile*" matches
// "Dockerfile.dev" at any depth. A pattern with "/" is anchored at the root and
// matched segment by segment: "*", "?" and "[...]" stay inside one segment and
// "**" matches zero or more whole directories (a trailing "**" needs at least
// one more segment). A malformed pattern never matches.
func MatchGlob(pattern, p string) bool {
	if pattern == "" || p == "" {
		return false
	}
	if !strings.Contains(pattern, "/") {
		ok, err := path.Match(pattern, p[strings.LastIndex(p, "/")+1:])
		return err == nil && ok
	}
	return matchSegments(strings.Split(pattern, "/"), strings.Split(p, "/"))
}

func matchSegments(pat, segs []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			if len(pat) == 1 {
				return len(segs) > 0
			}
			for skip := 0; skip <= len(segs); skip++ {
				if matchSegments(pat[1:], segs[skip:]) {
					return true
				}
			}
			return false
		}
		if len(segs) == 0 {
			return false
		}
		ok, err := path.Match(pat[0], segs[0])
		if err != nil || !ok {
			return false
		}
		pat, segs = pat[1:], segs[1:]
	}
	return len(segs) == 0
}
