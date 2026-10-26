package change

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fileURL(dir string) string {
	p := filepath.ToSlash(dir)
	if strings.HasPrefix(p, "/") {
		return "file://" + p
	}
	return "file:///" + p
}

// extractHead extracts from the committed HEAD so the working tree does not
// matter, and returns the result.
func extractHead(t *testing.T, dir string, opts Options) *ChangeSet {
	t.Helper()
	opts.Head = "HEAD"
	return mustExtract(t, dir, opts)
}

func TestExtractDefaultBase(t *testing.T) {
	t.Parallel()
	// main is at m1, trunk at m2, feature (HEAD) at m3.
	h := &history{}
	m1 := h.commit(heads("main"), 0, "m1", put("one.txt", "1\n"))
	m2 := h.commit(heads("trunk"), m1, "m2", put("two.txt", "2\n"))
	m3 := h.commit(heads("feature"), m2, "m3", put("three.txt", "3\n"))
	r := newRepo(t, h, "feature", false)

	for _, tc := range []struct {
		name, branch string
		wantBase     int
		wantPaths    []string
	}{
		{"configured default branch", "main", m1, []string{"three.txt", "two.txt"}},
		{"empty means main", "", m1, []string{"three.txt", "two.txt"}},
		{"custom default branch", "trunk", m2, []string{"three.txt"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := testOptions()
			opts.DefaultBranch = tc.branch
			cs := extractHead(t, r.dir, opts)
			if cs.BaseSHA != r.sha(tc.wantBase) || cs.HeadSHA != r.sha(m3) || cs.Dirty {
				t.Errorf("BaseSHA %s HeadSHA %s Dirty %v; want %s %s false", cs.BaseSHA, cs.HeadSHA, cs.Dirty, r.sha(tc.wantBase), r.sha(m3))
			}
			wantPaths(t, cs, tc.wantPaths...)
		})
	}
}

func TestExtractDefaultBasePrefersOriginBranch(t *testing.T) {
	t.Parallel()
	// origin/main is at o2 but the local main is stale at o1; the feature branch
	// forked from o2. Using the local main would pull in o2.txt.
	h := &history{}
	o1 := h.commit(remote("main"), 0, "o1", put("o1.txt", "1\n"))
	o2 := h.commit(remote("main"), o1, "o2", put("o2.txt", "2\n"))
	h.point(heads("main"), o1)
	h.commit(heads("feature"), o2, "f1", put("feature.txt", "f\n"))
	r := newRepo(t, h, "feature", false)

	cs := extractHead(t, r.dir, testOptions())
	if cs.BaseSHA != r.sha(o2) {
		t.Errorf("BaseSHA = %s, want origin/main %s (not the stale local main %s)", cs.BaseSHA, r.sha(o2), r.sha(o1))
	}
	wantPaths(t, cs, "feature.txt")
}

