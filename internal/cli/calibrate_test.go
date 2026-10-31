package cli

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/omorsi45/hazard/internal/config"
	"github.com/omorsi45/hazard/internal/control"
	"github.com/omorsi45/hazard/internal/engine"
	"github.com/omorsi45/hazard/internal/impact"
	"github.com/omorsi45/hazard/internal/index"
	"github.com/omorsi45/hazard/internal/labels"
	"github.com/omorsi45/hazard/internal/predict"
	"github.com/omorsi45/hazard/internal/store"
)

const (
	corpusObsFormat = "2006-01-02T15:04:05.000000000Z"
	refusalLine     = "calibrate: refused: need n >= 299, have 0"
)

// corpus seeds .hazard/hazard.db of the working directory with CI observations
// the way mining stores them, so that `labels build` derives strong, weak and
// excluded candidates from facts instead of from rows written by hand. This is
// test-only fixture data.
type corpus struct {
	t    *testing.T
	db   *sql.DB
	n    int
	base time.Time
}

// seedCorpus creates the store, lets fill add observations and then freezes
// them with `labels build`, which makes the build current.
func seedCorpus(t *testing.T, fill func(c *corpus)) {
	t.Helper()
	s, err := store.Open(labelsDBPath)
	if err != nil {
		t.Fatal(err)
	}
	c := &corpus{t: t, db: s.DB(), base: time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)}
	mustExec(t, c.db, `INSERT INTO mining_sources(singleton,provider,project,observed_at) VALUES(1,'github','o/r',?)`, c.base.Format(corpusObsFormat))
	fill(c)
	// Close before building and before any replay: a replay copies the file.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := invoke(context.Background(), "labels", "build", "--before", "2026-01-01"); code != 0 {
		t.Fatalf("labels build: code=%d out=%q err=%q", code, out, errOut)
	}
}

// observe adds one report with one case of test at sha.
func (c *corpus) observe(sha string, parents []string, status, test string, authored time.Time) {
	c.t.Helper()
	c.n++
	unit := fmt.Sprintf("unit-%d", c.n)
	sum := sha256.Sum256([]byte(unit))
	content := hex.EncodeToString(sum[:])
	at := c.base.Add(time.Duration(c.n) * time.Minute).Format(corpusObsFormat)
	run, job := fmt.Sprintf("run-%d", c.n), "job-"+unit
	parentJSON, err := json.Marshal(parents)
	if err != nil {
		c.t.Fatal(err)
	}
	var authoredAt any
	if !authored.IsZero() {
		authoredAt = authored.UTC().Format(corpusObsFormat)
	}
	mustExec(c.t, c.db, `INSERT INTO mining_runs(provider,project,run_id,attempt_key,attempt,source_head_sha,branch,state,observed_at) VALUES('github','o/r',?,'1',1,?,'main','complete',?)`, run, sha, at)
	mustExec(c.t, c.db, `INSERT INTO ci_artifacts(provider,project,unit_key,content_sha256,run_id,artifact_id,job_id,attempt,environment_id,tested_sha,tested_revision_evidence,
		execution_evidence,environment_evidence,raw_blob_path,source_head_sha,branch,tested_authored_at,tested_parent_shas_json,tested_kinds_json,suite_incomplete,diagnostics_truncated,state,observed_at)
		VALUES('github','o/r',?,?,?,?,?,1,'linux',?,'rev','exec','env','raw/x',?,'main',?,?,'null',0,0,'complete',?)`,
		unit, content, run, "artifact-"+unit, job, sha, sha, authoredAt, string(parentJSON), at)
	key := struct {
		Source, Project, SHA, TestID, RunID, JobID, EnvironmentID, ArtifactID string
		Attempt                                                               int
	}{"github", "o/r", sha, test, run, job, "linux", "artifact-" + unit, 1}
	encoded, err := json.Marshal(key)
	if err != nil {
		c.t.Fatal(err)
	}
	mustExec(c.t, c.db, `INSERT INTO ci_observations(provider,project,unit_key,content_sha256,artifact_entry,entry_sha256,case_ordinal,raw_suite,raw_name,raw_classname,raw_file,status,
		raw_message,raw_type,raw_properties_json,suite_incomplete,diagnostics_truncated,test_id,identity_language,identity_package,identity_name,identity_file,identity_runner_id,
		unstable_id,identity_reason,identity_error,infra_reason,result_key,observed_at)
		VALUES('github','o/r',?,?,'report.xml',?,0,'s','n','c','f.go',?,'assertion','','{}',0,0,?,'go','p','T','f.go','',0,'','','',?,?)`,
		unit, content, content, status, test, string(encoded), at)
	mustExec(c.t, c.db, `INSERT INTO test_results(source,project,commit_sha,test_id,pipeline_id,job_id,environment_id,attempt,artifact_id,status,observed_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, key.Source, key.Project, key.SHA, key.TestID, key.RunID, key.JobID, key.EnvironmentID, key.Attempt, key.ArtifactID, status, at)
}

// strong adds the history of a strong candidate: the parent passes, the
// commit fails and a child of the commit passes.
func (c *corpus) strong(parent, sha, test string, authored time.Time) {
	c.t.Helper()
	c.weak(parent, sha, test, authored)
	c.observe("child-"+sha, []string{sha}, "pass", test, time.Time{})
}

// weak is a failing commit whose parent passes and that nothing recovers.
func (c *corpus) weak(parent, sha, test string, authored time.Time) {
	c.t.Helper()
	c.observe(parent, []string{parent + "-root"}, "pass", test, time.Time{})
	c.observe(sha, []string{parent}, "fail", test, authored)
}

// flaky is a strong-looking history in which the test also passes at the
// failing commit: an excluded candidate.
func (c *corpus) flaky(parent, sha, test string, authored time.Time) {
	c.t.Helper()
	c.strong(parent, sha, test, authored)
	c.observe(sha, []string{parent}, "pass", test, time.Time{})
}

func (c *corpus) mutation(id, killer string) {
	c.t.Helper()
	mustExec(c.t, c.db, `INSERT INTO mutants(id,file,line,operator,original,mutated,base_sha,status,created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		id, "a/a.go", 3, "cmp", ">", ">=", "base", "killed", "2026-01-01T00:00:00Z")
	mustExec(c.t, c.db, `INSERT INTO mutant_kills(mutant_id,test_id) VALUES(?,?)`, id, killer)
}

func rowCount(t *testing.T, table string) int {
	t.Helper()
	s, err := store.Open(labelsDBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	}()
	var n int
	if err := s.DB().QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// forbidPredictorLoad makes any load of the predictor a test failure and
// returns the number of loads.
func forbidPredictorLoad(t *testing.T) *int {
	t.Helper()
	calls := new(int)
	prev := calibrateLoadPredictor
	calibrateLoadPredictor = func(string, config.Config, string) (predict.Predictor, control.Identity, error) {
		*calls++
		t.Error("the predictor was loaded: a refusal must come before any artifact is read")
		return nil, control.Identity{}, errors.New("the predictor must not be loaded")
	}
	t.Cleanup(func() { calibrateLoadPredictor = prev })
	return calls
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

type refusalOutput struct {
	Refused  bool           `json:"refused"`
	Reason   string         `json:"reason"`
	NeedN    int            `json:"need_n"`
	HaveN    int            `json:"have_n"`
	Excluded map[string]int `json:"excluded"`
	BuildID  string         `json:"build_id"`
	Cutoff   string         `json:"cutoff"`
}

func decodeRefusal(t *testing.T, out string) refusalOutput {
	t.Helper()
	var r refusalOutput
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("stdout is not a refusal object: %v\n%s", err, out)
	}
	return r
}

func TestCalibrateRejectsInvalidInvocationsBeforeTouchingAnything(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"alpha zero", []string{"--alpha", "0", "--split-at", "2026-06-01"}},
		{"alpha one", []string{"--alpha", "1", "--split-at", "2026-06-01"}},
		{"alpha NaN", []string{"--alpha", "NaN", "--split-at", "2026-06-01"}},
		{"alpha negative", []string{"--alpha", "-0.1", "--split-at", "2026-06-01"}},
		{"delta zero", []string{"--delta", "0", "--split-at", "2026-06-01"}},
		{"delta above one", []string{"--delta", "1.5", "--split-at", "2026-06-01"}},
		{"grid not a range", []string{"--grid", "0.1", "--split-at", "2026-06-01"}},
		{"grid descending", []string{"--grid", "0.5:0.1:0.1", "--split-at", "2026-06-01"}},
		{"unit pair", []string{"--unit", "pair", "--split-at", "2026-06-01"}},
		{"unknown predictor", []string{"--predictor", "oracle", "--split-at", "2026-06-01"}},
		{"unknown split", []string{"--split", "chronological", "--split-at", "2026-06-01"}},
		{"temporal without split-at", []string{"--split", "temporal"}},
		{"split-at not a date", []string{"--split-at", "June"}},
		{"split-at with a time", []string{"--split-at", "2026-06-01T00:00:00Z"}},
		{"split-at not a calendar date", []string{"--split-at", "2026-02-30"}},
		{"random with split-at", []string{"--split", "random", "--split-at", "2026-06-01"}},
		{"empty out", []string{"--out", "", "--split-at", "2026-06-01"}},
		{"positional argument", []string{"--split-at", "2026-06-01", "extra"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			loads := forbidPredictorLoad(t)
			code, out, errOut := invoke(context.Background(), append([]string{"calibrate"}, tc.args...)...)
			if code != 2 || out != "" || !strings.HasPrefix(errOut, "error: ") {
				t.Fatalf("code=%d stdout=%q stderr=%q, want a usage error (2) on stderr only", code, out, errOut)
			}
			if *loads != 0 {
				t.Errorf("predictor loaded %d times", *loads)
			}
			if _, err := os.Stat(labelsDBPath); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a rejected invocation touched the database: %v", err)
			}
		})
	}
}

