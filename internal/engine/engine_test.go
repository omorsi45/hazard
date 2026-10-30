package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/omorsi45/hazard/internal/change"
	"github.com/omorsi45/hazard/internal/impact"
	"github.com/omorsi45/hazard/internal/store"
)

const (
	coreEdited = "package core\n\nfunc Core() int { return 1 + 0 }\n"
	maxHops1   = "version: 1\nlanguages:\n  python:\n    enabled: false\nimpact:\n  max_hops: 1\n"
)

var hexID = regexp.MustCompile(`^[0-9a-f]{32}$`)

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// TestSelectCoreChange builds one indexed repository with core edited and runs
// every behavior that only needs a fresh index and a narrowed L1 result. The
// index build dominates the cost, so the sub-tests share it and run in order.
func TestSelectCoreChange(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	f.write(t, map[string]string{"core/core.go": coreEdited})
	f.indexNow(t, false)

	var first Decision
	t.Run("narrows to the hypothetical set and runs the full suite", func(t *testing.T) {
		d := selectOK(t, Options{RepoDir: f.dir})
		first = d
		if want := []string{idCore, idLeft, idRght, idTop}; !sameStrings(ids(d.Hypothetical), want) {
			t.Fatalf("Hypothetical = %v, want %v (never the unrelated solo package)", ids(d.Hypothetical), want)
		}
		for _, s := range d.Hypothetical {
			if s.Source != "certain" || s.Reason != "graph" || s.P != nil {
				t.Errorf("%s: source %q reason %q p %v, want certain/graph/nil", s.TestID, s.Source, s.Reason, s.P)
			}
		}
		if h := d.Hypothetical[0]; h.Package != "example.com/m/core" || h.RunnerID != "TestCore" {
			t.Errorf("inventory fields = %q, %q", h.Package, h.RunnerID)
		}
		if !d.FullSuite || !d.Abstained || d.AbstainReason != "no_lock" {
			t.Errorf("FullSuite %v Abstained %v reason %q, want no_lock", d.FullSuite, d.Abstained, d.AbstainReason)
		}
		if len(d.Tests) != 0 {
			t.Errorf("Tests = %v, want none while FullSuite", ids(d.Tests))
		}
		if !sameStrings(d.AllTests, diamondIDs) {
			t.Errorf("AllTests = %v, want %v", d.AllTests, diamondIDs)
		}
		if d.Change == nil || len(d.Change.Files) != 1 || d.Change.Files[0].Package != "example.com/m/core" {
			t.Errorf("Change = %+v, want core/core.go mapped to its package", d.Change)
		}
		if d.L1 == nil || d.L1.FullReason != "" || d.L1.CoverageState != "not_requested" {
			t.Errorf("L1 = %+v, want a graph-mode result without a full-suite directive", d.L1)
		}
		if !hexID.MatchString(d.ID) {
			t.Errorf("ID = %q, want 32 hex characters", d.ID)
		}
		if d.OverheadMS < 0 {
			t.Errorf("OverheadMS = %d", d.OverheadMS)
		}
		if again := selectOK(t, Options{RepoDir: f.dir}); again.ID == d.ID {
			t.Errorf("two decisions share the ID %q", d.ID)
		}
	})

	t.Run("JSON uses snake_case and omits hunk text", func(t *testing.T) {
		data, err := json.Marshal(first)
		if err != nil {
			t.Fatal(err)
		}
		var top map[string]any
		if err := json.Unmarshal(data, &top); err != nil {
			t.Fatalf("not JSON: %v\n%s", err, data)
		}
		for _, key := range []string{"id", "full_suite", "abstained", "abstain_reason", "lock_digest", "lambda", "tests", "hypothetical", "all_tests", "change", "l1", "warnings", "est_ms_selected", "est_ms_full", "overhead_ms", "cost_usd"} {
			if _, ok := top[key]; !ok {
				t.Errorf("missing key %q in %s", key, data)
			}
		}
		if top["full_suite"] != true || top["abstain_reason"] != "no_lock" {
			t.Errorf("full_suite/abstain_reason = %v/%v", top["full_suite"], top["abstain_reason"])
		}
		if tests, ok := top["tests"].([]any); !ok || len(tests) != 0 {
			t.Errorf("tests = %#v, want an empty array, not null", top["tests"])
		}
		hyp, _ := top["hypothetical"].([]any)
		if len(hyp) != 4 {
			t.Fatalf("hypothetical = %#v", top["hypothetical"])
		}
		hyp0, _ := hyp[0].(map[string]any)
		if hyp0["test_id"] != idCore || hyp0["source"] != "certain" || hyp0["reason"] != "graph" || hyp0["package"] != "example.com/m/core" {
			t.Errorf("hypothetical[0] = %v", hyp0)
		}
		chg, _ := top["change"].(map[string]any)
		files, _ := chg["files"].([]any)
		if len(files) != 1 {
			t.Fatalf("change = %v", chg)
		}
		file, _ := files[0].(map[string]any)
		if file["path"] != "core/core.go" || file["kind"] != "source" || file["status"] != "M" || file["package"] != "example.com/m/core" {
			t.Errorf("file = %v", file)
		}
		hunks, _ := file["hunks"].([]any)
		if len(hunks) != 1 {
			t.Fatalf("hunks = %v", file["hunks"])
		}
		hunk, _ := hunks[0].(map[string]any)
		if _, ok := hunk["text"]; ok {
			t.Errorf("hunk text must not be emitted: %v", hunk)
		}
		if hunk["old_start"] == nil || hunk["symbols"] == nil {
			t.Errorf("hunk = %v", hunk)
		}
		l1, _ := top["l1"].(map[string]any)
		for _, key := range []string{"certain", "candidates", "raw_candidates", "truncated", "changed_packages", "package_count", "test_hops", "coverage_state", "full_reason"} {
			if _, ok := l1[key]; !ok {
				t.Errorf("l1 is missing %q: %v", key, l1)
			}
		}
		for _, camel := range []string{"FullSuite", "AbstainReason", "TestID", "OldStart", "BaseSHA"} {
			if strings.Contains(string(data), `"`+camel+`"`) {
				t.Errorf("CamelCase key %q leaked into %s", camel, data)
			}
		}
	})

	t.Run("dry-run payload sends nothing", func(t *testing.T) {
		if first.Payload != nil {
			t.Errorf("Payload = %q without DryRunPayload", first.Payload)
		}
		d := selectOK(t, Options{RepoDir: f.dir, DryRunPayload: true})
		if string(d.Payload) != `{"requests":[]}` {
			t.Errorf("Payload = %q", d.Payload)
		}
		if !contains(d.Warnings, "no external predictor in the active policy; nothing would be sent") {
			t.Errorf("Warnings = %v", d.Warnings)
		}
	})

	t.Run("shadow keeps the hypothetical set", func(t *testing.T) {
		d := selectOK(t, Options{RepoDir: f.dir, Shadow: true})
		if !d.FullSuite || len(d.Tests) != 0 || len(d.Hypothetical) != 4 {
			t.Errorf("FullSuite %v Tests %v Hypothetical %v", d.FullSuite, ids(d.Tests), ids(d.Hypothetical))
		}
		if !strings.Contains(strings.Join(d.Warnings, "\n"), "shadow") {
			t.Errorf("Warnings = %v, want a note that the decision was already a full suite", d.Warnings)
		}
	})

	t.Run("an explicit graph mode is honoured", func(t *testing.T) {
		d := selectOK(t, Options{RepoDir: f.dir, Mode: impact.ModeCoverage, ModeSet: true})
		if d.L1 == nil || d.L1.CoverageState != "missing" {
			t.Errorf("an explicit coverage mode on an index without coverage: L1 = %+v, want state missing", d.L1)
		}
	})

	t.Run("a lock file that is not a lock", func(t *testing.T) {
		elsewhere := filepath.Join(t.TempDir(), "x.lock")
		if d := selectOK(t, Options{RepoDir: f.dir, LockPath: elsewhere}); d.AbstainReason != "no_lock" {
			t.Errorf("missing explicit lock: reason %q, want no_lock", d.AbstainReason)
		}
		if err := os.WriteFile(elsewhere, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		d := selectOK(t, Options{RepoDir: f.dir, LockPath: elsewhere})
		if !d.FullSuite || d.AbstainReason != "lock_corrupt" || len(d.Tests) != 0 {
			t.Errorf("explicit lock: FullSuite %v reason %q tests %v, want lock_corrupt", d.FullSuite, d.AbstainReason, ids(d.Tests))
		}
		if len(d.Hypothetical) != 4 {
			t.Errorf("Hypothetical = %v, want the narrowed set kept", ids(d.Hypothetical))
		}
		f.write(t, map[string]string{"hazard.lock": "{}"})
		if d := selectOK(t, Options{RepoDir: f.dir}); d.AbstainReason != "lock_corrupt" {
			t.Errorf("default lock location: reason %q, want lock_corrupt", d.AbstainReason)
		}
	})
}

func TestSelectRespectsMaxHops(t *testing.T) {
	t.Parallel()
	f := newFixture(t, map[string]string{"hazard.yml": maxHops1})
	f.write(t, map[string]string{"core/core.go": coreEdited})
	f.indexNow(t, false)

	d := selectOK(t, Options{RepoDir: f.dir})

	if want := []string{idCore, idLeft, idRght}; !sameStrings(ids(d.Hypothetical), want) {
		t.Errorf("Hypothetical = %v, want %v", ids(d.Hypothetical), want)
	}
	if d.L1 == nil {
		t.Fatal("L1 did not run")
	}
	var top bool
	for _, c := range d.L1.Candidates {
		top = top || c.TestID == idTop
	}
	if !top {
		t.Errorf("top is two hops away and must remain a residual candidate: %+v", d.L1.Candidates)
	}
}

// TestSelectHistoryDatabase shares one index across the docs-only change and
// every state of the history database.
func TestSelectHistoryDatabase(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	f.indexNow(t, false)
	f.write(t, map[string]string{"README.md": "# docs only\n"})

	t.Run("docs-only change with no database has an empty hypothetical set", func(t *testing.T) {
		d := selectOK(t, Options{RepoDir: f.dir})
		if d.Hypothetical == nil || len(d.Hypothetical) != 0 {
			t.Errorf("Hypothetical = %#v, want empty and non-nil (L1 ran and selected nothing)", d.Hypothetical)
		}
		if !d.FullSuite || d.AbstainReason != "no_lock" {
			t.Errorf("FullSuite %v reason %q", d.FullSuite, d.AbstainReason)
		}
	})

	t.Run("quarantined and unstable tests always run", func(t *testing.T) {
		s, err := store.Open(filepath.Join(f.dir, ".hazard", "hazard.db"))
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range []struct {
			id, pkg, name         string
			quarantined, unstable int
		}{
			{idCore, "example.com/m/core", "TestCore", 0, 0},
			{idSolo, "example.com/m/solo", "TestSolo", 1, 0},
			{idTop, "example.com/m/top", "TestTop", 0, 1},
		} {
			if _, err := s.DB().Exec(`INSERT INTO tests (id, lang, package, name, file, first_seen, last_seen, quarantined, unstable_id) VALUES (?, 'go', ?, ?, 'x_test.go', '2026-01-01', '2026-01-01', ?, ?)`,
				row.id, row.pkg, row.name, row.quarantined, row.unstable); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		d := selectOK(t, Options{RepoDir: f.dir})
		if want := []string{idSolo, idTop}; !sameStrings(ids(d.Hypothetical), want) {
			t.Fatalf("Hypothetical = %v, want the quarantined and unstable tests %v", ids(d.Hypothetical), want)
		}
		if h := d.Hypothetical; h[0].Source != "always_run" || h[0].Reason != "quarantined" || h[1].Source != "always_run" || h[1].Reason != "unstable_id" {
			t.Errorf("Hypothetical = %+v, want always_run with the database reasons", h)
		}
	})

	t.Run("an unreadable database runs the full suite", func(t *testing.T) {
		if err := os.Remove(filepath.Join(f.dir, ".hazard", "hazard.db")); err != nil {
			t.Fatal(err)
		}
		f.write(t, map[string]string{".hazard/hazard.db": strings.Repeat("this is not a database ", 400)})
		d := selectOK(t, Options{RepoDir: f.dir})
		if !d.FullSuite || d.AbstainReason != "history_error" || len(d.Warnings) == 0 {
			t.Errorf("FullSuite %v reason %q warnings %v, want history_error with a warning", d.FullSuite, d.AbstainReason, d.Warnings)
		}
		if d.Hypothetical != nil || len(d.Tests) != 0 {
			t.Errorf("an unreadable history must not produce a narrowed set: %v %v", ids(d.Hypothetical), ids(d.Tests))
		}
	})
}

func TestSelectFullSuiteTriggerForcesFullSuite(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	f.write(t, map[string]string{"go.mod": "module example.com/m\n\ngo 1.21\n\n// touched\n"})
	f.indexNow(t, false)

	d := selectOK(t, Options{RepoDir: f.dir})

	if !d.FullSuite || d.AbstainReason != "trigger:go.mod:go.mod" {
		t.Errorf("FullSuite %v reason %q, want the go.mod trigger", d.FullSuite, d.AbstainReason)
	}
	if d.Hypothetical != nil {
		t.Errorf("Hypothetical = %v, want nil when L1 demands the full suite", ids(d.Hypothetical))
	}
	if len(d.Tests) != 0 {
		t.Errorf("Tests = %v, want none", ids(d.Tests))
	}
	if d.L1 == nil || d.L1.FullReason != "trigger:go.mod:go.mod" {
		t.Errorf("L1 = %+v", d.L1)
	}
}

func TestSelectMissingAndCorruptIndex(t *testing.T) {
	t.Parallel()
	t.Run("no index", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, nil)
		d := selectOK(t, Options{RepoDir: f.dir, DryRunPayload: true})
		if !d.FullSuite || d.AbstainReason != "no_index" {
			t.Errorf("FullSuite %v reason %q, want no_index", d.FullSuite, d.AbstainReason)
		}
		if string(d.Payload) != `{"requests":[]}` {
			t.Errorf("an early abstention still reports the empty payload: %q", d.Payload)
		}
	})
	t.Run("corrupt index", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, nil)
		f.write(t, map[string]string{".hazard/index/manifest.json": "{not json"})
		d := selectOK(t, Options{RepoDir: f.dir})
		if !d.FullSuite || d.AbstainReason != "index_corrupt" || len(d.Warnings) == 0 {
			t.Errorf("FullSuite %v reason %q warnings %v, want index_corrupt with a warning", d.FullSuite, d.AbstainReason, d.Warnings)
		}
	})
}

