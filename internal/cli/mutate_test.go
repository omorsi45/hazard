package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/omorsi45/hazard/internal/mutate"
	"github.com/spf13/cobra"
)

// mutateRecorder stands in for mutate.Run: it records the options it is given
// and answers with a canned summary or error.
type mutateRecorder struct {
	calls int
	opts  mutate.Options
	sum   mutate.Summary
	err   error
}

func (r *mutateRecorder) run(_ context.Context, opts mutate.Options) (mutate.Summary, error) {
	r.calls++
	r.opts = opts
	if opts.Log != nil {
		fmt.Fprintln(opts.Log, "progress line")
	}
	return r.sum, r.err
}

// runMutate executes the mutate command built around run inside a root command
// that maps errors to exit codes the way Execute does.
func runMutate(ctx context.Context, run mutateRunFunc, args ...string) (int, string, string) {
	root := &cobra.Command{Use: "hazard", SilenceUsage: true, SilenceErrors: true}
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError{err} })
	root.PersistentFlags().Bool("json", false, "")
	root.PersistentFlags().CountP("verbose", "v", "")
	root.AddCommand(newMutateCommand(run))
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(append([]string{"mutate"}, args...))
	err := root.ExecuteContext(ctx)
	code := 0
	if err != nil {
		code = 70
		var usage usageError
		if errors.As(err, &usage) {
			code = 2
		}
		errOut.WriteString(err.Error())
	}
	return code, out.String(), errOut.String()
}

func requireSameDir(t *testing.T, got, want string) {
	t.Helper()
	g, gotErr := os.Stat(got)
	w, wantErr := os.Stat(want)
	if gotErr != nil || wantErr != nil || !os.SameFile(g, w) {
		t.Errorf("directory %q is not %q (%v, %v)", got, want, gotErr, wantErr)
	}
}

const mutateFixtureConfig = "version: 1\nlanguages:\n  go:\n    packages: [\"./calc\", \"./app\"]\n    tags: [\"slow\", \"extra\"]\nimpact:\n  max_hops: 2\n"

// mutateRepo creates a git repository holding a configuration file and returns
// its directory.
func mutateRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitInit(t, dir, map[string]string{"hazard.yml": mutateFixtureConfig, "a/x.txt": "x\n", "b/y.txt": "y\n"})
	return dir
}

func TestMutateDefaultsMapToOptions(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir, map[string]string{"a/x.txt": "x\n"})
	t.Chdir(dir)
	rec := &mutateRecorder{}
	code, out, errOut := runMutate(context.Background(), rec.run)
	if code != 0 || rec.calls != 1 {
		t.Fatalf("code=%d calls=%d out=%q err=%q", code, rec.calls, out, errOut)
	}
	o := rec.opts
	requireSameDir(t, o.RepoDir, dir)
	want := mutate.Options{
		RepoDir: o.RepoDir, Base: "HEAD", Files: []string{"internal/**/*.go"}, N: 500,
		Operators: []string{"cmp", "logic", "const", "ret", "stmt", "cfg"},
		Timeout:   5 * time.Minute, Concurrency: 4, Packages: []string{"./..."}, Tags: []string{}, MaxHops: 3,
		Out:    filepath.Join(o.RepoDir, ".hazard", "mutants.jsonl"),
		DBPath: filepath.Join(o.RepoDir, ".hazard", "hazard.db"),
	}
	if !reflect.DeepEqual(o, want) {
		t.Errorf("options = %+v\nwant      %+v", o, want)
	}
}