// Every flag of seed 6.6 parses: the command gets as far as the label build.
func TestCalibrateDocumentedFlagsParse(t *testing.T) {
	t.Chdir(t.TempDir())
	code, out, errOut := invoke(context.Background(), "calibrate", "--alpha", "0.01", "--delta", "0.05", "--split", "temporal", "--split-at", "2026-06-01",
		"--predictor", "ensemble", "--grid", "0.005:0.500:0.005", "--unit", "commit", "--out", "hazard.lock", "--seed", "1")
	if code != 1 || errOut != "" || !strings.Contains(out, "label build is unavailable") {
		t.Errorf("code=%d stdout=%q stderr=%q, want the unavailable refusal (exit 1)", code, out, errOut)
	}
}

func TestCalibrateHelpDocumentsFlagsAndExitCodes(t *testing.T) {
	code, out, errOut := invoke(context.Background(), "calibrate", "--help")
	if code != 0 || errOut != "" {
		t.Fatalf("help code=%d err=%q", code, errOut)
	}
	for _, want := range []string{
		"--alpha", "--delta", "--split", "--split-at", "--predictor", "--grid", "--unit", "--out", "--seed", "--config", "--json",
		"refusal is a result, not an error", "calibrate: refused: need n >= ", "Exit codes: 0 ", "1 refused", "2 usage", "70 internal",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("help omits %q", want)
		}
	}
}