func TestSelectShallowCloneAbstainsWithNoMergeBase(t *testing.T) {
	t.Parallel()
	src := newFixture(t, nil)
	src.write(t, map[string]string{"README.md": "# two\n"})
	commitAll(t, src.dir, "second")
	clone := filepath.Join(t.TempDir(), "clone")
	gitIn(t, filepath.Dir(clone), "clone", "-q", "--depth", "1", fileURL(src.dir), clone)
	if _, err := os.Stat(filepath.Join(clone, ".git", "shallow")); err != nil {
		t.Fatalf("premise: the clone is not shallow: %v", err)
	}
	cf := &fixture{dir: clone}
	cf.indexNow(t, false)

	d := selectOK(t, Options{RepoDir: clone})

	if !d.FullSuite || d.AbstainReason != "no_merge_base" {
		t.Fatalf("FullSuite %v reason %q, want no_merge_base", d.FullSuite, d.AbstainReason)
	}
	if len(d.Tests) != 0 || d.Hypothetical != nil {
		t.Errorf("Tests %v Hypothetical %v, want neither", ids(d.Tests), ids(d.Hypothetical))
	}
	if len(d.Warnings) == 0 {
		t.Error("the abstention must carry the extraction error as a warning")
	}
}

func TestSelectExplicitRefs(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	first := f.git(t, "rev-parse", "HEAD")
	f.write(t, map[string]string{"core/core.go": coreEdited})
	second := commitAll(t, f.dir, "edit core")
	f.indexNow(t, false)

	for _, opts := range []Options{
		{RepoDir: f.dir, Base: first},
		{RepoDir: f.dir, Base: "HEAD~1", Head: "HEAD"},
		{RepoDir: f.dir, Base: first, Head: second},
	} {
		d := selectOK(t, opts)
		if want := []string{idCore, idLeft, idRght, idTop}; !sameStrings(ids(d.Hypothetical), want) {
			t.Errorf("%+v: Hypothetical = %v, want %v", opts, ids(d.Hypothetical), want)
		}
		if d.Change == nil || d.Change.BaseSHA != first || d.Change.HeadSHA != second {
			t.Errorf("%+v: Change = %+v, want %s..%s", opts, d.Change, first, second)
		}
	}
}

