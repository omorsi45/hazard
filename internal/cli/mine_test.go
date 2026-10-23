package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/omorsi45/hazard/internal/mine"
)

func TestMineHelpAndRequiredProvider(t *testing.T) {
	code, out, errOut := invoke(context.Background(), "ci-mine", "--help")
	if code != 0 || errOut != "" {
		t.Fatalf("help code=%d stderr=%q", code, errOut)
	}
	for _, phrase := range []string{"--language", "--url", "YYYY-MM-DD", "UTC", "inclusive", "exclusive"} {
		if !strings.Contains(out, phrase) {
			t.Errorf("help omitted %q", phrase)
		}
	}
	code, out, errOut = invoke(context.Background(), "ci-mine", "--project", "owner/repo", "--since", "2026-01-01", "--until", "2026-01-02")
	if code != 2 || out != "" || !strings.Contains(errOut, "--provider") {
		t.Fatalf("missing provider code=%d stdout=%q stderr=%q", code, out, errOut)
	}
}

func executeMine(t *testing.T, run func(context.Context, mine.Request, mine.Options) (mine.Stats, error), args ...string) (string, string, error) {
	t.Helper()
	root := NewRoot()
	for _, cmd := range root.Commands() {
		if cmd.Name() == "ci-mine" {
			root.RemoveCommand(cmd)
		}
	}
	root.AddCommand(newMineCommand(run))
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(append([]string{"ci-mine"}, args...))
	err := root.ExecuteContext(context.Background())
	return out.String(), errOut.String(), err
}

func mineArgs() []string {
	return []string{"--provider", "github", "--project", "owner/repo", "--since", "2026-03-08", "--until", "2026-03-08"}
}

func TestMineRequestMapping(t *testing.T) {
	t.Setenv("HAZARD_CI_TOKEN", "fixture-default-token")
	t.Setenv("MINE_FIXTURE_TOKEN", "fixture-explicit-token")
	for _, tc := range []struct {
		name string
		args []string
		want mine.Request
		opts mine.Options
	}{
		{
			name: "defaults_automatic_language",
			want: mine.Request{Provider: "github", Project: "owner/repo", Branch: "main", TokenEnv: "HAZARD_CI_TOKEN", JUnitGlob: "**/junit*.xml", MaxPipelines: 5000, Concurrency: 8, DBPath: ".hazard/hazard.db", RawDir: ".hazard/raw"},
			opts: mine.Options{Token: "fixture-default-token"},
		},
		{
			name: "all_documented_flags_python",
			args: []string{"--provider", "gitlab", "--project", "group/nested/repo", "--branch", "", "--token-env", "MINE_FIXTURE_TOKEN", "--junit-glob", "reports/**/*.xml", "--max-pipelines", "12", "--concurrency", "3", "--resume", "--language", "python", "--url", "https://gitlab.example/api/v4", "-vv"},
			want: mine.Request{Provider: "gitlab", Project: "group/nested/repo", Branch: "", TokenEnv: "MINE_FIXTURE_TOKEN", JUnitGlob: "reports/**/*.xml", MaxPipelines: 12, Concurrency: 3, Resume: true, Language: "python", DBPath: ".hazard/hazard.db", RawDir: ".hazard/raw"},
			opts: mine.Options{APIBase: "https://gitlab.example/api/v4", Token: "fixture-explicit-token"},
		},
		{
			name: "explicit_go",
			args: []string{"--language", "go"},
			want: mine.Request{Provider: "github", Project: "owner/repo", Branch: "main", TokenEnv: "HAZARD_CI_TOKEN", JUnitGlob: "**/junit*.xml", MaxPipelines: 5000, Concurrency: 8, Language: "go", DBPath: ".hazard/hazard.db", RawDir: ".hazard/raw"},
			opts: mine.Options{Token: "fixture-default-token"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			var got mine.Request
			var opts mine.Options
			_, _, err := executeMine(t, func(_ context.Context, req mine.Request, options mine.Options) (mine.Stats, error) {
				calls++
				got, opts = req, options
				return mine.Stats{}, nil
			}, append(mineArgs(), tc.args...)...)
			tc.want.Since = time.Date(2026, 3, 8, 0, 0, 0, 0, time.UTC)
			tc.want.Until = tc.want.Since.AddDate(0, 0, 1)
			if err != nil || calls != 1 || !reflect.DeepEqual(got, tc.want) || !reflect.DeepEqual(opts, tc.opts) {
				t.Fatalf("request/options mapping failed: error=%v calls=%d request=%+v", err, calls, got)
			}
			encoded, err := json.Marshal(struct {
				Request mine.Request
				Options mine.Options
			}{got, opts})
			if err != nil || strings.Contains(string(encoded), tc.opts.Token) || strings.Contains(string(encoded), "APIBase") {
				t.Fatalf("credential/options serialization leaked: error=%v", err)
			}
		})
	}
}