// The refusal that the Definition of Done relies on: a current label build
// without usable units refuses with exactly this line, writes nothing and
// leaves an existing lock byte for byte alone.
func TestCalibrateRefusesWithTheExactLineOnAnEmptyCurrentBuild(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	seedCorpus(t, func(*corpus) {})
	lockPath := filepath.Join(dir, "hazard.lock")
	original := []byte("{\"keep\": \"this lock exactly\"}\n")
	if err := os.WriteFile(lockPath, original, 0o644); err != nil {
		t.Fatal(err)
	}
	loads := forbidPredictorLoad(t)

	code, out, errOut := invoke(context.Background(), "calibrate", "--split-at", "2026-06-01")
	if code != 1 || errOut != "" {
		t.Fatalf("code=%d stderr=%q, want exit 1 and nothing on stderr (a refusal is a result, not an error)", code, errOut)
	}
	if got := firstLine(out); got != refusalLine {
		t.Errorf("first line = %q, want %q\nfull output:\n%s", got, refusalLine, out)
	}
	if strings.Contains(out, "error:") {
		t.Errorf("stdout carries an error line:\n%s", out)
	}
	if !strings.Contains(out, "label_build: build_id=") || !strings.Contains(out, "cutoff=2026-01-01T00:00:00Z") || !strings.Contains(out, "excluded_units: none") {
		t.Errorf("the refusal does not name the label build and the excluded units:\n%s", out)
	}
	if got, err := os.ReadFile(lockPath); err != nil || !slices.Equal(got, original) {
		t.Errorf("the existing lock changed: %q, %v", got, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if want := []string{".hazard", "hazard.lock"}; !reflect.DeepEqual(names, want) {
		t.Errorf("working directory = %v, want %v: a refusal creates no file", names, want)
	}
	if n := rowCount(t, "calibrations"); n != 0 {
		t.Errorf("calibrations rows = %d, want 0", n)
	}
	if *loads != 0 {
		t.Errorf("predictor loaded %d times", *loads)
	}
}

func TestCalibrateRefusalJSON(t *testing.T) {
	t.Chdir(t.TempDir())
	seedCorpus(t, func(*corpus) {})
	code, out, errOut := invoke(context.Background(), "calibrate", "--split-at", "2026-06-01", "--json")
	if code != 1 || errOut != "" {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	r := decodeRefusal(t, out)
	if !r.Refused || r.NeedN != 299 || r.HaveN != 0 {
		t.Errorf("refusal = %+v, want refused with need_n 299 and have_n 0", r)
	}
	// The refusal names the frozen label build it was made against.
	if len(r.BuildID) != 64 || r.Cutoff != "2026-01-01T00:00:00Z" {
		t.Errorf("build_id=%q cutoff=%q, want the 64 character ID of the current build and its cutoff", r.BuildID, r.Cutoff)
	}
	if !strings.Contains(out, `"reason":"need n >= 299, have 0"`) {
		t.Errorf("the reason must read as the refusal line, unescaped:\n%s", out)
	}
	if strings.Contains(out, "calibrate: refused") {
		t.Errorf("the JSON output carries the text line:\n%s", out)
	}
}

func TestCalibrateRefusesWhenTheLabelBuildIsNotCurrent(t *testing.T) {
	const wantTail = ": run hazard labels build before calibrating"
	t.Run("stale", func(t *testing.T) {
		t.Chdir(t.TempDir())
		seedCorpus(t, func(c *corpus) { c.strong("p1", "c1", "go:p:T", time.Date(2025, 7, 1, 0, 0, 0, 0, time.UTC)) })
		s, err := store.Open(labelsDBPath)
		if err != nil {
			t.Fatal(err)
		}
		mustExec(t, s.DB(), `INSERT INTO tests(id,lang,package,name,file,first_seen,last_seen,quarantined) VALUES('go:p:T','go','p','T','f.go','2025-01-01T00:00:00Z','2025-01-01T00:00:00Z',1)`)
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		loads := forbidPredictorLoad(t)
		code, out, errOut := invoke(context.Background(), "calibrate", "--split-at", "2026-06-01")
		if code != 1 || errOut != "" || !strings.Contains(out, "label build is stale"+wantTail) {
			t.Errorf("code=%d stderr=%q stdout=%q, want exit 1 and the stale refusal", code, errOut, out)
		}
		if strings.Contains(out, "need n") {
			t.Errorf("units were counted for a stale build:\n%s", out)
		}
		if *loads != 0 {
			t.Errorf("predictor loaded %d times", *loads)
		}
		code, out, errOut = invoke(context.Background(), "calibrate", "--split-at", "2026-06-01", "--json")
		if r := decodeRefusal(t, out); code != 1 || errOut != "" || !r.Refused || !strings.Contains(r.Reason, "label build is stale") {
			t.Errorf("json: code=%d stderr=%q refusal=%+v", code, errOut, r)
		}
	})
	t.Run("no build in the database", func(t *testing.T) {
		t.Chdir(t.TempDir())
		s, err := store.Open(labelsDBPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		code, out, errOut := invoke(context.Background(), "calibrate", "--split-at", "2026-06-01")
		if code != 1 || errOut != "" || !strings.Contains(out, "label build is unavailable"+wantTail) {
			t.Errorf("code=%d stderr=%q stdout=%q, want exit 1 and the unavailable refusal", code, errOut, out)
		}
	})
	t.Run("no database", func(t *testing.T) {
		t.Chdir(t.TempDir())
		code, out, errOut := invoke(context.Background(), "calibrate", "--split-at", "2026-06-01")
		if code != 1 || errOut != "" || !strings.Contains(out, "label build is unavailable"+wantTail) {
			t.Errorf("code=%d stderr=%q stdout=%q, want exit 1 and the unavailable refusal", code, errOut, out)
		}
		if _, err := os.Stat(labelsDBPath); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("calibrate created the database: %v", err)
		}
	})
}

// Weak labels, mutation records and excluded (flaky) candidates never count
// toward have: the cohort query reads strong candidates only (G12, ruling c).
func TestCalibrateCountsOnlyStrongUnits(t *testing.T) {
	t.Chdir(t.TempDir())
	at := time.Date(2025, 7, 1, 0, 0, 0, 0, time.UTC)
	seedCorpus(t, func(c *corpus) {
		c.strong("p1", "c1", "go:p:T", at)
		c.strong("p2", "c2", "go:p:T", at.Add(24*time.Hour))
		c.weak("p3", "c3", "go:p:T", at.Add(48*time.Hour))
		c.flaky("p4", "c4", "go:p:T", at.Add(72*time.Hour))
		c.mutation("m1", "go:p:T")
		c.mutation("m2", "go:p:U")
	})
	if code, out, errOut := invoke(context.Background(), "labels", "stats", "--json"); code != 0 {
		t.Fatalf("labels stats: %d %q", code, errOut)
	} else {
		var stats labels.Report
		if err := json.Unmarshal([]byte(out), &stats); err != nil {
			t.Fatal(err)
		}
		if stats.StrongCandidates != 2 || stats.WeakCandidates != 1 || stats.ExcludedCandidates != 1 {
			t.Fatalf("fixture: strong=%d weak=%d excluded=%d, want 2, 1, 1", stats.StrongCandidates, stats.WeakCandidates, stats.ExcludedCandidates)
		}
	}
	loads := forbidPredictorLoad(t)

	code, out, errOut := invoke(context.Background(), "calibrate", "--split-at", "2026-06-01", "--json")
	if code != 1 || errOut != "" {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if r := decodeRefusal(t, out); !r.Refused || r.HaveN != 2 || r.NeedN != 299 {
		t.Errorf("refusal = %+v, want have_n 2 (the two strong commits only) and need_n 299", r)
	}
	code, out, _ = invoke(context.Background(), "calibrate", "--split-at", "2026-06-01")
	if want := "calibrate: refused: need n >= 299, have 2"; code != 1 || firstLine(out) != want {
		t.Errorf("code=%d first line %q, want %q", code, firstLine(out), want)
	}
	if *loads != 0 {
		t.Errorf("predictor loaded %d times", *loads)
	}
}

func writeTrainingJSON(t *testing.T, dir string, m predict.TrainingManifest) {
	t.Helper()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "training.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Units the predictor was trained on, units before its training cutoff and
// units at or after the split point are reported by reason and not counted.
func TestCalibrateExcludesTrainingEventsAndOutOfWindowUnits(t *testing.T) {
	t.Chdir(t.TempDir())
	day := func(d int) time.Time { return time.Date(2025, 7, d, 0, 0, 0, 0, time.UTC) }
	seedCorpus(t, func(c *corpus) {
		c.strong("p1", "c1", "go:p:T", day(1))  // before the training cutoff
		c.strong("p2", "c2", "go:p:T", day(5))  // eligible
		c.strong("p3", "c3", "go:p:T", day(6))  // used by training
		c.strong("p4", "c4", "go:p:T", day(20)) // after the split point
		c.strong("p5", "c5", "go:p:T", time.Time{})
	})
	writeTrainingJSON(t, ".hazard", predict.TrainingManifest{
		Schema: predict.TrainingSchema, Cutoff: "2025-07-02",
		Splits: map[string]predict.SplitReport{"fit": {EventIDs: []string{"ci:c3"}}, "scale": {EventIDs: []string{}}},
	})
	loads := forbidPredictorLoad(t)

	code, out, errOut := invoke(context.Background(), "calibrate", "--split-at", "2025-07-10", "--json")
	if code != 1 || errOut != "" {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	r := decodeRefusal(t, out)
	want := map[string]int{"before_training_cutoff": 1, "in_training": 1, "at_or_after_split_at": 1, "commit_time_unknown": 1}
	if r.HaveN != 1 || !reflect.DeepEqual(r.Excluded, want) {
		t.Errorf("have_n=%d excluded=%v, want have_n 1 excluded %v", r.HaveN, r.Excluded, want)
	}
	code, out, _ = invoke(context.Background(), "calibrate", "--split-at", "2025-07-10")
	if code != 1 || firstLine(out) != "calibrate: refused: need n >= 299, have 1" || !strings.Contains(out, "excluded_units") ||
		!strings.Contains(out, "in_training=1") {
		t.Errorf("code=%d output:\n%s", code, out)
	}
	if *loads != 0 {
		t.Errorf("predictor loaded %d times", *loads)
	}
}

func TestCalibrateRefusesACorruptTrainingRecord(t *testing.T) {
	t.Chdir(t.TempDir())
	seedCorpus(t, func(c *corpus) { c.strong("p1", "c1", "go:p:T", time.Date(2025, 7, 1, 0, 0, 0, 0, time.UTC)) })
	if err := os.WriteFile(filepath.Join(".hazard", "training.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := invoke(context.Background(), "calibrate", "--split-at", "2026-06-01")
	if code != 70 || !strings.Contains(errOut, "training.json") || strings.Contains(out, "need n") {
		t.Errorf("code=%d stdout=%q stderr=%q, want an internal error naming training.json and no count", code, out, errOut)
	}
}

// With no unit at all there is nothing to exclude, so an unreadable training
// record cannot change the answer: the refusal of the Definition of Done does
// not depend on it.
func TestCalibrateRefusalWithoutUnitsDoesNotNeedTheTrainingRecord(t *testing.T) {
	t.Chdir(t.TempDir())
	seedCorpus(t, func(*corpus) {})
	if err := os.WriteFile(filepath.Join(".hazard", "training.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := invoke(context.Background(), "calibrate", "--split-at", "2026-06-01")
	if code != 1 || errOut != "" || firstLine(out) != refusalLine {
		t.Errorf("code=%d stderr=%q first line %q, want exit 1 and %q", code, errOut, firstLine(out), refusalLine)
	}
}

func TestReadTraining(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		rec, err := readTraining(t.TempDir())
		if err != nil || rec.HasRecord || !rec.Cutoff.IsZero() || len(rec.EventIDs) != 0 {
			t.Errorf("readTraining of a directory without training.json = %+v, %v, want an empty record", rec, err)
		}
	})
	t.Run("valid", func(t *testing.T) {
		dir := t.TempDir()
		writeTrainingJSON(t, dir, predict.TrainingManifest{
			Schema: predict.TrainingSchema, Cutoff: "2026-03-01",
			Splits: map[string]predict.SplitReport{"fit": {EventIDs: []string{"ci:a", "ci:b"}}, "scale": {EventIDs: []string{"ci:c"}}},
		})
		rec, err := readTraining(dir)
		wantIDs := map[string]bool{"ci:a": true, "ci:b": true, "ci:c": true}
		if err != nil || !rec.HasRecord || !rec.Cutoff.Equal(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)) || !reflect.DeepEqual(rec.EventIDs, wantIDs) {
			t.Errorf("readTraining = %+v, %v, want cutoff 2026-03-01 and %v", rec, err, wantIDs)
		}
	})
	for name, content := range map[string]string{
		"malformed":   "{not json",
		"wrong shape": `[1,2]`,
		"schema":      `{"schema": 99, "cutoff": "2026-03-01", "splits": {}}`,
		"cutoff":      `{"schema": 1, "cutoff": "March", "splits": {}}`,
		"no cutoff":   `{"schema": 1, "splits": {}}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "training.json"), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := readTraining(dir); err == nil {
				t.Error("readTraining accepted a training record it cannot trust")
			}
		})
	}
}

func TestTrainingDirFollowsThePredictorArtifact(t *testing.T) {
	cfg := config.Defaults()
	cfg.Predictor.Static.Calibration = "models/p0/static.json"
	cfg.Predictor.Logistic.Weights = "models/p1/logistic.json"
	base := filepath.Join(t.TempDir(), "repo")
	for kind, want := range map[string]string{
		"static":   filepath.Join(base, "models", "p0"),
		"logistic": filepath.Join(base, "models", "p1"),
		"ensemble": filepath.Join(base, "models", "p1"),
		"jev":      "", // nothing was trained
	} {
		if got := trainingDir(base, cfg, kind); got != want {
			t.Errorf("trainingDir(%s) = %q, want %q", kind, got, want)
		}
	}
	abs := filepath.Join(t.TempDir(), "elsewhere", "logistic.json")
	cfg.Predictor.Logistic.Weights = abs
	if got, want := trainingDir(base, cfg, "logistic"), filepath.Dir(abs); got != want {
		t.Errorf("trainingDir with an absolute artifact path = %q, want %q", got, want)
	}
}

func unitAt(sha string, at time.Time) labels.Unit {
	return labels.Unit{ID: "ci:" + sha, SHA: sha, ParentSHA: "p-" + sha, CommitTime: at, Repository: "github/o/r", Failing: []string{"go:p:T"}}
}

func idsOf(units []labels.Unit) []string {
	ids := make([]string, 0, len(units))
	for _, u := range units {
		ids = append(ids, u.ID)
	}
	return ids
}

func TestSplitUnitsTemporal(t *testing.T) {
	at := func(m, d int) time.Time { return time.Date(2026, time.Month(m), d, 0, 0, 0, 0, time.UTC) }
	cutoff, splitAt := at(3, 1), at(6, 1)
	units := []labels.Unit{
		unitAt("before", cutoff.Add(-time.Second)),
		unitAt("atcutoff", cutoff),
		unitAt("inside", at(4, 15)),
		unitAt("lastsecond", splitAt.Add(-time.Second)),
		unitAt("atsplit", splitAt),
		unitAt("later", at(8, 1)),
		unitAt("trained", at(4, 20)),
		unitAt("trainedlate", at(9, 1)),
	}
	training := trainingRecord{HasRecord: true, Cutoff: cutoff, EventIDs: map[string]bool{"ci:trained": true, "ci:trainedlate": true}}

	t.Run("with a training record", func(t *testing.T) {
		got, excluded := splitUnits(units, splitSpec{Method: "temporal", SplitAt: splitAt, Training: training})
		if want := []string{"ci:atcutoff", "ci:inside", "ci:lastsecond"}; !reflect.DeepEqual(idsOf(got), want) {
			t.Errorf("eligible = %v, want %v: at or after the cutoff and strictly before split-at", idsOf(got), want)
		}
		wantExcluded := map[string]int{"before_training_cutoff": 1, "in_training": 2, "at_or_after_split_at": 2}
		if !reflect.DeepEqual(excluded, wantExcluded) {
			t.Errorf("excluded = %v, want %v (a trained unit is counted as trained, whatever its date)", excluded, wantExcluded)
		}
	})
	t.Run("without a training record there is no lower bound", func(t *testing.T) {
		got, excluded := splitUnits(units, splitSpec{Method: "temporal", SplitAt: splitAt})
		if want := []string{"ci:atcutoff", "ci:before", "ci:inside", "ci:lastsecond", "ci:trained"}; !reflect.DeepEqual(idsOf(got), want) {
			t.Errorf("eligible = %v, want %v", idsOf(got), want)
		}
		if want := map[string]int{"at_or_after_split_at": 3}; !reflect.DeepEqual(excluded, want) {
			t.Errorf("excluded = %v, want %v", excluded, want)
		}
	})
}

func TestSplitUnitsRandom(t *testing.T) {
	day := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	var units []labels.Unit
	for i := 0; i < 11; i++ {
		units = append(units, unitAt(fmt.Sprintf("u%02d", i), day))
	}
	training := trainingRecord{HasRecord: true, Cutoff: day.Add(24 * time.Hour), EventIDs: map[string]bool{"ci:u03": true}}

	spec := splitSpec{Method: "random", Seed: 1, Training: training}
	got, excluded := splitUnits(units, spec)
	// Ten units remain after training; the first half of the shuffle (floor) calibrates.
	if len(got) != 5 || excluded["in_training"] != 1 || excluded["random_holdout"] != 5 || len(excluded) != 2 {
		t.Fatalf("calibration %d excluded %v, want 5 units, in_training 1, random_holdout 5", len(got), excluded)
	}
	if slices.Contains(idsOf(got), "ci:u03") {
		t.Error("a unit the predictor was trained on was assigned to calibration")
	}
	if !slices.IsSorted(idsOf(got)) {
		t.Errorf("eligible units are not sorted: %v", idsOf(got))
	}

	again, _ := splitUnits(slices.Clone(units), spec)
	if !reflect.DeepEqual(idsOf(got), idsOf(again)) {
		t.Errorf("same seed, different assignment: %v vs %v", idsOf(got), idsOf(again))
	}
	reversed := slices.Clone(units)
	slices.Reverse(reversed)
	if shuffled, _ := splitUnits(reversed, spec); !reflect.DeepEqual(idsOf(got), idsOf(shuffled)) {
		t.Errorf("the assignment depends on the order of the input: %v vs %v", idsOf(got), idsOf(shuffled))
	}
	assignments := map[string]bool{}
	for seed := uint64(1); seed <= 10; seed++ {
		s, _ := splitUnits(units, splitSpec{Method: "random", Seed: seed, Training: training})
		assignments[strings.Join(idsOf(s), ",")] = true
	}
	if len(assignments) < 2 {
		t.Errorf("ten seeds gave %d distinct assignments, want the seed to matter", len(assignments))
	}
}

func TestSplitUnitsOfNothingIsNothing(t *testing.T) {
	for _, method := range []string{"temporal", "random"} {
		got, excluded := splitUnits(nil, splitSpec{Method: method, SplitAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)})
		if len(got) != 0 || len(excluded) != 0 {
			t.Errorf("%s: %v %v, want nothing", method, idsOf(got), excluded)
		}
	}
}

// The render functions are checked directly: the success path of the command
// is too slow to run once per output format.
func TestCalibrateAcceptedOutput(t *testing.T) {
	lock := control.Lock{
		Version: 1, CreatedAt: "2026-10-14T18:22:04Z", Alpha: 0.01, Delta: 0.05, Unit: "commit", Lambda: 0.035, Predictor: "static",
		ModelVersion: "p0", RubricVersion: "none", FeatureVersion: "f", NCal: 300, ReplayFailures: 2, EmpiricalMissRate: 0.0033, Split: "temporal", SplitAt: "2026-06-01",
		Grid: []float64{0.035}, PValues: map[string]float64{"0.035": 0.01}, CalibrationIDs: []string{"ci:a"},
	}
	out := calibrateAccepted{Out: "hazard.lock", Lock: lock, Excluded: map[string]int{"in_training": 2}}

	var text strings.Builder
	if err := writeCalibrateAccepted(&text, false, out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"calibrate: wrote hazard.lock", "lambda: 0.035", "n_cal: 300", "replay_failures: 2", "empirical_miss_rate: 0.0033", "in_training=2", "exchangeable", "temporal backtest"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("text output omits %q:\n%s", want, text.String())
		}
	}
	var js strings.Builder
	if err := writeCalibrateAccepted(&js, true, out); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Refused  bool           `json:"refused"`
		Out      string         `json:"out"`
		Lock     control.Lock   `json:"lock"`
		Excluded map[string]int `json:"excluded"`
		Note     string         `json:"note"`
	}
	if err := json.Unmarshal([]byte(js.String()), &got); err != nil {
		t.Fatalf("json output: %v\n%s", err, js.String())
	}
	if got.Refused || got.Out != "hazard.lock" || got.Lock.Lambda != 0.035 || got.Lock.NCal != 300 || got.Excluded["in_training"] != 2 || !strings.Contains(got.Note, "exchangeable") {
		t.Errorf("json output = %+v", got)
	}
}

// TestCalibrationReplayKeepsAUnitWhoseReplayFailsAsAMiss: a unit that cannot
// be replayed has an unknown outcome. Dropping it would take a would-be miss
// out of the sample, so it stays, marked failed, and the controller counts it as
// a miss at every lambda.
func TestCalibrationReplayKeepsAUnitWhoseReplayFailsAsAMiss(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	at := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	writeTrainFiles(t, dir, map[string]string{
		".gitignore": ".hazard/\n",
		"go.mod":     "this is not a go.mod\n", // the parent does not build an index
		"a/a.go":     "package a\n\nfunc A() int { return 1 }\n",
	})
	trainGit(t, dir, at, "init", "-q", "-b", "main")
	trainGit(t, dir, at, "add", "-A")
	trainGit(t, dir, at, "commit", "-q", "-m", "parent")
	parent := trainGit(t, dir, at, "rev-parse", "HEAD")
	writeTrainFiles(t, dir, map[string]string{"go.mod": "module example.com/m\n\ngo 1.21\n", "a/a.go": "package a\n\nfunc A() int { return 2 }\n"})
	trainGit(t, dir, at, "add", "-A")
	trainGit(t, dir, at, "commit", "-q", "-m", "child")
	child := trainGit(t, dir, at, "rev-parse", "HEAD")

	cfg := config.Defaults()
	cfg.Languages.Python.Enabled = false
	r := &calibrationReplay{repoDir: dir, cfg: cfg, lockPath: filepath.Join(t.TempDir(), "absent.lock")}

	for _, tc := range []struct{ name, parent string }{
		{"the parent cannot be checked out", strings.Repeat("0", 40)},
		{"the index cannot be built at the parent", parent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := labels.Unit{ID: "ci:" + child, SHA: child, ParentSHA: tc.parent, Failing: []string{"go:example.com/m/a:TestA"}}

			cu, skip, err := r.unit(context.Background(), u)

			if err != nil {
				t.Fatal(err)
			}
			if skip != "" {
				t.Fatalf("skip = %q: a unit whose replay failed must stay in the sample, not be dropped", skip)
			}
			if !cu.Failed || cu.ID != u.ID || !reflect.DeepEqual(cu.Failing, u.Failing) {
				t.Errorf("unit = %+v, want it kept as a failed unit with its ID and regressions", cu)
			}
			if got := control.UnitLoss(cu, 0.5); got != 1 {
				t.Errorf("UnitLoss = %v, want 1", got)
			}
		})
	}
}

// --- end to end -----------------------------------------------------------

// calibrationRepo is a Go module with three packages (b imports a, c stands
// alone, one test each) and a chain of commits that all change a/a.go. Every
// odd commit is blamed for TestB by the seeded labels; every even one is clean.
type calibrationRepo struct {
	dir    string
	chain  []string // chain[0] is the initial commit
	authAt time.Time
}

func newCalibrationRepo(t *testing.T, commits int) *calibrationRepo {
	t.Helper()
	r := &calibrationRepo{dir: filepath.Join(t.TempDir(), "repo"), authAt: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)}
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTrainFiles(t, r.dir, map[string]string{
		"hazard.yml":  trainConfig,
		".gitignore":  ".hazard/\n",
		"go.mod":      "module example.com/m\n\ngo 1.21\n",
		"a/a.go":      aSource(0),
		"a/a_test.go": "package a\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {\n\tif A() != 1 {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n",
		"b/b.go":      "package b\n\nimport \"example.com/m/a\"\n\nfunc B() int { return a.A() }\n",
		"b/b_test.go": "package b\n\nimport \"testing\"\n\nfunc TestB(t *testing.T) {\n\tif B() != 1 {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n",
		// A third, independent package keeps a change to a below the 40% of
		// the packages at which the controller abstains as too broad.
		"c/c.go":      "package c\n\nfunc C() int { return 1 }\n",
		"c/c_test.go": "package c\n\nimport \"testing\"\n\nfunc TestC(t *testing.T) {\n\tif C() != 1 {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n",
	})
	trainGit(t, r.dir, r.authAt, "init", "-q", "-b", "main")
	trainGit(t, r.dir, r.authAt, "add", "-A")
	trainGit(t, r.dir, r.authAt, "commit", "-q", "-m", "init")
	r.chain = append(r.chain, trainGit(t, r.dir, r.authAt, "rev-parse", "HEAD"))
	for i := 1; i <= commits; i++ {
		writeTrainFiles(t, r.dir, map[string]string{"a/a.go": aSource(i)})
		trainGit(t, r.dir, r.authAt, "add", "-A")
		trainGit(t, r.dir, r.authAt.Add(time.Duration(i)*time.Hour), "commit", "-q", "-m", fmt.Sprintf("change %d", i))
		r.chain = append(r.chain, trainGit(t, r.dir, r.authAt, "rev-parse", "HEAD"))
	}
	return r
}

// writeStatic writes a static artifact that gives every candidate probability
// p, so that a regression in TestB is selected exactly when lambda <= p.
func (r *calibrationRepo) writeStatic(t *testing.T, p float64) {
	t.Helper()
	a := predict.StaticArtifact{
		Schema: predict.ArtifactSchema, FeatureVersion: impact.FeatureVersion,
		Model: predict.IsotonicModel{X: []float64{0, 1}, Y: []float64{p, p}},
	}
	if err := os.MkdirAll(filepath.Join(r.dir, ".hazard"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := predict.WriteStatic(filepath.Join(r.dir, ".hazard", "static.json"), a); err != nil {
		t.Fatal(err)
	}
}

func TestCalibrateEndToEndAcceptance(t *testing.T) {
	repo := newCalibrationRepo(t, 6)
	t.Chdir(repo.dir)
	repo.writeStatic(t, 0.3)

	// c1, c3 and c5 are the regression-bearing commits; their parents c0, c2
	// and c4 are clean. alpha and delta are loose so that three commits are
	// enough: n >= MinCalibrationN(0.34, 0.34).
	const alpha, delta = 0.34, 0.34
	if need := control.MinCalibrationN(alpha, delta); need != 3 {
		t.Fatalf("fixture: MinCalibrationN(%v, %v) = %d, want 3", alpha, delta, need)
	}
	seedCorpus(t, func(c *corpus) {
		for _, i := range []int{1, 3, 5} {
			c.strong(repo.chain[i-1], repo.chain[i], trainTestB, repo.authAt.Add(time.Duration(i)*24*time.Hour))
		}
	})
	lockPath := filepath.Join(repo.dir, "hazard.lock")

	t.Run("too few commits refuses before replaying anything", func(t *testing.T) {
		loads := forbidPredictorLoad(t)
		code, out, errOut := invoke(context.Background(), "calibrate", "--predictor", "static", "--alpha", "0.01", "--delta", "0.05", "--split-at", "2026-09-01")
		if want := "calibrate: refused: need n >= 299, have 3"; code != 1 || errOut != "" || firstLine(out) != want {
			t.Errorf("code=%d stderr=%q first line %q, want exit 1 and %q", code, errOut, firstLine(out), want)
		}
		if _, err := os.Stat(lockPath); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a lock exists after a refusal: %v", err)
		}
		if *loads != 0 {
			t.Errorf("predictor loaded %d times", *loads)
		}
	})

	code, out, errOut := invoke(context.Background(), "calibrate", "--predictor", "static", "--alpha", "0.34", "--delta", "0.34",
		"--grid", "0.005:0.500:0.005", "--split-at", "2026-09-01")
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d stderr=%q stdout:\n%s", code, errOut, out)
	}
	for _, want := range []string{"calibrate: wrote hazard.lock", "lambda: 0.3\n", "n_cal: 3\n", "empirical_miss_rate: 0\n", "exchangeable", "temporal backtest"} {
		if !strings.Contains(out, want) {
			t.Errorf("output omits %q:\n%s", want, out)
		}
	}

	lock, err := control.ReadLock(lockPath)
	if err != nil {
		t.Fatalf("ReadLock: %v", err)
	}
	wantIDs := []string{"ci:" + repo.chain[1], "ci:" + repo.chain[3], "ci:" + repo.chain[5]}
	slices.Sort(wantIDs)
	checks := []struct {
		field     string
		got, want any
	}{
		{"Lambda", lock.Lambda, 0.3}, {"NCal", lock.NCal, 3}, {"Alpha", lock.Alpha, alpha}, {"Delta", lock.Delta, delta},
		{"EmpiricalMissRate", lock.EmpiricalMissRate, 0.0}, {"Unit", lock.Unit, "commit"}, {"Split", lock.Split, "temporal"},
		{"SplitAt", lock.SplitAt, "2026-09-01"}, {"Predictor", lock.Predictor, "static"}, {"ModelVersion", lock.ModelVersion, "p0"},
		{"RubricVersion", lock.RubricVersion, "none"}, {"CalibrationIDs", lock.CalibrationIDs, wantIDs},
		{"L1Mode", lock.L1Mode, "graph"},
	}
	for _, c := range checks {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("Lock.%s = %v, want %v", c.field, c.got, c.want)
		}
	}
	if len(lock.Grid) != 100 || lock.PValues[control.PValueKey(0.3)] == 0 || lock.PValues[control.PValueKey(0.305)] != 1 {
		t.Errorf("grid %d points, p-values at 0.3 and 0.305 = %v and %v, want 100 points and the walk stopping at 0.305",
			len(lock.Grid), lock.PValues[control.PValueKey(0.3)], lock.PValues[control.PValueKey(0.305)])
	}

	// The identity in the lock is the one the engine reports for the same
	// predictor and policy, so Verify accepts it and a selection uses it.
	cfg, err := loadIndexConfig(repo.dir, "")
	if err != nil {
		t.Fatal(err)
	}
	_, id, err := engine.LoadPredictor(repo.dir, cfg, "static")
	if err != nil {
		t.Fatal(err)
	}
	id.PolicyDigest = engine.BuildPolicy(cfg, "static", impact.ModeGraph).Digest()
	if reason := lock.Verify(id); reason != "" {
		t.Errorf("Verify(engine identity) = %q, want the lock accepted", reason)
	}
	m, err := index.Build(context.Background(), repo.dir, cfg, index.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := index.Publish(repo.dir, m); err != nil {
		t.Fatal(err)
	}
	d, err := engine.Select(context.Background(), engine.Options{RepoDir: repo.dir, Base: repo.chain[5], Head: repo.chain[6], LockPath: lockPath})
	if err != nil {
		t.Fatal(err)
	}
	if d.LockDigest != lock.Digest || d.Lambda != lock.Lambda {
		t.Errorf("engine.Select did not accept the lock: digest %q lambda %v abstain %q (%v)", d.LockDigest, d.Lambda, d.AbstainReason, d.Warnings)
	}

	s, err := store.Open(labelsDBPath)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	var lambda float64
	var split, predictor, model string
	if err := s.DB().QueryRow(`SELECT count(*), max(lambda), max(split), max(predictor), max(model_version) FROM calibrations`).Scan(&n, &lambda, &split, &predictor, &model); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil { // before the next replay, which copies the file
		t.Fatal(err)
	}
	if n != 1 || lambda != 0.3 || split != "temporal" || predictor != "static" || model != "p0" {
		t.Errorf("calibrations = %d rows lambda %v split %s predictor %s model %s, want one row of this lock", n, lambda, split, predictor, model)
	}

	t.Run("a predictor that never catches the regression certifies nothing and leaves the lock alone", func(t *testing.T) {
		before, err := os.ReadFile(lockPath)
		if err != nil {
			t.Fatal(err)
		}
		repo.writeStatic(t, 0) // probability 0 is below every lambda of the grid: every regression is missed
		code, out, errOut := invoke(context.Background(), "calibrate", "--predictor", "static", "--alpha", "0.34", "--delta", "0.34", "--split-at", "2026-09-01")
		if want := "calibrate: refused: need n >= 3, have 3"; code != 1 || errOut != "" || firstLine(out) != want {
			t.Fatalf("code=%d stderr=%q stdout:\n%s\nwant exit 1 and first line %q", code, errOut, out, want)
		}
		if !strings.Contains(out, "detail: no lambda of the grid is certified") {
			t.Errorf("the refusal does not say why a sample that is large enough was refused:\n%s", out)
		}
		if after, err := os.ReadFile(lockPath); err != nil || !slices.Equal(after, before) {
			t.Errorf("the existing lock changed on a refusal: %v", err)
		}
		if rows := rowCount(t, "calibrations"); rows != 1 {
			t.Errorf("calibrations rows = %d, want the 1 of the earlier lock", rows)
		}
	})
}

// A random split calibrates on the first half of what training left over.
func TestCalibrateRandomSplitCountsTheCalibrationHalf(t *testing.T) {
	t.Chdir(t.TempDir())
	at := time.Date(2025, 7, 1, 0, 0, 0, 0, time.UTC)
	seedCorpus(t, func(c *corpus) {
		for i := 0; i < 5; i++ {
			c.strong(fmt.Sprintf("p%d", i), fmt.Sprintf("c%d", i), "go:p:T", at.Add(time.Duration(i)*time.Hour))
		}
	})
	code, out, errOut := invoke(context.Background(), "calibrate", "--split", "random", "--seed", "7", "--json")
	if code != 1 || errOut != "" {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	r := decodeRefusal(t, out)
	if r.HaveN != 2 || !reflect.DeepEqual(r.Excluded, map[string]int{"random_holdout": 3}) {
		t.Errorf("have_n=%d excluded=%v, want the first half (2 of 5) and 3 held out", r.HaveN, r.Excluded)
	}
}

func TestHistoryDurationsAreTheMedianPerTest(t *testing.T) {
	t.Chdir(t.TempDir())
	s, err := store.Open(labelsDBPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := historyDurations(context.Background()); err != nil || len(got) != 0 {
		t.Errorf("an empty history gave %v, %v, want no durations", got, err)
	}
	rows := []struct {
		test, status string
		ms           any
	}{
		{"go:p:odd", "pass", 10}, {"go:p:odd", "pass", 30}, {"go:p:odd", "fail", 20},
		{"go:p:even", "pass", 10}, {"go:p:even", "pass", 21},
		{"go:p:skipped", "skip", 500},
		{"go:p:negative", "pass", -1},
		{"go:p:unmeasured", "pass", nil},
	}
	for i, r := range rows {
		mustExec(t, s.DB(), `INSERT INTO test_results(commit_sha,test_id,status,duration_ms,observed_at) VALUES(?,?,?,?,'2026-01-01T00:00:00Z')`,
			fmt.Sprintf("sha%d", i), r.test, r.status, r.ms)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := historyDurations(context.Background())
	// The median of an even count is the mean of the middle pair rounded up,
	// so a test never looks cheaper than it is; skips, negatives and missing
	// durations are not measurements.
	if want := map[string]int{"go:p:odd": 20, "go:p:even": 16}; err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("durations = %v, %v, want %v", got, err, want)
	}
}

func TestCalibrateWithoutARepositoryOrPredictorIsAnUsageError(t *testing.T) {
	const alpha, delta = "0.34", "0.34"
	at := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	seed := func(c *corpus) {
		for i := 0; i < 3; i++ {
			c.strong(fmt.Sprintf("p%d", i), fmt.Sprintf("c%d", i), "go:p:T", at.Add(time.Duration(i)*time.Hour))
		}
	}
	t.Run("not a git repository", func(t *testing.T) {
		dir := t.TempDir()
		t.Chdir(dir)
		seedCorpus(t, seed)
		code, out, errOut := invoke(context.Background(), "calibrate", "--alpha", alpha, "--delta", delta, "--split-at", "2026-09-01", "--predictor", "static")
		if code != 2 || !strings.Contains(errOut, "not inside a git repository") || strings.Contains(out, "refused") {
			t.Errorf("code=%d stdout=%q stderr=%q, want a usage error", code, out, errOut)
		}
		if _, err := os.Stat(filepath.Join(dir, "hazard.lock")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a lock was written: %v", err)
		}
	})
	t.Run("predictor artifact missing", func(t *testing.T) {
		repo := newCalibrationRepo(t, 1)
		t.Chdir(repo.dir)
		seedCorpus(t, seed)
		code, out, errOut := invoke(context.Background(), "calibrate", "--alpha", alpha, "--delta", delta, "--split-at", "2026-09-01", "--predictor", "static")
		if code != 2 || !strings.Contains(errOut, "static") || strings.Contains(out, "refused") {
			t.Errorf("code=%d stdout=%q stderr=%q, want a usage error naming the predictor", code, out, errOut)
		}
		if _, err := os.Stat(filepath.Join(repo.dir, "hazard.lock")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a lock was written: %v", err)
		}
	})
}
