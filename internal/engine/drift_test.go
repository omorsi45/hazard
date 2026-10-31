package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/omorsi45/hazard/internal/config"
	"github.com/omorsi45/hazard/internal/control"
)

const lockDigest = "sha256:lock-under-test"

// auditLine is one line of audits.jsonl, in the field order telemetry writes.
func auditLine(id, digest string, missed []string, bearing bool) string {
	list := func(s []string) string {
		quoted := make([]string, len(s))
		for i, v := range s {
			quoted[i] = fmt.Sprintf("%q", v)
		}
		return "[" + strings.Join(quoted, ",") + "]"
	}
	failing := missed
	if bearing && len(failing) == 0 {
		failing = []string{"go:m/p:TestOther"}
	}
	return fmt.Sprintf(`{"selection_id":%q,"lock_digest":%q,"commit":"c0","full_run_id":"r0","failing":%s,"missed":%s,"regression_bearing":%v}`,
		id, digest, list(failing), list(missed), bearing)
}

func writeAudits(t *testing.T, repo string, content string) string {
	t.Helper()
	path := auditsPath(repo)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAuditsPathIsUnderTheStateDirectory(t *testing.T) {
	t.Parallel()
	repo := filepath.Join(t.TempDir(), "r")
	if got, want := auditsPath(repo), filepath.Join(repo, ".hazard", "audits.jsonl"); got != want {
		t.Errorf("auditsPath = %q, want %q", got, want)
	}
}

func TestReadObservations(t *testing.T) {
	t.Parallel()
	miss := []string{"go:m/p:TestA"}
	for _, tc := range []struct {
		name     string
		content  string
		want     []control.Observation
		warnings int
		wantErr  string
	}{
		{"an empty file has no observations", "", nil, 0, ""},
		{"a regression-bearing miss and a regression-bearing hit",
			auditLine("s1", "d1", miss, true) + "\n" + auditLine("s2", "d1", nil, true) + "\n",
			[]control.Observation{{SelectionID: "s1", LockDigest: "d1", Missed: true}, {SelectionID: "s2", LockDigest: "d1"}}, 0, ""},
		{"a selection with nothing failing is not an observation",
			auditLine("s1", "d1", nil, false) + "\n" + auditLine("s2", "d1", miss, true) + "\n",
			[]control.Observation{{SelectionID: "s2", LockDigest: "d1", Missed: true}}, 0, ""},
		{"a repeated selection keeps the last line, at the position of the last",
			auditLine("s1", "d1", miss, true) + "\n" + auditLine("s2", "d1", nil, true) + "\n" + auditLine("s1", "d1", nil, true) + "\n",
			[]control.Observation{{SelectionID: "s2", LockDigest: "d1"}, {SelectionID: "s1", LockDigest: "d1"}}, 0, ""},
		{"a repeated selection whose last line is not regression-bearing is dropped",
			auditLine("s1", "d1", miss, true) + "\n" + auditLine("s1", "d1", nil, false) + "\n",
			nil, 0, ""},
		{"blank lines and unknown fields are fine",
			"\n" + `{"selection_id":"s1","lock_digest":"d1","regression_bearing":true,"missed":["x"],"extra":1}` + "\n\n",
			[]control.Observation{{SelectionID: "s1", LockDigest: "d1", Missed: true}}, 0, ""},
		{"a last line without a newline that is complete counts",
			auditLine("s1", "d1", miss, true),
			[]control.Observation{{SelectionID: "s1", LockDigest: "d1", Missed: true}}, 0, ""},
		{"a truncated last line is skipped with a warning",
			auditLine("s1", "d1", miss, true) + "\n" + `{"selection_id":"s2","lock_dig`,
			[]control.Observation{{SelectionID: "s1", LockDigest: "d1", Missed: true}}, 1, ""},
		{"a malformed line in the middle is an error",
			auditLine("s1", "d1", miss, true) + "\n{not json\n" + auditLine("s2", "d1", nil, true) + "\n",
			nil, 0, "line 2"},
		{"a malformed last line that ended in a newline is an error, not a truncation",
			auditLine("s1", "d1", miss, true) + "\n{not json\n",
			nil, 0, "line 2"},
		{"a line that is not an object is an error", "[1]\n", nil, 0, "line 1"},
		{"a line without a selection_id is an error", `{"lock_digest":"d1","regression_bearing":true}` + "\n", nil, 0, "line 1"},
		{"a field of the wrong type is an error", `{"selection_id":"s1","regression_bearing":"yes"}` + "\n", nil, 0, "line 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := writeAudits(t, t.TempDir(), tc.content)

			got, warnings, err := readObservations(path)

			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one naming %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("observations\n got: %+v\nwant: %+v", got, tc.want)
			}
			if len(warnings) != tc.warnings {
				t.Errorf("warnings = %q, want %d", warnings, tc.warnings)
			}
		})
	}
}