func TestMutateEveryFlagReachesTheOptions(t *testing.T) {
	dir := mutateRepo(t)
	t.Chdir(filepath.Join(dir, "a")) // a subdirectory: the repository root is still the git top level
	configPath := filepath.Join(dir, "hazard.yml")

	t.Run("flags", func(t *testing.T) {
		rec := &mutateRecorder{}
		code, out, errOut := runMutate(context.Background(), rec.run,
			"--base", "main~1", "--files", "a/*.go, b/*.go", "--n", "7", "--operators", "ret,cmp",
			"--timeout", "45s", "--concurrency", "3", "--out", "out/m.jsonl", "--packages", "./x,./y", "--config", configPath)
		if code != 0 || rec.calls != 1 {
			t.Fatalf("code=%d calls=%d out=%q err=%q", code, rec.calls, out, errOut)
		}
		o := rec.opts
		requireSameDir(t, o.RepoDir, dir)
		want := mutate.Options{
			RepoDir: o.RepoDir, Base: "main~1", Files: []string{"a/*.go", "b/*.go"}, N: 7,
			Operators: []string{"cmp", "ret"}, // canonical order, whatever the order given
			Timeout:   45 * time.Second, Concurrency: 3,
			Packages: []string{"./x", "./y"},                     // --packages beats the configuration
			Tags:     []string{"slow", "extra"},                  // from the configuration
			MaxHops:  2,                                          // from the configuration
			Out:      filepath.Join(o.RepoDir, "out", "m.jsonl"), // relative to the repository root, not to the directory the command ran in
			DBPath:   filepath.Join(o.RepoDir, ".hazard", "hazard.db"),
		}
		if !reflect.DeepEqual(o, want) {
			t.Errorf("options = %+v\nwant      %+v", o, want)
		}
	})

	t.Run("packages from the configuration", func(t *testing.T) {
		rec := &mutateRecorder{}
		if code, _, errOut := runMutate(context.Background(), rec.run, "--config", configPath); code != 0 {
			t.Fatalf("code=%d err=%q", code, errOut)
		}
		if want := []string{"./calc", "./app"}; !reflect.DeepEqual(rec.opts.Packages, want) {
			t.Errorf("packages = %v, want %v from languages.go.packages", rec.opts.Packages, want)
		}
	})

	t.Run("absolute output path", func(t *testing.T) {
		rec := &mutateRecorder{}
		abs := filepath.Join(t.TempDir(), "elsewhere", "m.jsonl")
		if code, _, errOut := runMutate(context.Background(), rec.run, "--config", configPath, "--out", abs); code != 0 {
			t.Fatalf("code=%d err=%q", code, errOut)
		}
		if rec.opts.Out != abs {
			t.Errorf("out = %q, want the absolute path kept: %q", rec.opts.Out, abs)
		}
	})
}

func TestMutateUsageErrorsNeverRun(t *testing.T) {
	dir := mutateRepo(t)
	t.Chdir(dir)
	badConfig := filepath.Join(t.TempDir(), "bad.yml")
	if err := os.WriteFile(badConfig, []byte("version: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]string{
		"unknown operator":       {"--operators", "bogus"},
		"empty operators":        {"--operators", ""},
		"empty operator in list": {"--operators", "cmp,,ret"},
		"empty files":            {"--files", ""},
		"only separators":        {"--files", " , "},
		"negative n":             {"--n", "-1"},
		"zero timeout":           {"--timeout", "0s"},
		"negative timeout":       {"--timeout", "-5s"},
		"unparsable timeout":     {"--timeout", "wrong"},
		"zero concurrency":       {"--concurrency", "0"},
		"negative concurrency":   {"--concurrency", "-2"},
		"empty packages":         {"--packages", ""},
		"unknown flag":           {"--no-such-flag"},
		"missing config":         {"--config", filepath.Join(dir, "absent.yml")},
		"invalid config":         {"--config", badConfig},
		"positional argument":    {"extra"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := &mutateRecorder{}
			code, out, errOut := runMutate(context.Background(), rec.run, args...)
			if code != 2 || rec.calls != 0 || out != "" || errOut == "" {
				t.Errorf("code=%d calls=%d out=%q err=%q, want exit 2 without running", code, rec.calls, out, errOut)
			}
		})
	}
}

func TestMutateRunFailuresExit70(t *testing.T) {
	t.Chdir(mutateRepo(t))
	rec := &mutateRecorder{err: errors.New("mutate: baseline is not green (run 1): exit 1")}
	code, out, errOut := runMutate(context.Background(), rec.run)
	if code != 70 || out != "" || !strings.Contains(errOut, "baseline is not green") {
		t.Errorf("run failure: code=%d out=%q err=%q, want 70 naming the cause", code, out, errOut)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	canceled := &mutateRecorder{err: context.Canceled}
	code, out, errOut = runMutate(ctx, canceled.run)
	if code != 70 || out != "" || !strings.Contains(errOut, "canceled") {
		t.Errorf("canceled: code=%d out=%q err=%q, want 70", code, out, errOut)
	}
}

func TestMutateOutsideAGitWorkTreeFailsBeforeRunning(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir))
	t.Chdir(dir)
	rec := &mutateRecorder{}
	code, out, errOut := runMutate(context.Background(), rec.run)
	if code != 70 || rec.calls != 0 || out != "" || !strings.Contains(errOut, "git work tree") {
		t.Errorf("code=%d calls=%d out=%q err=%q, want 70 without running", code, rec.calls, out, errOut)
	}
}

func fixtureMutateSummary() mutate.Summary {
	return mutate.Summary{
		BaseSHA: "abc123def", Candidates: 17, Executed: 11,
		ByStatus:   map[string]int{"killed": 4, "survived": 3, "build_error": 2, "timeout": 1, "unconfirmed": 1},
		ByOperator: map[string]int{"cmp": 5, "ret": 4, "cfg": 2},
		Usable:     4, UnreachableMutants: 3, FullyUnreachableMutants: 2, BeyondMaxHopsMutants: 1,
		BaselineSeconds: 12.5,
		Records:         []mutate.Record{{ID: "r1-secret-record", Status: "killed"}},
	}
}