func TestSelectRejectsUnresolvableExplicitRefs(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	for _, opts := range []Options{
		{RepoDir: f.dir, Base: "no-such-ref"},
		{RepoDir: f.dir, Head: "no-such-ref"},
		{RepoDir: f.dir, Base: "--output=x"},
		{RepoDir: f.dir, Base: "HEAD", Head: "-x"},
	} {
		d, err := Select(context.Background(), opts)
		if !errors.Is(err, ErrInvalidRef) {
			t.Errorf("%+v: err = %v, want ErrInvalidRef", opts, err)
		}
		if d.FullSuite || d.ID != "" {
			t.Errorf("%+v: an invalid invocation must not yield a decision: %+v", opts, d)
		}
	}
}

func TestSelectInvalidInvocation(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	t.Run("relative repo dir", func(t *testing.T) {
		t.Parallel()
		if _, err := Select(context.Background(), Options{RepoDir: "repo"}); err == nil {
			t.Error("a relative RepoDir must be an error")
		}
	})
	t.Run("missing repo dir", func(t *testing.T) {
		t.Parallel()
		if _, err := Select(context.Background(), Options{}); err == nil {
			t.Error("an empty RepoDir must be an error")
		}
	})
	t.Run("invalid config file", func(t *testing.T) {
		t.Parallel()
		bad := newFixture(t, map[string]string{"hazard.yml": "version: 2\n"})
		_, err := Select(context.Background(), Options{RepoDir: bad.dir})
		if !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("err = %v, want ErrInvalidConfig", err)
		}
	})
	t.Run("missing explicit config", func(t *testing.T) {
		t.Parallel()
		_, err := Select(context.Background(), Options{RepoDir: f.dir, ConfigPath: filepath.Join(f.dir, "nope.yml")})
		if !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("err = %v, want ErrInvalidConfig", err)
		}
	})
	t.Run("config from the second location", func(t *testing.T) {
		t.Parallel()
		moved := newFixture(t, map[string]string{".hazard/hazard.yml": "version: 2\n"})
		if err := os.Remove(filepath.Join(moved.dir, "hazard.yml")); err != nil {
			t.Fatal(err)
		}
		// .hazard/ is ignored by git but the file exists on disk: it must be
		// found and rejected as invalid.
		_, err := Select(context.Background(), Options{RepoDir: moved.dir})
		if !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("err = %v, want the .hazard/hazard.yml location to be read", err)
		}
	})
	t.Run("canceled context", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := Select(ctx, Options{RepoDir: f.dir}); !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	})
}