func TestMineUTCDateBoundsAndAnonymousToken(t *testing.T) {
	t.Setenv("MINE_EMPTY_TOKEN", "")
	for _, day := range []string{"2026-03-08", "2026-11-01", "2024-02-29"} {
		_, _, err := executeMine(t, func(_ context.Context, req mine.Request, opts mine.Options) (mine.Stats, error) {
			want, parseErr := time.Parse("2006-01-02", day)
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			if !req.Since.Equal(want) || !req.Until.Equal(want.AddDate(0, 0, 1)) || req.Since.Location() != time.UTC || req.Until.Location() != time.UTC || opts.Token != "" {
				t.Fatalf("UTC bounds or anonymous token changed: request=%+v", req)
			}
			return mine.Stats{}, nil
		}, append(mineArgs(), "--since", day, "--until", day, "--token-env", "MINE_EMPTY_TOKEN")...)
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestMineValidationBeforeSideEffects(t *testing.T) {
	cases := map[string][]string{
		"provider":                {"--provider", "other"},
		"project_missing":         {"--project", ""},
		"project_shape":           {"--project", "owner"},
		"project_traversal":       {"--project", "../repo"},
		"project_extra_segment":   {"--project", "owner/sub/repo"},
		"gitlab_zero_id":          {"--provider", "gitlab", "--project", "0"},
		"gitlab_id_overflow":      {"--provider", "gitlab", "--project", "9223372036854775808"},
		"language":                {"--language", "ruby"},
		"since_missing":           {"--since", ""},
		"until_missing":           {"--until", ""},
		"malformed_date":          {"--since", "2026-02-30"},
		"timestamp_unsupported":   {"--since", "2026-03-08T00:00:00Z"},
		"reverse_bounds":          {"--since", "2026-03-10"},
		"equal_exclusive_bounds":  {"--since", "2026-03-09"},
		"zero_start":              {"--since", "0001-01-01"},
		"exclusive_year_overflow": {"--until", "9999-12-31"},
		"glob_empty":              {"--junit-glob", ""},
		"glob_syntax":             {"--junit-glob", "["},
		"glob_traversal":          {"--junit-glob", "../junit.xml"},
		"glob_absolute":           {"--junit-glob", "/junit.xml"},
		"glob_drive":              {"--junit-glob", "C:/junit.xml"},
		"glob_backslash":          {"--junit-glob", `reports\*.xml`},
		"glob_control":            {"--junit-glob", "reports/\njunit.xml"},
		"token_env_empty":         {"--token-env", ""},
		"token_env_digit":         {"--token-env", "1TOKEN"},
		"token_env_punctuation":   {"--token-env", "TOKEN-NAME"},
		"pipeline_limit":          {"--max-pipelines", "0"},
		"concurrency":             {"--concurrency", "-1"},
		"positionals":             {"unexpected"},
		"github_url":              {"--url", "https://user:fixture-url-secret@example.test/?token=fixture-url-secret"},
	}
	for name, flags := range cases {
		t.Run(name, func(t *testing.T) {
			calls := 0
			out, errOut, err := executeMine(t, func(context.Context, mine.Request, mine.Options) (mine.Stats, error) {
				calls++
				return mine.Stats{}, nil
			}, append(mineArgs(), flags...)...)
			var usage usageError
			if !errors.As(err, &usage) || calls != 0 || out != "" || errOut != "" {
				t.Fatalf("invalid input reached acquisition: error=%v calls=%d stdout=%q stderr=%q", err, calls, out, errOut)
			}
			if strings.Contains(err.Error(), "fixture-url-secret") {
				t.Fatal("URL credential leaked in validation error")
			}
		})
	}
}

func mineFixtureStats() mine.Stats {
	return mine.Stats{
		RequestedSince: time.Date(2026, 3, 8, 0, 0, 0, 0, time.UTC),
		RequestedUntil: time.Date(2026, 3, 9, 0, 0, 0, 0, time.UTC),
		InspectedRuns:  4, InspectedAttempts: 5, InspectedArtifacts: 6,
		RawCases: 9, FactualResults: 3, FactualResultBearingCommits: 2,
		ReplayDuplicates: 7, IncompleteUnits: 2, CappedUnits: 1,
		ActiveGaps: 2, ResolvedGaps: 1,
		AcquisitionExclusions: map[string]int{"z_missing": 2, "a_unknown": 1},
		Activity:              mine.Activity{InspectedRuns: 1, InspectedAttempts: 2, PersistedArtifacts: 3, NewRawCases: 4, ReplayCases: 5, NetFactualResults: -2, IncompleteRuns: 1, CappedRuns: 1, GapsByReason: map[string]int{"z_missing": 2, "a_unknown": 1}},
	}
}

func TestMineOutputPreservesPartialStats(t *testing.T) {
	stats := mineFixtureStats()
	primary := errors.New("fixture acquisition failed")
	for _, runErr := range []error{nil, primary} {
		for _, jsonMode := range []bool{false, true} {
			args := mineArgs()
			if jsonMode {
				args = append(args, "--json")
			}
			run := func(context.Context, mine.Request, mine.Options) (mine.Stats, error) { return stats, runErr }
			out, errOut, err := executeMine(t, run, args...)
			if !errors.Is(err, runErr) || errOut != "" {
				t.Fatalf("partial return changed: error=%v stderr=%q", err, errOut)
			}
			if jsonMode {
				var got mine.Stats
				if decodeErr := json.Unmarshal([]byte(out), &got); decodeErr != nil || !reflect.DeepEqual(got, stats) {
					t.Fatalf("actual partial Stats missing: stdout=%q error=%v", out, decodeErr)
				}
			} else {
				for _, phrase := range []string{"stored:", "activity:", "coverage:", "raw_cases=9", "net_factual_results=-2", "incomplete_units=2", "capped_units=1", "active_gaps=2"} {
					if !strings.Contains(out, phrase) {
						t.Errorf("text omitted actual stats %q: %q", phrase, out)
					}
				}
				if strings.Contains(out, "complete coverage") {
					t.Fatal("partial coverage described as complete")
				}
			}
			again, _, _ := executeMine(t, run, args...)
			if out != again {
				t.Fatal("Stats output is nondeterministic")
			}
		}
	}
}

func TestMineVerboseAggregatesAndJSONIsolation(t *testing.T) {
	stats := mineFixtureStats()
	run := func(context.Context, mine.Request, mine.Options) (mine.Stats, error) { return stats, nil }
	plain, _, _ := executeMine(t, run, mineArgs()...)
	verbose, detail, err := executeMine(t, run, append(mineArgs(), "-vv")...)
	if err != nil || verbose != plain || detail != "ci-mine: "+stats.String()+"\n" {
		t.Fatalf("verbose aggregate output changed: error=%v stderr=%q", err, detail)
	}
	out, errOut, err := executeMine(t, run, append(mineArgs(), "--json", "-v")...)
	var got mine.Stats
	if json.Unmarshal([]byte(out), &got) != nil || !reflect.DeepEqual(got, stats) || err != nil || errOut != "" {
		t.Fatalf("JSON contaminated by human progress: error=%v stdout=%q stderr=%q", err, out, errOut)
	}
}

func TestMineExecuteUsageAndSafeRuntimeError(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, args := range [][]string{
		append(mineArgs(), "--language", "ruby", "--json"),
		append(mineArgs(), "--since", "bad", "--json"),
		append(mineArgs(), "--url", "https://user:fixture-url-secret@example.test/?token=fixture-url-secret", "--json"),
		append(mineArgs(), "unexpected", "--json"),
	} {
		code, out, errOut := invoke(context.Background(), append([]string{"ci-mine"}, args...)...)
		var payload struct {
			Error string `json:"error"`
			Code  int    `json:"code"`
		}
		if json.Unmarshal([]byte(errOut), &payload) != nil || code != 2 || payload.Code != 2 || out != "" || strings.Contains(errOut, "fixture-url-secret") {
			t.Fatalf("usage/JSON contract changed: code=%d stdout=%q stderr=%q", code, out, errOut)
		}
	}
	args := append(mineArgs(), "--provider", "gitlab", "--project", "42", "--url", "https://user:fixture-url-secret@example.test/api/v4?token=fixture-url-secret", "--json", "-v")
	code, out, errOut := invoke(context.Background(), append([]string{"ci-mine"}, args...)...)
	var payload struct {
		Error string `json:"error"`
		Code  int    `json:"code"`
	}
	if json.Unmarshal([]byte(errOut), &payload) != nil || code != 70 || payload.Code != 70 || !strings.Contains(payload.Error, "invalid_url") || strings.Contains(out+errOut, "fixture-url-secret") {
		t.Fatalf("safe runtime/JSON contract changed: code=%d stdout=%q stderr=%q", code, out, errOut)
	}
}

func TestMineOutputFailurePreservesAcquisitionError(t *testing.T) {
	primary := errors.New("fixture acquisition failed")
	for _, jsonMode := range []bool{false, true} {
		root := NewRoot()
		for _, cmd := range root.Commands() {
			if cmd.Name() == "ci-mine" {
				root.RemoveCommand(cmd)
			}
		}
		root.AddCommand(newMineCommand(func(context.Context, mine.Request, mine.Options) (mine.Stats, error) {
			return mineFixtureStats(), primary
		}))
		root.SetOut(failingWriter{})
		var errOut bytes.Buffer
		root.SetErr(&errOut)
		args := append([]string{"ci-mine"}, mineArgs()...)
		if jsonMode {
			args = append(args, "--json")
		}
		root.SetArgs(args)
		err := root.ExecuteContext(context.Background())
		if !errors.Is(err, primary) || !strings.Contains(err.Error(), "injected write failure") {
			t.Fatalf("output/acquisition error lost: %v", err)
		}
	}
}