func TestExtractExplicitRefs(t *testing.T) {
	t.Parallel()
	h := &history{}
	e1 := h.commit(heads("main"), 0, "e1", put("e1.txt", "1\n"))
	e2 := h.commit(heads("main"), e1, "e2", put("e2.txt", "2\n"))
	e3 := h.commit(heads("main"), e2, "e3", put("e3.txt", "3\n"))
	r := newRepo(t, h, "main", false)

	for _, tc := range []struct {
		name       string
		base, head string
		wantBase   int
		wantHead   int
		wantPaths  []string
	}{
		{"shas", r.sha(e1), r.sha(e3), e1, e3, []string{"e2.txt", "e3.txt"}},
		{"relative refs", "HEAD~2", "HEAD~1", e1, e2, []string{"e2.txt"}},
		{"branch names", "main~1", "main", e2, e3, []string{"e3.txt"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := testOptions()
			opts.Base, opts.Head = tc.base, tc.head
			cs := mustExtract(t, r.dir, opts)
			if cs.BaseSHA != r.sha(tc.wantBase) || cs.HeadSHA != r.sha(tc.wantHead) {
				t.Errorf("SHAs = %s..%s, want %s..%s", cs.BaseSHA, cs.HeadSHA, r.sha(tc.wantBase), r.sha(tc.wantHead))
			}
			wantPaths(t, cs, tc.wantPaths...)
		})
	}

	t.Run("unresolvable refs are ordinary errors", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct{ name, base, head string }{
			{"missing base", "no-such-ref", "HEAD"},
			{"missing head", "HEAD~1", "no-such-ref"},
		} {
			opts := testOptions()
			opts.Base, opts.Head = tc.base, tc.head
			cs, err := Extract(context.Background(), r.dir, opts)
			if err == nil {
				t.Fatalf("%s: Extract succeeded: %+v", tc.name, cs)
			}
			if errors.Is(err, ErrNoMergeBase) {
				t.Errorf("%s: error wraps ErrNoMergeBase: %v", tc.name, err)
			}
			// The error carries the git argv and stderr.
			if !strings.Contains(err.Error(), "no-such-ref") || !strings.Contains(err.Error(), "rev-parse") {
				t.Errorf("%s: error = %v, want the argv and the ref", tc.name, err)
			}
		}
	})

	t.Run("refs cannot be read as options", func(t *testing.T) {
		t.Parallel()
		victim := filepath.Join(t.TempDir(), "written-by-git")
		for _, tc := range []struct{ name, base, head string }{
			{"base", "--output=" + victim, ""},
			{"head", "HEAD~1", "--output=" + victim},
			{"short option", "-p", ""},
		} {
			opts := testOptions()
			opts.Base, opts.Head = tc.base, tc.head
			cs, err := Extract(context.Background(), r.dir, opts)
			if err == nil {
				t.Fatalf("%s: Extract accepted an option-like ref: %+v", tc.name, cs)
			}
			// Refused up front, before any git process could read it as an option.
			if !strings.Contains(err.Error(), "must not start with '-'") {
				t.Errorf("%s: error = %v, want the up-front refusal", tc.name, err)
			}
			if _, err := os.Stat(victim); err == nil {
				t.Fatalf("%s: git wrote the file named by an option-like ref", tc.name)
			}
		}
	})
}

func assertNoMergeBase(t *testing.T, dir string, opts Options) {
	t.Helper()
	cs, err := Extract(context.Background(), dir, opts)
	if !errors.Is(err, ErrNoMergeBase) {
		t.Fatalf("Extract = %+v, %v; want an error wrapping ErrNoMergeBase", cs, err)
	}
	if cs != nil {
		t.Errorf("ChangeSet = %+v alongside ErrNoMergeBase, want nil", cs)
	}
}

func TestExtractShallowCloneHasNoMergeBase(t *testing.T) {
	t.Parallel()
	origin := &history{}
	o1 := origin.commit(heads("main"), 0, "o1", put("o1.txt", "1\n"))
	origin.commit(heads("main"), o1, "o2", put("o2.txt", "2\n"))
	src := newRepo(t, origin, "main", false)

	clone := filepath.Join(t.TempDir(), "clone")
	if _, err := gitRun(filepath.Dir(clone), "clone", "-q", "--depth", "1", fileURL(src.dir), clone); err != nil {
		t.Fatal(err)
	}
	// Premise: one commit, history cut, and origin/main is the cut-off HEAD.
	if got, err := gitRun(clone, "rev-list", "--count", "HEAD"); err != nil || got != "1" {
		t.Fatalf("premise: clone has %q commits (%v), want 1", got, err)
	}
	if _, err := os.Stat(filepath.Join(clone, ".git", "shallow")); err != nil {
		t.Fatalf("premise: clone is not shallow: %v", err)
	}
	if _, err := gitRun(clone, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/main"); err != nil {
		t.Fatalf("premise: origin/main missing: %v", err)
	}

	assertNoMergeBase(t, clone, testOptions())

	t.Run("an explicit base still works", func(t *testing.T) {
		opts := testOptions()
		opts.Base = "HEAD"
		cs := mustExtract(t, clone, opts)
		if cs.BaseSHA != cs.HeadSHA || cs.Dirty {
			t.Errorf("BaseSHA %s HeadSHA %s Dirty %v", cs.BaseSHA, cs.HeadSHA, cs.Dirty)
		}
	})
}