func TestApplyShadowOnNarrowedDecision(t *testing.T) {
	t.Parallel()
	hyp := []Selected{{TestID: "a", Source: "certain", Reason: "graph"}}
	d := Decision{Tests: hyp, Hypothetical: hyp}
	applyShadow(&d)
	if !d.FullSuite || d.Abstained || d.AbstainReason != "shadow" || len(d.Tests) != 0 || len(d.Hypothetical) != 1 {
		t.Errorf("narrowed decision under shadow = %+v", d)
	}
	full := Decision{FullSuite: true, Abstained: true, AbstainReason: "no_lock"}
	applyShadow(&full)
	if full.AbstainReason != "no_lock" || !full.Abstained {
		t.Errorf("an existing abstention must keep its reason: %+v", full)
	}
}

func TestSelectCoverageModeIsAutomaticAndFresh(t *testing.T) {
	t.Parallel()
	f := newFixtureFiles(t, map[string]string{
		"go.mod":            "module example.com/m\n\ngo 1.21\n",
		"hazard.yml":        "version: 1\nlanguages:\n  python:\n    enabled: false\n",
		".gitignore":        ".hazard/\n",
		"README.md":         "# readme\n",
		"core/core.go":      "package core\n\nfunc Core() int { return 1 }\n",
		"core/core_test.go": "package core\n\nimport \"testing\"\n\nfunc TestCore(t *testing.T) {\n\tif Core() != 1 {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n",
	})
	f.indexNow(t, true)
	f.write(t, map[string]string{"README.md": "# docs only\n"})

	d := selectOK(t, Options{RepoDir: f.dir})
	if d.L1 == nil || d.L1.CoverageState != "fresh" {
		t.Fatalf("an index with coverage must select coverage mode and find it fresh (age 0): L1 = %+v", d.L1)
	}
	d = selectOK(t, Options{RepoDir: f.dir, Mode: impact.ModeGraph, ModeSet: true})
	if d.L1 == nil || d.L1.CoverageState != "not_requested" {
		t.Errorf("an explicit graph mode must win: L1 = %+v", d.L1)
	}
}

