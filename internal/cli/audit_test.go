package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/omorsi45/hazard/internal/runner"
	"github.com/omorsi45/hazard/internal/store"
	"github.com/omorsi45/hazard/internal/telemetry"
)

// runAuditCmd runs the audit command built around fake runners inside a root
// command with the persistent flags of the real one.
func runAuditCmd(ctx context.Context, rn runners, args ...string) (int, string, string) {
	root := &cobra.Command{Use: "hazard", SilenceUsage: true, SilenceErrors: true}
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError{err} })
	root.PersistentFlags().Bool("json", false, "")
	root.PersistentFlags().CountP("verbose", "v", "")
	root.AddCommand(newAuditCommand(rn))
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(append([]string{"audit"}, args...))
	err := root.ExecuteContext(ctx)
	code := 0
	if err != nil {
		var silent bool
		code, silent = exitCodeOf(err, false)
		if !silent {
			errOut.WriteString("error: " + err.Error() + "\n")
		}
	}
	return code, out.String(), errOut.String()
}

// isolateGit makes git ignore the user's configuration (hooks, signing) for
// the test, so fixture repositories are fast and behave the same everywhere.
func isolateGit(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

// auditRepo is a git repository (the working directory) with one commit per
// name: each rewrites notes.txt. It returns the directory and the SHAs.
func auditRepo(t *testing.T, commits int) (string, []string) {
	t.Helper()
	isolateGit(t)
	dir := chdirRepo(t)
	var shas []string
	for i := 1; i <= commits; i++ {
		if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte(fmt.Sprintf("v%d", i)), 0o644); err != nil {
			t.Fatal(err)
		}
		gitTest(t, dir, "add", "-A")
		gitTest(t, dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "-m", fmt.Sprintf("commit %d", i))
		shas = append(shas, gitTest(t, dir, "rev-parse", "HEAD"))
	}
	return dir, shas
}

const (
	auditA = "go:example.com/audit/p:TestA"
	auditB = "go:example.com/audit/p:TestB"
)

var auditT0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// auditRecord is a narrowed run record at commit that selected members.
func auditRecord(id, commit string, offset time.Duration, members ...string) telemetry.Record {
	r := telemetry.Record{
		ID: id, Time: auditT0.Add(offset).Format(time.RFC3339), Commit: commit, Mode: telemetry.ModeRun,
		LockDigest: "sha256:lock", HazardVersion: version, Members: []telemetry.Member{},
	}
	for _, m := range members {
		r.Members = append(r.Members, telemetry.Member{TestID: m, Source: "certain"})
	}
	return r
}

func appendRecords(t *testing.T, dir string, recs ...telemetry.Record) {
	t.Helper()
	for _, r := range recs {
		if err := telemetry.Append(filepath.Join(dir, ".hazard", "runs.jsonl"), r); err != nil {
			t.Fatal(err)
		}
	}
}