func TestReadObservationsOfAMissingFileIsNone(t *testing.T) {
	t.Parallel()
	got, warnings, err := readObservations(filepath.Join(t.TempDir(), "nope", "audits.jsonl"))
	if err != nil || len(got) != 0 || len(warnings) != 0 {
		t.Errorf("got %+v, %q, %v; want no observations, no warnings, no error", got, warnings, err)
	}
}

func TestReadObservationsOfADirectoryIsAnError(t *testing.T) {
	t.Parallel()
	if _, _, err := readObservations(t.TempDir()); err == nil {
		t.Error("a directory in place of the audit log must be an error, not an empty history")
	}
}

func lockForDrift() control.Lock {
	return control.Lock{Digest: lockDigest, Alpha: 0.01, Delta: 0.05}
}

func TestDriftForAnAuditHistory(t *testing.T) {
	t.Parallel()
	miss := []string{"go:m/p:TestA"}
	escalating := config.Defaults()
	quiet := config.Defaults()
	quiet.Control.Drift.EscalateIfUCBAboveAlpha = false
	short := config.Defaults()
	short.Control.Drift.WindowRuns = 1

	type want struct {
		state   string
		alarm   bool
		n, miss int
		warning string // substring of a warning, "" for none
	}
	for _, tc := range []struct {
		name    string
		cfg     config.Config
		content string // "" writes nothing
		noFile  bool
		want    want
	}{
		{"no audit log is warming", escalating, "", true, want{state: "warming"}},
		{"an empty audit log is warming", escalating, "", false, want{state: "warming"}},
		{"a confirmed miss under this lock alarms at once", escalating,
			auditLine("s1", lockDigest, miss, true) + "\n", false, want{"alarm", true, 1, 1, "drift: alarm"}},
		{"a miss under another lock is not evidence against this one", escalating,
			auditLine("s1", "sha256:other", miss, true) + "\n", false, want{state: "warming"}},
		{"a hit under this lock is still warming", escalating,
			auditLine("s1", lockDigest, nil, true) + "\n", false, want{state: "warming", n: 1}},
		{"a miss that a re-audit withdrew is not a miss", escalating,
			auditLine("s1", lockDigest, miss, true) + "\n" + auditLine("s1", lockDigest, nil, true) + "\n", false, want{state: "warming", n: 1}},
		{"an alarm that the configuration does not escalate is a warning only", quiet,
			auditLine("s1", lockDigest, miss, true) + "\n", false, want{"alarm", false, 1, 1, "not escalated"}},
		{"only the last window_runs observations count", short,
			auditLine("s1", lockDigest, miss, true) + "\n" + auditLine("s2", lockDigest, nil, true) + "\n", false, want{state: "warming", n: 1}},
		{"an unreadable history is an unknown drift that alarms", escalating,
			auditLine("s1", lockDigest, nil, true) + "\n{broken\n" + auditLine("s2", lockDigest, nil, true) + "\n", false, want{"alarm", true, 0, 0, "drift_unknown: "}},
		{"an unreadable history alarms even when drift is not escalated", quiet,
			"{broken\n" + auditLine("s2", lockDigest, nil, true) + "\n", false, want{"alarm", true, 0, 0, "drift_unknown: "}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo := t.TempDir()
			if !tc.noFile {
				writeAudits(t, repo, tc.content)
			}

			got, warnings := driftFor(repo, tc.cfg, lockForDrift())

			if got.State != tc.want.state || got.Alarm != tc.want.alarm || got.N != tc.want.n || got.Misses != tc.want.miss {
				t.Errorf("status = %+v, want state %q alarm %v n %d misses %d", got, tc.want.state, tc.want.alarm, tc.want.n, tc.want.miss)
			}
			joined := strings.Join(warnings, "\n")
			if tc.want.warning == "" && joined != "" {
				t.Errorf("warnings = %q, want none", warnings)
			}
			if tc.want.warning != "" && !strings.Contains(joined, tc.want.warning) {
				t.Errorf("warnings = %q, want one containing %q", warnings, tc.want.warning)
			}
		})
	}
}

func TestDriftForSkipsATruncatedLastLineWithAWarning(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	writeAudits(t, repo, auditLine("s1", lockDigest, nil, true)+"\n"+`{"selection_id":"s2","lock_`)

	got, warnings := driftFor(repo, config.Defaults(), lockForDrift())

	if got.Alarm || got.N != 1 {
		t.Errorf("status = %+v, want the complete line counted and no alarm", got)
	}
	if !strings.Contains(strings.Join(warnings, "\n"), "truncated") {
		t.Errorf("warnings = %q, want one naming the truncated line", warnings)
	}
}