func TestCoverageAge(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	c1 := f.git(t, "rev-parse", "HEAD")
	f.write(t, map[string]string{"README.md": "2\n"})
	c2 := commitAll(t, f.dir, "two")
	f.write(t, map[string]string{"README.md": "3\n"})
	c3 := commitAll(t, f.dir, "three")
	f.git(t, "checkout", "-q", "-b", "side", c1)
	f.write(t, map[string]string{"README.md": "side\n"})
	s1 := commitAll(t, f.dir, "side")
	zero := strings.Repeat("0", 40)

	for _, tc := range []struct {
		name, cov, base string
		want            int
	}{
		{"same commit", c3, c3, 0},
		{"two behind", c1, c3, 2},
		{"one behind", c2, c3, 1},
		{"coverage is newer than the base", c3, c1, -1},
		{"diverged branches", s1, c3, -1},
		{"unknown commit", zero, c3, -1},
		{"not a hash", "--output=x", c3, -1},
		{"empty coverage head", "", c3, -1},
		{"empty base", c3, "", -1},
	} {
		got, err := coverageAge(context.Background(), f.dir, tc.cov, tc.base)
		if err != nil || got != tc.want {
			t.Errorf("%s: coverageAge = %d, %v, want %d", tc.name, got, err, tc.want)
		}
	}
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestBaseDigests(t *testing.T) {
	t.Parallel()
	f := newFixture(t, map[string]string{
		"a.go":                "package a\n",
		"dir with space/b.go": "package b\n",
		"ünï.go":              "package u\n",
		"gone.go":             "package gone\n",
	})
	base := f.git(t, "rev-parse", "HEAD")
	files := []change.ChangedFile{
		{Path: "a.go", Status: "M"},
		{Path: "added.go", Status: "A"},
		{Path: "renamed.go", OldPath: "dir with space/b.go", Status: "R"},
		{Path: "ünï.go", Status: "M"},
		{Path: "gone.go", Status: "D"},
		{Path: "never-existed.go", Status: "M"},
		{Path: "bad\nname.go", Status: "M"},
		{Path: "core", Status: "M"},
	}

	got, err := baseDigests(context.Background(), f.dir, base, files)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"a.go":                sha("package a\n"),
		"dir with space/b.go": sha("package b\n"),
		"ünï.go":              sha("package u\n"),
		"gone.go":             sha("package gone\n"),
	}
	if len(got) != len(want) {
		t.Errorf("baseDigests = %v, want exactly %v (an added, missing, newline or directory path has no base digest)", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("digest[%q] = %q, want %q", k, got[k], v)
		}
	}
}

func TestDecisionJSONDistinguishesNilFromEmptyHypothetical(t *testing.T) {
	t.Parallel()
	data, err := json.Marshal(Decision{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"hypothetical":null`) || !strings.Contains(string(data), `"tests":[]`) || !strings.Contains(string(data), `"change":null`) {
		t.Errorf("zero Decision = %s", data)
	}
	data, err = json.Marshal(Decision{Hypothetical: []Selected{}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"hypothetical":[]`) {
		t.Errorf("empty Hypothetical = %s", data)
	}
}