// writeFullRun writes a JUnit report whose failing tests are names in the
// package example.com/audit/p, and returns its path.
func writeFullRun(t *testing.T, dir string, passing, failing []string) string {
	t.Helper()
	var cases []string
	for _, n := range passing {
		cases = append(cases, fmt.Sprintf(`<testcase classname="example.com/audit/p" name=%q/>`, n))
	}
	for _, n := range failing {
		cases = append(cases, fmt.Sprintf(`<testcase classname="example.com/audit/p" name=%q><failure message="FAIL"/></testcase>`, n))
	}
	path := filepath.Join(dir, "full.xml")
	if err := os.WriteFile(path, []byte(junitXML("example.com/audit/p", cases...)), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// noRunners are fake runners that fail the test if they are ever used.
func noRunners(t *testing.T) runners {
	t.Helper()
	fail := func(context.Context, runner.Request) (runner.Result, error) {
		t.Error("no test may be run without --bisect")
		return runner.Result{ExitCode: 70}, errors.New("unexpected run")
	}
	return runners{Go: fail, Python: fail}
}

type auditJSON struct {
	FullRunID         string `json:"full_run_id"`
	Commit            string `json:"commit"`
	Audited           int    `json:"audited_selections"`
	RegressionBearing int    `json:"regression_bearing"`
	Misses            int    `json:"misses"`
	Attributed        int    `json:"attributed"`
	Unattributed      int    `json:"unattributed"`
	MissList          []struct {
		SelectionID string `json:"selection_id"`
		TestID      string `json:"test_id"`
		BlameWindow string `json:"blame_window"`
		Attributed  bool   `json:"attributed"`
	} `json:"miss_list"`
	UnattributedTests []struct {
		TestID string `json:"test_id"`
		Reason string `json:"reason"`
	} `json:"unattributed_tests"`
}

func decodeAudit(t *testing.T, out string) auditJSON {
	t.Helper()
	var a auditJSON
	if err := json.Unmarshal([]byte(out), &a); err != nil {
		t.Fatalf("not an audit report: %q: %v", out, err)
	}
	return a
}

func fieldMissCount(t *testing.T, dir string) int {
	t.Helper()
	path := filepath.Join(dir, ".hazard", "hazard.db")
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return 0
	}
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	}()
	var n int
	if err := s.DB().QueryRow(`SELECT count(*) FROM field_misses`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAuditUsageErrors(t *testing.T) {
	dir, _ := auditRepo(t, 1)
	good := writeFullRun(t, dir, []string{"TestA"}, nil)
	garbage := filepath.Join(dir, "garbage.xml")
	if err := os.WriteFile(garbage, []byte("not xml"), 0o644); err != nil {
		t.Fatal(err)
	}
	appendRecords(t, dir, auditRecord("r1", "x", time.Minute, auditA))
	for name, args := range map[string][]string{
		"no --full-run":           {"--since-run", "r1"},
		"no --since-run":          {"--full-run", good},
		"a missing report":        {"--full-run", filepath.Join(dir, "absent.xml"), "--since-run", "r1"},
		"an invalid report":       {"--full-run", garbage, "--since-run", "r1"},
		"a bad --commit":          {"--full-run", good, "--since-run", "r1", "--commit", "no-such-ref"},
		"an unknown selection ID": {"--full-run", good, "--since-run", "ghost"},
		"a positional argument":   {"--full-run", good, "--since-run", "r1", "extra"},
	} {
		t.Run(name, func(t *testing.T) {
			code, _, errOut := runAuditCmd(context.Background(), noRunners(t), args...)
			if code != 2 || errOut == "" {
				t.Errorf("code=%d err=%q, want a usage error", code, errOut)
			}
		})
	}
}

func TestAuditCanceledContext(t *testing.T) {
	dir, _ := auditRepo(t, 1)
	good := writeFullRun(t, dir, []string{"TestA"}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	code, _, errOut := runAuditCmd(ctx, noRunners(t), "--full-run", good, "--since-run", "r1")
	if code != 70 || !strings.Contains(errOut, "canceled") {
		t.Errorf("code=%d err=%q, want 70 canceled", code, errOut)
	}
}

func TestAuditSameCommitMiss(t *testing.T) {
	dir, shas := auditRepo(t, 1)
	appendRecords(t, dir, auditRecord("r1", shas[0], time.Minute, "go:example.com/audit/p:TestA"))
	full := writeFullRun(t, dir, []string{"TestA"}, []string{"TestB"})

	code, out, errOut := runAuditCmd(context.Background(), noRunners(t), "--full-run", full, "--since-run", "r1", "--json")
	if code != 0 {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
	a := decodeAudit(t, out)
	if a.Audited != 1 || a.RegressionBearing != 1 || a.Misses != 1 || a.Attributed != 1 || a.Unattributed != 0 || a.Commit != shas[0] || !strings.HasPrefix(a.FullRunID, "junit-") {
		t.Errorf("report = %+v", a)
	}
	if len(a.MissList) != 1 || a.MissList[0].SelectionID != "r1" || a.MissList[0].TestID != auditB || !a.MissList[0].Attributed {
		t.Errorf("misses = %+v", a.MissList)
	}
	if n := fieldMissCount(t, dir); n != 1 {
		t.Errorf("field_misses rows = %d, want 1", n)
	}
	data, err := os.ReadFile(filepath.Join(dir, ".hazard", "audits.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"selection_id":"r1"`, `"commit":"` + shas[0] + `"`, `"failing":["` + auditB + `"]`, `"missed":["` + auditB + `"]`, `"regression_bearing":true`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("audits.jsonl lacks %s: %s", want, data)
		}
	}

	// The text form reports the same counts.
	code, out, errOut = runAuditCmd(context.Background(), noRunners(t), "--full-run", full, "--since-run", "r1")
	if code != 0 {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
	for _, want := range []string{"audited selections: 1", "regression-bearing: 1", "misses: 1", "attributed: 1", "unattributed: 0", auditB, "r1"} {
		if !strings.Contains(out, want) {
			t.Errorf("text output lacks %q:\n%s", want, out)
		}
	}
	if n := fieldMissCount(t, dir); n != 1 {
		t.Errorf("re-auditing the same full run must not duplicate rows: %d", n)
	}
}

func TestAuditSameCommitNonMiss(t *testing.T) {
	dir, shas := auditRepo(t, 1)
	appendRecords(t, dir, auditRecord("r1", shas[0], time.Minute, auditA, auditB))
	full := writeFullRun(t, dir, []string{"TestA"}, []string{"TestB"})
	code, out, errOut := runAuditCmd(context.Background(), noRunners(t), "--full-run", full, "--since-run", "r1", "--json")
	if code != 0 {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
	a := decodeAudit(t, out)
	if a.Audited != 1 || a.RegressionBearing != 1 || a.Misses != 0 || a.Unattributed != 0 {
		t.Errorf("report = %+v; the selection caught the failing test", a)
	}
	if n := fieldMissCount(t, dir); n != 0 {
		t.Errorf("field_misses rows = %d, want none", n)
	}
	data, err := os.ReadFile(filepath.Join(dir, ".hazard", "audits.jsonl"))
	if err != nil || !strings.Contains(string(data), `"missed":[]`) {
		t.Errorf("audits.jsonl = %q, err %v; want a line with no misses", data, err)
	}
}

func TestAuditGreenFullRunIsNotRegressionBearing(t *testing.T) {
	dir, shas := auditRepo(t, 1)
	appendRecords(t, dir, auditRecord("r1", shas[0], time.Minute, auditA))
	full := writeFullRun(t, dir, []string{"TestA", "TestB"}, nil)
	code, out, errOut := runAuditCmd(context.Background(), noRunners(t), "--full-run", full, "--since-run", "r1", "--json")
	a := decodeAudit(t, out)
	if code != 0 || a.Audited != 1 || a.RegressionBearing != 0 || a.Misses != 0 {
		t.Errorf("code=%d err=%q report=%+v", code, errOut, a)
	}
}

func TestAuditRecordOnAnotherCommitIsNotAMissWithoutBisect(t *testing.T) {
	dir, shas := auditRepo(t, 2)
	appendRecords(t, dir, auditRecord("r1", shas[0], time.Minute, auditA))
	full := writeFullRun(t, dir, []string{"TestA"}, []string{"TestB"}) // HEAD is shas[1]
	code, out, errOut := runAuditCmd(context.Background(), noRunners(t), "--full-run", full, "--since-run", "r1", "--json")
	if code != 0 {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
	a := decodeAudit(t, out)
	if a.Misses != 0 || a.Attributed != 0 || a.Unattributed != 1 || a.Audited != 0 || len(a.UnattributedTests) != 1 || !strings.Contains(a.UnattributedTests[0].Reason, "--bisect") {
		t.Errorf("report = %+v; a selection on another commit is not a miss without a bisect", a)
	}
	if n := fieldMissCount(t, dir); n != 0 {
		t.Errorf("field_misses rows = %d, want none", n)
	}
}

func TestAuditSinceRunByIDAndByTime(t *testing.T) {
	dir, shas := auditRepo(t, 1)
	appendRecords(t, dir,
		auditRecord("old", shas[0], -time.Hour, auditB),
		auditRecord("r1", shas[0], time.Minute, auditA),
		auditRecord("r2", shas[0], 2*time.Minute, auditA, auditB))
	full := writeFullRun(t, dir, nil, []string{"TestB"})
	audited := func(since string) int {
		code, out, errOut := runAuditCmd(context.Background(), noRunners(t), "--full-run", full, "--since-run", since, "--json")
		if code != 0 {
			t.Fatalf("since %s: code=%d err=%q", since, code, errOut)
		}
		return decodeAudit(t, out).Audited
	}
	if n := audited("r1"); n != 2 {
		t.Errorf("since r1: %d audited, want r1 and r2 (inclusive, not old)", n)
	}
	if n := audited(auditT0.Add(2 * time.Minute).Format(time.RFC3339)); n != 1 {
		t.Errorf("since r2's time: %d audited, want 1 (inclusive)", n)
	}
	if n := audited(auditT0.Add(-2 * time.Hour).Format(time.RFC3339)); n != 3 {
		t.Errorf("since long ago: %d audited, want all 3", n)
	}
}

func TestAuditCommitFlagSelectsTheFullRunsCommit(t *testing.T) {
	dir, shas := auditRepo(t, 2)
	appendRecords(t, dir, auditRecord("r1", shas[0], time.Minute, auditA))
	full := writeFullRun(t, dir, nil, []string{"TestB"})
	code, out, errOut := runAuditCmd(context.Background(), noRunners(t), "--full-run", full, "--since-run", "r1", "--commit", shas[0][:12], "--json")
	a := decodeAudit(t, out)
	if code != 0 || a.Commit != shas[0] || a.Audited != 1 || a.Misses != 1 {
		t.Errorf("code=%d err=%q report=%+v; --commit must pick the older commit (abbreviated, resolved to a full SHA)", code, errOut, a)
	}
}

func TestAuditReadsTheConfiguredTelemetryPathAndWarnsAboutATruncatedLine(t *testing.T) {
	dir, shas := auditRepo(t, 1)
	if err := os.WriteFile(filepath.Join(dir, "hazard.yml"), []byte("version: 1\ntelemetry:\n  enabled: true\n  path: logs/t.jsonl\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "logs", "t.jsonl")
	if err := telemetry.Append(path, auditRecord("r1", shas[0], time.Minute, auditA)); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"id":"half`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	full := writeFullRun(t, dir, nil, []string{"TestB"})
	code, out, errOut := runAuditCmd(context.Background(), noRunners(t), "--full-run", full, "--since-run", "r1", "--json")
	a := decodeAudit(t, out)
	if code != 0 || a.Audited != 1 || !strings.Contains(errOut, "warning:") || !strings.Contains(errOut, "truncated") {
		t.Errorf("code=%d err=%q report=%+v", code, errOut, a)
	}
}

func TestAuditWithoutTelemetryFindsNothingToAudit(t *testing.T) {
	dir, _ := auditRepo(t, 1)
	full := writeFullRun(t, dir, nil, []string{"TestB"})
	auditAt := auditT0.Format(time.RFC3339)
	code, out, errOut := runAuditCmd(context.Background(), noRunners(t), "--full-run", full, "--since-run", auditAt, "--json")
	a := decodeAudit(t, out)
	if code != 0 || a.Audited != 0 || a.Unattributed != 1 {
		t.Errorf("code=%d err=%q report=%+v", code, errOut, a)
	}
	if code, _, _ := runAuditCmd(context.Background(), noRunners(t), "--full-run", full, "--since-run", "r1"); code != 2 {
		t.Errorf("an ID cannot be found in a missing telemetry file: code=%d, want 2", code)
	}
}

func TestAuditWarnsWhenTheFullRunReportIsIncomplete(t *testing.T) {
	dir, shas := auditRepo(t, 1)
	appendRecords(t, dir, auditRecord("r1", shas[0], time.Minute, auditA))
	// tests="3" but only one testcase: the suite is incomplete.
	incomplete := `<?xml version="1.0"?><testsuites><testsuite name="s" tests="3"><testcase classname="example.com/audit/p" name="TestB"><failure/></testcase></testsuite></testsuites>`
	path := filepath.Join(dir, "incomplete.xml")
	if err := os.WriteFile(path, []byte(incomplete), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := runAuditCmd(context.Background(), noRunners(t), "--full-run", path, "--since-run", "r1")
	if code != 0 || !strings.Contains(errOut, "incomplete") {
		t.Errorf("code=%d err=%q, want a warning that failures may be missing", code, errOut)
	}
}

// TestAuditBisectNamesTheCommitThatBrokeTheTest plants a miss in a real Go
// repository: commit 3 breaks TestB, the selection recorded at commit 3 left it
// out, and the full run at commit 5 fails it. The bisect must name commit 3.
func TestAuditBisectNamesTheCommitThatBrokeTheTest(t *testing.T) {
	isolateGit(t)
	dir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(dir, "p"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitTest(t, dir, "init", "-q", "-b", "main")
	t.Chdir(dir)
	write := func(rel, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(rel)), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	commit := func(n int) string {
		t.Helper()
		gitTest(t, dir, "add", "-A")
		gitTest(t, dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "-m", fmt.Sprintf("commit %d", n))
		return gitTest(t, dir, "rev-parse", "HEAD")
	}
	value := func(v int) string { return fmt.Sprintf("package p\n\nfunc Value() int { return %d }\n", v) }
	write("go.mod", "module example.com/audit\n\ngo 1.21\n")
	write(".gitignore", ".hazard/\nfull.xml\n")
	write("p/p_test.go", "package p\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {}\n\nfunc TestB(t *testing.T) {\n\tif Value() != 1 {\n\t\tt.Fatalf(\"Value() = %d\", Value())\n\t}\n}\n")
	write("p/p.go", value(1))
	var shas []string
	shas = append(shas, commit(1))
	write("notes.txt", "two")
	shas = append(shas, commit(2))
	write("p/p.go", value(2)) // commit 3 breaks TestB
	shas = append(shas, commit(3))
	write("notes.txt", "four")
	shas = append(shas, commit(4))
	write("notes.txt", "five")
	shas = append(shas, commit(5))

	appendRecords(t, dir, auditRecord("sel-at-3", shas[2], time.Minute, auditA)) // omitted TestB
	full := writeFullRun(t, dir, []string{"TestA"}, []string{"TestB"})

	code, out, errOut := runAuditCmd(context.Background(), nativeRunners(), "--full-run", full, "--since-run", auditT0.Format(time.RFC3339), "--bisect", "--json")
	if code != 0 {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
	}
	a := decodeAudit(t, out)
	if a.Commit != shas[4] || a.Misses != 1 || a.Attributed != 1 || a.Unattributed != 0 || a.Audited != 0 {
		t.Fatalf("report = %+v", a)
	}
	m := a.MissList[0]
	if m.SelectionID != "sel-at-3" || m.TestID != auditB || !m.Attributed || m.BlameWindow != shas[1]+".."+shas[2] {
		t.Errorf("miss = %+v, want sel-at-3 blamed for TestB with window %s..%s (commit 3 broke it)", m, shas[1], shas[2])
	}
	if n := fieldMissCount(t, dir); n != 1 {
		t.Errorf("field_misses rows = %d, want 1", n)
	}
	if listing := gitTest(t, dir, "worktree", "list", "--porcelain"); strings.Count(listing, "worktree ") != 1 {
		t.Errorf("the bisect worktrees must be removed:\n%s", listing)
	}
}