func TestExtractNoMergeBaseCases(t *testing.T) {
	t.Parallel()
	t.Run("the default branch ref does not exist", func(t *testing.T) {
		t.Parallel()
		h := &history{}
		h.commit(heads("topic"), 0, "t1", put("t1.txt", "1\n"))
		r := newRepo(t, h, "topic", false)
		opts := testOptions()
		opts.Head = "HEAD"
		assertNoMergeBase(t, r.dir, opts)
	})
	t.Run("unrelated histories", func(t *testing.T) {
		t.Parallel()
		h := &history{}
		h.commit(heads("main"), 0, "m1", put("m1.txt", "1\n"))
		h.commit(heads("other"), 0, "o1", put("o1.txt", "1\n")) // a second root
		r := newRepo(t, h, "other", false)
		opts := testOptions()
		opts.Head = "HEAD"
		assertNoMergeBase(t, r.dir, opts)
		opts.DefaultBranch = "release" // no such branch either
		assertNoMergeBase(t, r.dir, opts)
	})
	t.Run("shallow history", func(t *testing.T) {
		t.Parallel()
		// main: m1..m6; topic forks from m5. The clone is cut below the merge base.
		h := &history{}
		var m [7]int
		for i := 1; i <= 6; i++ {
			name := "m" + string(rune('0'+i))
			m[i] = h.commit(heads("main"), m[i-1], name, put(name+".txt", "x\n"))
		}
		t1 := h.commit(heads("topic"), m[5], "t1", put("t1.txt", "x\n"))
		t2 := h.commit(heads("topic"), t1, "t2", put("t2.txt", "x\n"))
		r := newRepo(t, h, "topic", false)
		shallowFile := filepath.Join(r.dir, ".git", "shallow")
		opts := testOptions()
		opts.Head = "HEAD"

		// Cut at m4: the merge base m5 is interior, so it can be trusted.
		if err := os.WriteFile(shallowFile, []byte(r.sha(m[4])+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		cs := mustExtract(t, r.dir, opts)
		if cs.BaseSHA != r.sha(m[5]) || cs.HeadSHA != r.sha(t2) {
			t.Errorf("SHAs = %s..%s, want %s..%s", cs.BaseSHA, cs.HeadSHA, r.sha(m[5]), r.sha(t2))
		}
		wantPaths(t, cs, "t1.txt", "t2.txt")

		// Cut at the merge base itself: the history beneath it is unknown.
		if err := os.WriteFile(shallowFile, []byte(r.sha(m[5])+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		assertNoMergeBase(t, r.dir, opts)
	})
}

func TestExtractErrorsAreNeverAnEmptyChange(t *testing.T) {
	t.Parallel()
	t.Run("not a repository", func(t *testing.T) {
		t.Parallel()
		cs, err := Extract(context.Background(), t.TempDir(), testOptions())
		if err == nil || cs != nil {
			t.Errorf("Extract = %+v, %v; want nil and an error", cs, err)
		}
	})
	t.Run("missing directory", func(t *testing.T) {
		t.Parallel()
		cs, err := Extract(context.Background(), filepath.Join(t.TempDir(), "absent"), testOptions())
		if err == nil || cs != nil {
			t.Errorf("Extract = %+v, %v; want nil and an error", cs, err)
		}
	})
	t.Run("no commits", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if _, err := gitRun(dir, "init", "-q", "-b", "main"); err != nil {
			t.Fatal(err)
		}
		cs, err := Extract(context.Background(), dir, testOptions())
		if err == nil || cs != nil {
			t.Errorf("Extract = %+v, %v; want nil and an error", cs, err)
		}
	})
	t.Run("canceled context", func(t *testing.T) {
		t.Parallel()
		h := &history{}
		h.commit(heads("main"), 0, "m1", put("m1.txt", "1\n"))
		r := newRepo(t, h, "main", false)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		cs, err := Extract(ctx, r.dir, testOptions())
		if !errors.Is(err, context.Canceled) || cs != nil {
			t.Errorf("Extract = %+v, %v; want nil and context.Canceled", cs, err)
		}
	})
}
