package labels

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/omorsi45/hazard/internal/store"
)

const testObservationFormat = "2006-01-02T15:04:05.000000000Z"

var (
	baseTime  = time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	cutoff    = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	repoKey   = "github/o/r"
	testAlpha = "go:p:TestAlpha"
	testBeta  = "go:p:TestBeta"
)

// spec describes one artifact holding one case. Zero values select the
// verified, complete, comparable defaults; the No* flags remove one fact.
type spec struct {
	Run, Job, Env, SHA, Test, Status, Branch, Message, Infra, State, RunState string
	Attempt                                                                   int
	Parents                                                                   string // JSON text; empty means unknown (NULL)
	At                                                                        time.Time
	Ordinal                                                                   int
	NoEnv, NoEnvEvidence, NoSHA, NoTestID, NoExec, Unstable, Suite, Trunc     bool
}

type fixture struct {
	t        *testing.T
	store    *store.Store
	db       *sql.DB
	provider string
	project  string
	n        int
	units    []string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "hazard.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	f := &fixture{t: t, store: s, db: s.DB(), provider: "github", project: "o/r"}
	f.exec(`INSERT INTO mining_sources(singleton,provider,project,observed_at) VALUES(1,?,?,?)`, f.provider, f.project, baseTime.Format(testObservationFormat))
	return f
}

func (f *fixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Exec(query, args...); err != nil {
		f.t.Fatalf("%s: %v", query, err)
	}
}

func ps(shas ...string) string {
	out := "["
	for i, sha := range shas {
		if i > 0 {
			out += ","
		}
		out += strconv.Quote(sha)
	}
	return out + "]"
}

func textOrNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// obs inserts one artifact with one case and returns its raw unit key.
func (f *fixture) obs(s spec) string {
	f.t.Helper()
	f.n++
	if s.Run == "" {
		s.Run = "run-" + s.SHA
	}
	if s.Job == "" {
		s.Job = "job-" + strconv.Itoa(f.n)
	}
	if s.Env == "" {
		s.Env = "linux"
	}
	if s.Test == "" {
		s.Test = testAlpha
	}
	if s.Status == "" {
		s.Status = "pass"
	}
	if s.Branch == "" {
		s.Branch = "main"
	}
	if s.State == "" {
		s.State = "complete"
	}
	if s.RunState == "" {
		s.RunState = "complete"
	}
	if s.Attempt == 0 {
		s.Attempt = 1
	}
	if s.At.IsZero() {
		s.At = baseTime.Add(time.Duration(f.n) * time.Hour)
	}
	unit := fmt.Sprintf("unit-%d", f.n)
	sum := sha256.Sum256([]byte(unit))
	content := hex.EncodeToString(sum[:])
	var job, execEvidence, env, envEvidence, sha, shaEvidence, parents any
	if !s.NoExec {
		job, execEvidence = s.Job, "exec-proof"
	}
	if !s.NoEnv {
		env = s.Env
		if !s.NoEnvEvidence {
			envEvidence = "env-proof"
		}
	}
	if !s.NoSHA {
		sha, shaEvidence = s.SHA, "rev-proof"
	}
	if s.Parents != "" {
		parents = s.Parents
	}
	at := s.At.UTC().Format(testObservationFormat)
	f.exec(`INSERT INTO ci_artifacts(provider,project,unit_key,content_sha256,run_id,artifact_id,job_id,attempt,environment_id,tested_sha,
		tested_revision_evidence,execution_evidence,environment_evidence,raw_blob_path,source_head_sha,branch,tested_parent_shas_json,tested_kinds_json,
		suite_incomplete,diagnostics_truncated,state,observed_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		f.provider, f.project, unit, content, s.Run, "artifact-"+unit, job, s.Attempt, env, sha, shaEvidence, execEvidence, envEvidence,
		"raw/"+unit, s.SHA, s.Branch, parents, "null", s.Suite, s.Trunc, s.State, at)
	testID, identityError := s.Test, ""
	if s.NoTestID {
		testID, identityError = "", "unsupported identity"
	}
	f.exec(`INSERT INTO ci_observations(provider,project,unit_key,content_sha256,artifact_entry,entry_sha256,case_ordinal,raw_suite,raw_name,raw_classname,raw_file,
		status,raw_message,raw_type,raw_properties_json,suite_incomplete,diagnostics_truncated,test_id,identity_language,identity_package,identity_name,identity_file,
		identity_runner_id,unstable_id,identity_reason,identity_error,infra_reason,observed_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		f.provider, f.project, unit, content, "report.xml", content, s.Ordinal, "suite", "name", "class", "file.go",
		s.Status, s.Message, "", "{}", s.Suite, s.Trunc, testID, "go", "p", "TestAlpha", "file.go", "", s.Unstable, "", identityError, s.Infra, at)
	f.units = append(f.units, unit)
	attemptKey := strconv.Itoa(s.Attempt)
	f.exec(`INSERT INTO mining_runs(provider,project,run_id,attempt_key,attempt,source_head_sha,branch,state,observed_at) VALUES(?,?,?,?,?,?,?,?,?)
		ON CONFLICT DO NOTHING`, f.provider, f.project, s.Run, attemptKey, s.Attempt, s.SHA, s.Branch, s.RunState, baseTime.Format(testObservationFormat))
	return unit
}

// run inserts a mining_runs row without any artifact, as an acquired but
// unfinished attempt at the given head revision.
func (f *fixture) run(run, headSHA, state string) {
	f.t.Helper()
	f.exec(`INSERT INTO mining_runs(provider,project,run_id,attempt_key,attempt,source_head_sha,branch,state,observed_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		f.provider, f.project, run, "1", 1, headSHA, "main", state, baseTime.Format(testObservationFormat))
}
func (f *fixture) setRunState(run, state string) {
	f.t.Helper()
	f.exec(`UPDATE mining_runs SET state=? WHERE run_id=?`, state, run)
}

func (f *fixture) gap(run, unit, reason string, active bool) {
	f.t.Helper()
	resolved := any(nil)
	if !active {
		resolved = baseTime.Format(testObservationFormat)
	}
	f.exec(`INSERT INTO acquisition_gaps(provider,project,run_id,unit_key,reason,first_acquired_at,last_acquired_at,resolved_at) VALUES(?,?,?,?,?,?,?,?)`,
		f.provider, f.project, run, unit, reason, baseTime.Format(testObservationFormat), baseTime.Format(testObservationFormat), resolved)
}

func (f *fixture) exclusion(unit, reason string) {
	f.t.Helper()
	var content string
	if err := f.db.QueryRow(`SELECT content_sha256 FROM ci_artifacts WHERE unit_key=?`, unit).Scan(&content); err != nil {
		f.t.Fatal(err)
	}
	f.exec(`INSERT INTO acquisition_exclusions(provider,project,unit_key,content_sha256,artifact_entry,case_ordinal,reason,detail,observed_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		f.provider, f.project, unit, content, "report.xml", 0, reason, "detail", baseTime.Format(testObservationFormat))
}

func (f *fixture) flag(test, column string) {
	f.t.Helper()
	f.exec(`INSERT INTO tests(id,lang,package,name,file,first_seen,last_seen) VALUES(?,?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING`,
		test, "go", "p", "T", "t.go", "2025-01-01T00:00:00Z", "2025-01-01T00:00:00Z")
	f.exec(`UPDATE tests SET `+column+`=1 WHERE id=?`, test)
}

// standard inserts parent pass p1, failing candidate c1 (parent p1) and passing
// descendant d1 (parent c1). Mutators run before insertion.
func (f *fixture) standard(mods ...func(parent, fail, desc *spec)) (parent, fail, desc string) {
	f.t.Helper()
	p := spec{SHA: "p1", Parents: ps("p0"), Status: "pass"}
	c := spec{SHA: "c1", Parents: ps("p1"), Status: "fail", Message: "assertion failed"}
	d := spec{SHA: "d1", Parents: ps("c1"), Status: "pass"}
	for _, mod := range mods {
		mod(&p, &c, &d)
	}
	if p.SHA != "" {
		parent = f.obs(p)
	}
	fail = f.obs(c)
	if d.SHA != "" {
		desc = f.obs(d)
	}
	return parent, fail, desc
}

func (f *fixture) build() Report {
	f.t.Helper()
	report, err := Build(context.Background(), f.db, Options{Before: cutoff})
	if err != nil {
		f.t.Fatalf("Build: %v", err)
	}
	return report
}

type candidateRow struct {
	State   string
	Kind    string
	Reasons map[string]string // reason -> effect
}

// candidate reads the frozen decision for the group (sha, test, env).
func (f *fixture) candidate(buildID, sha, test, env string) candidateRow {
	f.t.Helper()
	var key string
	row := candidateRow{Reasons: map[string]string{}}
	err := f.db.QueryRow(`SELECT candidate_key,state,kind FROM label_candidates WHERE build_id=? AND tested_sha=? AND test_id=? AND environment_id=?`,
		buildID, sha, test, env).Scan(&key, &row.State, &row.Kind)
	if err != nil {
		f.t.Fatalf("candidate %s/%s/%s: %v", sha, test, env, err)
	}
	f.reasonsInto(buildID, key, row.Reasons)
	return row
}

func (f *fixture) reasonsInto(buildID, key string, into map[string]string) {
	f.t.Helper()
	rows, err := f.db.Query(`SELECT reason,effect FROM label_candidate_exclusions WHERE build_id=? AND candidate_key=?`, buildID, key)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			f.t.Fatal(err)
		}
	}()
	for rows.Next() {
		var reason, effect string
		if err := rows.Scan(&reason, &effect); err != nil {
			f.t.Fatal(err)
		}
		into[reason] = effect
	}
	if err := rows.Err(); err != nil {
		f.t.Fatal(err)
	}
}

// unresolved returns the frozen unresolved records of a build, keyed by raw unit.
func (f *fixture) unresolved(buildID string) map[string]candidateRow {
	f.t.Helper()
	type pair struct{ unit, key string }
	rows, err := f.db.Query(`SELECT m.unit_key,c.candidate_key,c.state FROM label_candidates c
		JOIN label_candidate_members m ON m.build_id=c.build_id AND m.candidate_key=c.candidate_key WHERE c.build_id=? AND c.kind='unresolved'`, buildID)
	if err != nil {
		f.t.Fatal(err)
	}
	var pairs []pair
	out := map[string]candidateRow{}
	states := map[string]string{}
	for rows.Next() {
		var p pair
		var state string
		if err := rows.Scan(&p.unit, &p.key, &state); err != nil {
			f.t.Fatal(err)
		}
		pairs = append(pairs, p)
		states[p.unit] = state
	}
	if err := rows.Err(); err != nil {
		f.t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		f.t.Fatal(err)
	}
	for _, p := range pairs {
		row := candidateRow{State: states[p.unit], Kind: "unresolved", Reasons: map[string]string{}}
		f.reasonsInto(buildID, p.key, row.Reasons)
		out[p.unit] = row
	}
	return out
}

func (f *fixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.db.QueryRow(query, args...).Scan(&n); err != nil {
		f.t.Fatalf("%s: %v", query, err)
	}
	return n
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