func TestMutateTextSummaryPrintsEveryCount(t *testing.T) {
	t.Chdir(mutateRepo(t))
	rec := &mutateRecorder{sum: fixtureMutateSummary()}
	code, out, errOut := runMutate(context.Background(), rec.run, "--out", "res.jsonl")
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
	for _, want := range []string{
		"base=abc123def", "candidates=17", "executed=11", "usable=4", "baseline_seconds=12.5",
		"killed=4", "survived=3", "build_error=2", "timeout=1", "unconfirmed=1",
		"cfg=2", "cmp=5", "ret=4",
		"unreachable_mutants=3", "fully_unreachable_mutants=2", "beyond_max_hops_mutants=1",
		"output: " + rec.opts.Out, "database: " + rec.opts.DBPath,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary omitted %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "r1-secret-record") {
		t.Errorf("the text summary lists the records:\n%s", out)
	}

	// A status that did not occur is still printed, as zero.
	rec = &mutateRecorder{sum: mutate.Summary{BaseSHA: "abc", ByStatus: map[string]int{"survived": 1}, ByOperator: map[string]int{"cmp": 1}}}
	if code, out, errOut = runMutate(context.Background(), rec.run); code != 0 || !strings.Contains(out, "killed=0") || !strings.Contains(out, "timeout=0") {
		t.Errorf("code=%d err=%q, want zero counts printed:\n%s", code, errOut, out)
	}
}

func TestMutateJSONSummaryOmitsRecords(t *testing.T) {
	t.Chdir(mutateRepo(t))
	rec := &mutateRecorder{sum: fixtureMutateSummary()}
	code, out, errOut := runMutate(context.Background(), rec.run, "--json", "-vv")
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
	if rec.opts.Log != nil {
		t.Errorf("--json must not enable progress output")
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %q: %v", out, err)
	}
	if _, ok := got["records"]; ok {
		t.Errorf("the JSON summary carries the records: %s", out)
	}
	want := map[string]any{
		"base_sha": "abc123def", "candidates": 17.0, "executed": 11.0, "usable": 4.0,
		"unreachable_mutants": 3.0, "fully_unreachable_mutants": 2.0, "beyond_max_hops_mutants": 1.0, "baseline_seconds": 12.5,
		"by_status":   map[string]any{"killed": 4.0, "survived": 3.0, "build_error": 2.0, "timeout": 1.0, "unconfirmed": 1.0},
		"by_operator": map[string]any{"cmp": 5.0, "ret": 4.0, "cfg": 2.0},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("JSON summary = %v\nwant          %v", got, want)
	}
}

func TestMutateProgressIsVerboseOnly(t *testing.T) {
	t.Chdir(mutateRepo(t))
	quiet := &mutateRecorder{}
	if code, _, errOut := runMutate(context.Background(), quiet.run); code != 0 || errOut != "" || quiet.opts.Log != nil {
		t.Errorf("without -v: code=%d err=%q log=%v, want silence", code, errOut, quiet.opts.Log)
	}
	loud := &mutateRecorder{}
	code, _, errOut := runMutate(context.Background(), loud.run, "-v")
	if code != 0 || loud.opts.Log == nil || !strings.Contains(errOut, "progress line") {
		t.Errorf("with -v: code=%d log=%v err=%q, want the progress on stderr", code, loud.opts.Log, errOut)
	}
}

func TestMutateHelpStatesWhatItRuns(t *testing.T) {
	code, out, errOut := invoke(context.Background(), "mutate", "--help")
	if code != 0 || errOut != "" {
		t.Fatalf("help code=%d err=%q", code, errOut)
	}
	for _, phrase := range []string{
		"builds and runs the test suite many times in temporary copies",
		"--base", "--files", "--n", "--operators", "--timeout", "--concurrency", "--out", "--packages", "--config",
	} {
		if !strings.Contains(out, phrase) {
			t.Errorf("help omitted %q:\n%s", phrase, out)
		}
	}
}

// The registered command is the real one, not the scaffold stub.
func TestMutateIsWiredToTheDriver(t *testing.T) {
	dir := mutateRepo(t)
	t.Chdir(dir)
	code, out, errOut := invoke(context.Background(), "mutate", "--operators", "bogus")
	if code != 2 || out != "" || !strings.Contains(errOut, "bogus") {
		t.Errorf("bad operator: code=%d out=%q err=%q, want exit 2 naming it", code, out, errOut)
	}
	code, out, errOut = invoke(context.Background(), "mutate", "--base", "no-such-revision")
	if code != 70 || out != "" || !strings.Contains(errOut, "no-such-revision") || strings.Contains(errOut, "not implemented") {
		t.Errorf("unknown base: code=%d out=%q err=%q, want 70 from the driver", code, out, errOut)
	}
	for _, p := range []string{".hazard"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err == nil {
			t.Errorf("%s exists after a refused run", p)
		}
	}
}
