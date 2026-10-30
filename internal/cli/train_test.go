package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/omorsi45/hazard/internal/config"
	"github.com/omorsi45/hazard/internal/events"
	"github.com/omorsi45/hazard/internal/index"
	"github.com/omorsi45/hazard/internal/mutate"
	"github.com/omorsi45/hazard/internal/predict"
	"github.com/omorsi45/hazard/internal/store"
)

const (
	trainConfig = "version: 1\nlanguages:\n  python:\n    enabled: false\nimpact:\n  max_hops: 0\n"
	trainTestA  = "go:example.com/m/a:TestA"
	trainTestB  = "go:example.com/m/b:TestB"
)

// trainBase is the committer time of the repository's head commit, exactly UTC
// midnight, so that --before on that date must exclude it.
var trainBase = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// trainFixture is a Go module with two packages (b imports a, one test each),
// a published index at its head commit and mutated-file candidates spread over
// the train, tune and test partitions. impact.max_hops is 0, so a change in a
// leaves TestB as the only residual candidate.
type trainFixture struct {
	dir        string
	base       string   // head commit: the mutation base the index describes
	ciCommit   string   // a commit that changes a/a.go, in the train partition
	ciParent   string   // its parent
	trainFiles []string // sorted
	tuneFile   string
	testFile   string
}

func trainGit(t *testing.T, dir string, at time.Time, args ...string) string {
	t.Helper()
	full := append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	stamp := at.UTC().Format("2006-01-02T15:04:05+0000")
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE="+stamp, "GIT_COMMITTER_DATE="+stamp,
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeTrainFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func trainSource(n int) string {
	return fmt.Sprintf("package a\n\nfunc M%03d(x int) bool { return x > 3 && x < 100 }\n", n)
}

func aSource(variant int) string {
	return fmt.Sprintf("package a\n\nfunc A() int { return 1 } // v%d\n", variant)
}

func newTrainFixture(t *testing.T) *trainFixture {
	t.Helper()
	f := &trainFixture{dir: filepath.Join(t.TempDir(), "repo")}
	files := map[string]string{
		"hazard.yml":  trainConfig,
		".gitignore":  ".hazard/\n",
		"README.md":   "readme\n",
		"go.mod":      "module example.com/m\n\ngo 1.21\n",
		"a/a.go":      aSource(0),
		"a/a_test.go": "package a\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {\n\tif A() != 1 {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n",
		"b/b.go":      "package b\n\nimport \"example.com/m/a\"\n\nfunc B() int { return a.A() }\n",
		"b/b_test.go": "package b\n\nimport \"testing\"\n\nfunc TestB(t *testing.T) {\n\tif B() != 1 {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n",
	}
	// Pick file names by the partition of their group: five in train, one each in tune and test.
	for n := 0; len(f.trainFiles) < 5 || f.tuneFile == "" || f.testFile == ""; n++ {
		name := fmt.Sprintf("a/m%03d.go", n)
		switch events.Partition(events.MutationGroup(name)) {
		case events.PartitionTrain:
			if len(f.trainFiles) == 5 {
				continue
			}
			f.trainFiles = append(f.trainFiles, name)
		case events.PartitionTune:
			if f.tuneFile != "" {
				continue
			}
			f.tuneFile = name
		default:
			if f.testFile != "" {
				continue
			}
			f.testFile = name
		}
		files[name] = trainSource(n)
	}
	sort.Strings(f.trainFiles)
	if err := os.MkdirAll(f.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTrainFiles(t, f.dir, files)
	trainGit(t, f.dir, trainBase, "init", "-q", "-b", "main")
	trainGit(t, f.dir, trainBase, "add", "-A")
	trainGit(t, f.dir, trainBase.Add(-48*time.Hour), "commit", "-q", "-m", "init")
	f.ciParent = trainGit(t, f.dir, trainBase, "rev-parse", "HEAD")

	// A commit in the train partition changing package a, one day before the base.
	for v := 1; ; v++ {
		if v > 200 {
			t.Fatal("no commit variant landed in the train partition")
		}
		writeTrainFiles(t, f.dir, map[string]string{"a/a.go": aSource(v)})
		trainGit(t, f.dir, trainBase, "add", "-A")
		trainGit(t, f.dir, trainBase.Add(-24*time.Hour), "commit", "-q", "-m", "change a")
		sha := trainGit(t, f.dir, trainBase, "rev-parse", "HEAD")
		if events.Partition("ci:"+sha) == events.PartitionTrain {
			f.ciCommit = sha
			break
		}
		trainGit(t, f.dir, trainBase, "reset", "-q", "--hard", f.ciParent)
	}

	// The head commit (the mutation base) is outside the train partition, so a
	// label on it must be counted and never replayed.
	for v := 1; ; v++ {
		if v > 200 {
			t.Fatal("no head commit variant landed outside the train partition")
		}
		writeTrainFiles(t, f.dir, map[string]string{"README.md": fmt.Sprintf("readme, edit %d\n", v)})
		trainGit(t, f.dir, trainBase, "add", "-A")
		trainGit(t, f.dir, trainBase, "commit", "-q", "-m", "docs")
		sha := trainGit(t, f.dir, trainBase, "rev-parse", "HEAD")
		if events.Partition("ci:"+sha) != events.PartitionTrain {
			f.base = sha
			break
		}
		trainGit(t, f.dir, trainBase, "reset", "-q", "--hard", f.ciCommit)
	}

	cfg, err := config.Load(filepath.Join(f.dir, "hazard.yml"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := index.Build(context.Background(), f.dir, cfg, index.Options{})
	if err != nil {
		t.Fatalf("index.Build: %v", err)
	}
	if m.HeadSHA != f.base || !m.Complete || len(m.Tests) != 2 {
		t.Fatalf("fixture index = head %s complete %v tests %d, want %s, complete, 2 tests", m.HeadSHA, m.Complete, len(m.Tests), f.base)
	}
	if err := index.Publish(f.dir, m); err != nil {
		t.Fatal(err)
	}
	return f
}

// mutantRow is a row of the mutants table and its kills.
type mutantRow struct {
	m       mutate.Mutant
	base    string
	status  string
	killers []string
}

// fileMutants returns the first n mutants of op in file as generated from the
// fixture's source of that file.
func (f *trainFixture) fileMutants(t *testing.T, file, op string, n int) []mutate.Mutant {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(f.dir, filepath.FromSlash(file)))
	if err != nil {
		t.Fatal(err)
	}
	ms, err := mutate.GoMutants(file, src, []string{op})
	if err != nil || len(ms) < n {
		t.Fatalf("GoMutants(%s, %s) = %d mutants, %v, want at least %d", file, op, len(ms), err, n)
	}
	return ms[:n]
}

// standardRows is the mutation corpus: per file one killed mutant (killed by
// TestA, which L1 selects anyway, and TestB, the residual candidate) and one
// survivor, plus a build error and a timeout in the first train file.
func (f *trainFixture) standardRows(t *testing.T) []mutantRow {
	t.Helper()
	var rows []mutantRow
	for _, file := range append(append([]string{}, f.trainFiles...), f.tuneFile, f.testFile) {
		ms := f.fileMutants(t, file, "cmp", 2)
		rows = append(rows,
			mutantRow{m: ms[0], base: f.base, status: "killed", killers: []string{trainTestA, trainTestB}},
			mutantRow{m: ms[1], base: f.base, status: "survived"})
	}
	other := f.fileMutants(t, f.trainFiles[0], "const", 2)
	return append(rows,
		mutantRow{m: other[0], base: f.base, status: "build_error"},
		mutantRow{m: other[1], base: f.base, status: "timeout"})
}

// labelSpec is one label candidate of the current build.
type labelSpec struct{ sha, state string }

// seed replaces the repository's database with the given mutants and labels
// on TestB.
func (f *trainFixture) seed(t *testing.T, rows []mutantRow, labels ...labelSpec) {
	t.Helper()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(filepath.Join(f.dir, ".hazard", "hazard.db"+suffix)); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	st, err := store.Open(filepath.Join(f.dir, ".hazard", "hazard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()
	db := st.DB()
	run := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	for _, r := range rows {
		run(`INSERT INTO mutants(id,file,line,operator,original,mutated,base_sha,status,created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
			r.m.ID, r.m.File, r.m.Line, r.m.Operator, r.m.Original, r.m.Mutated, r.base, r.status, "2026-01-01T00:00:00Z")
		for _, k := range r.killers {
			run(`INSERT INTO mutant_kills(mutant_id,test_id) VALUES(?,?)`, r.m.ID, k)
		}
	}
	for _, l := range labels {
		insertLabel(t, db, l.sha, trainTestB, l.state, true)
	}
}

// insertLabel adds one label candidate to the current build, creating the
// build when there is none.
func insertLabel(t *testing.T, db *sql.DB, sha, testID, state string, current bool) {
	t.Helper()
	build := strings.Repeat("b", 64)
	if _, err := db.Exec(`INSERT OR IGNORE INTO label_builds(build_id,provider,project,cutoff,algorithm_version,input_digest,decision_digest,always_run_json,acquisition_exclusions_json,active_gaps,created_at,is_current)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, build, "github", "o/r", "2026-01-01T00:00:00Z", "labels/1", strings.Repeat("c", 64), strings.Repeat("d", 64), "[]", "{}", 0, "2026-01-01T00:00:00Z", boolInt(current)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO label_candidates(build_id,candidate_key,kind,state,tested_sha,test_id,environment_id,branch,member_count,evidence_json)
		VALUES(?,?,?,?,?,?,?,?,?,?)`, build, sha+"/"+testID, "candidate", state, sha, testID, "linux", "main", 1, "{}"); err != nil {
		t.Fatal(err)
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// runTrain runs `hazard train` in the fixture and returns the exit code, the
// standard output and the standard error.
func (f *trainFixture) runTrain(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	t.Chdir(f.dir)
	return invoke(context.Background(), append([]string{"train"}, args...)...)
}

func (f *trainFixture) readManifest(t *testing.T, stdout string) predict.TrainingManifest {
	t.Helper()
	var m predict.TrainingManifest
	if err := json.Unmarshal([]byte(stdout), &m); err != nil {
		t.Fatalf("stdout is not a training manifest: %v\n%s", err, stdout)
	}
	return m
}

func countOf(m predict.TrainingManifest, origin, partition, status string) int {
	for _, c := range m.Collected.Events {
		if c.Origin == origin && c.Partition == partition && c.Status == status {
			return c.Count
		}
	}
	return 0
}

func (f *trainFixture) artifactFiles(t *testing.T) []string {
	t.Helper()
	var names []string
	for _, name := range []string{"static.json", "logistic.json", "training.json"} {
		if _, err := os.Stat(filepath.Join(f.dir, ".hazard", name)); err == nil {
			names = append(names, name)
		}
	}
	return names
}

func removeArtifacts(t *testing.T, dir string) {
	t.Helper()
	for _, name := range []string{"static.json", "logistic.json", "training.json"} {
		if err := os.Remove(filepath.Join(dir, ".hazard", name)); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
}

func TestTrainEndToEnd(t *testing.T) {
	f := newTrainFixture(t)

	t.Run("trains on the train partition only", func(t *testing.T) {
		f.seed(t, f.standardRows(t))
		code, out, errOut := f.runTrain(t, "--before", "2026-01-02", "--json")
		if code != 0 || errOut != "" {
			t.Fatalf("code=%d err=%q out=%s", code, errOut, out)
		}
		m := f.readManifest(t, out)
		if got := f.artifactFiles(t); !reflect.DeepEqual(got, []string{"static.json", "logistic.json", "training.json"}) {
			t.Fatalf("artifacts = %v, want the three files", got)
		}
		static, err := predict.ReadStatic(filepath.Join(f.dir, ".hazard", "static.json"))
		if err != nil {
			t.Fatalf("ReadStatic: %v", err)
		}
		logistic, err := predict.ReadLogistic(filepath.Join(f.dir, ".hazard", "logistic.json"))
		if err != nil {
			t.Fatalf("ReadLogistic: %v", err)
		}
		if m.Artifacts["static"].Digest != static.Digest || m.Artifacts["logistic"].Digest != logistic.Digest {
			t.Errorf("manifest digests %+v do not match the artifacts %s %s", m.Artifacts, static.Digest, logistic.Digest)
		}
		onDisk, err := os.ReadFile(filepath.Join(f.dir, ".hazard", "training.json"))
		if err != nil {
			t.Fatal(err)
		}
		var fromFile predict.TrainingManifest
		if err := json.Unmarshal(onDisk, &fromFile); err != nil {
			t.Fatalf("training.json: %v", err)
		}
		if !reflect.DeepEqual(fromFile, m) {
			t.Errorf("training.json differs from the printed manifest:\nfile  %+v\nprint %+v", fromFile, m)
		}

		// What was observed, by origin, partition and status.
		for key, want := range map[[3]string]int{
			{"mutation", "train", "killed"}: 5, {"mutation", "train", "survived"}: 5,
			{"mutation", "train", "build_error"}: 1, {"mutation", "train", "timeout"}: 1,
			{"mutation", "tune", "killed"}: 1, {"mutation", "tune", "survived"}: 1,
			{"mutation", "test", "killed"}: 1, {"mutation", "test", "survived"}: 1,
		} {
			if got := countOf(m, key[0], key[1], key[2]); got != want {
				t.Errorf("observed %v = %d, want %d", key, got, want)
			}
		}
		if want := map[string]int{"mutation:build_error": 1, "mutation:timeout": 1}; !reflect.DeepEqual(m.Collected.Skipped, want) {
			t.Errorf("skipped = %v, want %v", m.Collected.Skipped, want)
		}
		if m.Collected.FailingCertain != 5 || m.Collected.FailingUnlisted != 0 {
			t.Errorf("failing tests that are not rows = certain %d unlisted %d, want 5 and 0", m.Collected.FailingCertain, m.Collected.FailingUnlisted)
		}
		if want := (predict.ExampleCounts{Events: 10, Examples: 10, Positives: 5, Negatives: 5}); m.Examples != want {
			t.Errorf("examples = %+v, want %+v", m.Examples, want)
		}
		if fit, scale := m.Splits["fit"], m.Splits["scale"]; len(fit.Groups) != 4 || len(scale.Groups) != 1 || fit.Events != 8 || scale.Events != 2 ||
			scale.Groups[0] != events.MutationGroup(f.trainFiles[4]) {
			t.Errorf("splits fit %+v scale %+v, want the first four files fitting and the last scaling", fit, scale)
		}

		// Tune and test events are evaluation data: never in any split, any artifact or any count of rows.
		inSplit := map[string]bool{}
		for _, s := range m.Splits {
			for _, id := range s.EventIDs {
				inSplit[id] = true
			}
			for _, g := range s.Groups {
				if g == events.MutationGroup(f.tuneFile) || g == events.MutationGroup(f.testFile) {
					t.Errorf("split lists the evaluation group %s", g)
				}
			}
		}
		for _, file := range []string{f.tuneFile, f.testFile} {
			for _, r := range f.standardRowsOf(t, file) {
				if inSplit[r.m.ID] {
					t.Errorf("mutant %s of %s (not train partition) is in a split", r.m.ID, file)
				}
			}
		}
		for _, file := range f.trainFiles {
			for _, r := range f.standardRowsOf(t, file) {
				if !inSplit[r.m.ID] {
					t.Errorf("mutant %s of %s (train partition) is in no split", r.m.ID, file)
				}
			}
		}
		for _, g := range append(append([]string{}, logistic.Training.FitGroups...), logistic.Training.ScaleGroups...) {
			if g == events.MutationGroup(f.tuneFile) || g == events.MutationGroup(f.testFile) {
				t.Errorf("logistic artifact was fitted on the evaluation group %s", g)
			}
		}
		if logistic.Training.Positives != 5 || logistic.Training.Negatives != 5 || logistic.Training.Cutoff != "2026-01-02" || !logistic.Model.Converged {
			t.Errorf("logistic training info = %+v converged %v", logistic.Training, logistic.Model.Converged)
		}
		if static.Training.Sources["mutation"] != 8 {
			t.Errorf("static sources = %v, want 8 mutation events in the fit set", static.Training.Sources)
		}
	})

	t.Run("a CI label becomes a weighted row", func(t *testing.T) {
		f.seed(t, f.standardRows(t), labelSpec{f.ciCommit, "strong"}, labelSpec{f.base, "weak"})
		removeArtifacts(t, f.dir)
		code, out, errOut := f.runTrain(t, "--before", "2026-01-02", "--json")
		if code != 0 || errOut != "" {
			t.Fatalf("code=%d err=%q out=%s", code, errOut, out)
		}
		m := f.readManifest(t, out)
		if got := countOf(m, "ci_strong", "train", "strong"); got != 1 {
			t.Errorf("observed ci_strong/train/strong = %d, want 1", got)
		}
		// The weak label sits on a commit outside the train partition: counted, never replayed.
		if got := countOf(m, "ci_weak", events.Partition("ci:"+f.base), "weak"); got != 1 {
			t.Errorf("observed ci_weak/%s/weak = %d, want 1", events.Partition("ci:"+f.base), got)
		}
		for _, s := range m.Splits {
			for _, id := range s.EventIDs {
				if id == "ci:"+f.base {
					t.Errorf("the label on %s (not train partition) is in a split", f.base)
				}
			}
		}
		if want := (predict.ExampleCounts{Events: 11, Examples: 11, Positives: 6, Negatives: 5}); m.Examples != want {
			t.Errorf("examples = %+v, want %+v (ten mutation rows and the CI row); skipped %v", m.Examples, want, m.Collected.Skipped)
		}
		ciGroup := "ci:" + f.ciCommit
		if fit := m.Splits["fit"]; len(fit.Groups) != 4 || fit.Groups[0] != ciGroup {
			t.Errorf("fit groups = %v, want the CI commit (a day older than the mutation base) first", fit.Groups)
		}
		logistic, err := predict.ReadLogistic(filepath.Join(f.dir, ".hazard", "logistic.json"))
		if err != nil {
			t.Fatal(err)
		}
		if logistic.Training.Sources["ci_strong"] != 1 || logistic.Training.Options.CIWeight != 10 {
			t.Errorf("logistic training info = %+v, want one ci_strong event and CI weight 10", logistic.Training)
		}
	})

	t.Run("the cutoff is exclusive at UTC midnight", func(t *testing.T) {
		f.seed(t, f.standardRows(t))
		removeArtifacts(t, f.dir)
		// The base commit is at exactly 2026-01-01T00:00:00Z.
		code, out, _ := f.runTrain(t, "--before", "2026-01-01", "--json")
		if code != 1 {
			t.Fatalf("code=%d, want 1 (nothing left to train on)\n%s", code, out)
		}
		m := f.readManifest(t, out)
		if got := m.Collected.Skipped["mutation:after_cutoff"]; got != 10 {
			t.Errorf("events excluded by the cutoff = %d, want all 10 at exactly midnight", got)
		}
		if m.Examples.Events != 0 || len(f.artifactFiles(t)) != 0 {
			t.Errorf("examples %+v, artifacts %v, want none", m.Examples, f.artifactFiles(t))
		}
		code, out, _ = f.runTrain(t, "--before", "2026-01-02", "--json")
		if code != 0 {
			t.Fatalf("the next day: code=%d\n%s", code, out)
		}
		if m := f.readManifest(t, out); m.Examples.Events != 10 || m.Collected.Skipped["mutation:after_cutoff"] != 0 {
			t.Errorf("the next day: examples %+v skipped %v, want all 10 events in", m.Examples, m.Collected.Skipped)
		}
	})

	t.Run("a scale set without positives refuses P1 and keeps P0", func(t *testing.T) {
		rows := f.standardRows(t)
		last := events.MutationGroup(f.trainFiles[4])
		for i := range rows {
			if events.MutationGroup(rows[i].m.File) == last {
				rows[i].status, rows[i].killers = "survived", nil
			}
		}
		f.seed(t, rows)
		removeArtifacts(t, f.dir)
		code, out, errOut := f.runTrain(t, "--before", "2026-01-02")
		if code != 1 {
			t.Fatalf("code=%d err=%q, want 1\n%s", code, errOut, out)
		}
		if got := f.artifactFiles(t); !reflect.DeepEqual(got, []string{"static.json", "training.json"}) {
			t.Errorf("artifacts = %v, want static.json and training.json only", got)
		}
		if _, err := predict.ReadStatic(filepath.Join(f.dir, ".hazard", "static.json")); err != nil {
			t.Errorf("the P0 artifact is not valid: %v", err)
		}
		// Both outcomes are printed, with every count.
		for _, want := range []string{
			"static: wrote", "logistic: refused: scale set has no positive examples",
			"observed: mutation/train/killed=4", "skipped: mutation:build_error=1", "examples: events=10 examples=10 positives=4 negatives=6",
			"split fit:", "split scale:", "cutoff: 2026-01-02",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("output lacks %q:\n%s", want, out)
			}
		}
		if !strings.Contains(errOut, "refused") {
			t.Errorf("stderr = %q, want the refusal as the error", errOut)
		}
	})

	t.Run("a corpus without positives writes nothing", func(t *testing.T) {
		rows := f.standardRows(t)
		for i := range rows {
			if rows[i].status == "killed" {
				rows[i].status, rows[i].killers = "survived", nil
			}
		}
		f.seed(t, rows)
		removeArtifacts(t, f.dir)
		code, out, _ := f.runTrain(t, "--before", "2026-01-02", "--json")
		if code != 1 {
			t.Fatalf("code=%d, want 1\n%s", code, out)
		}
		if got := f.artifactFiles(t); len(got) != 0 {
			t.Errorf("artifacts = %v, want none", got)
		}
		m := f.readManifest(t, out)
		if len(m.Refusals) != 2 || len(m.Artifacts) != 0 {
			t.Errorf("refusals %+v artifacts %+v, want both fits refused", m.Refusals, m.Artifacts)
		}
	})

	t.Run("an index that does not describe the mutation base is refused", func(t *testing.T) {
		rows := f.standardRows(t)
		for i := range rows {
			rows[i].base = f.ciParent // an older commit: the files are the same, the index is not for it
		}
		f.seed(t, rows)
		removeArtifacts(t, f.dir)
		code, out, errOut := f.runTrain(t, "--before", "2026-01-02")
		if code != 1 {
			t.Fatalf("code=%d, want 1\n%s", code, out)
		}
		for _, want := range []string{f.base, f.ciParent, "hazard index"} {
			if !strings.Contains(out+errOut, want) {
				t.Errorf("refusal lacks %q:\nstdout %s\nstderr %s", want, out, errOut)
			}
		}
		if got := f.artifactFiles(t); len(got) != 0 {
			t.Errorf("artifacts = %v, want none", got)
		}
	})

	t.Run("a repository without a database is refused", func(t *testing.T) {
		f.seed(t, f.standardRows(t))
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if err := os.Remove(filepath.Join(f.dir, ".hazard", "hazard.db"+suffix)); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
		}
		removeArtifacts(t, f.dir)
		code, out, errOut := f.runTrain(t, "--before", "2026-01-02")
		if code != 1 || !strings.Contains(out, "there is no database") || !strings.Contains(errOut, "refused") {
			t.Errorf("code=%d, want 1 and a message about the database\nstdout %s\nstderr %s", code, out, errOut)
		}
		if _, err := os.Stat(filepath.Join(f.dir, ".hazard", "hazard.db")); !os.IsNotExist(err) {
			t.Errorf("train created a database (%v)", err)
		}
	})

	t.Run("verbose progress goes to stderr and the report to stdout", func(t *testing.T) {
		f.seed(t, f.standardRows(t))
		removeArtifacts(t, f.dir)
		code, out, errOut := f.runTrain(t, "--before", "2026-01-02", "-v")
		if code != 0 {
			t.Fatalf("code=%d err=%q\n%s", code, errOut, out)
		}
		if want := "replaying 10 mutation events on " + f.base; !strings.Contains(errOut, want) {
			t.Errorf("stderr lacks %q:\n%s", want, errOut)
		}
		for _, want := range []string{"cutoff: 2026-01-02", "static: wrote", "logistic: wrote", "training: "} {
			if !strings.Contains(out, want) {
				t.Errorf("stdout lacks %q:\n%s", want, out)
			}
		}
		if strings.Contains(out, "replaying") {
			t.Errorf("progress leaked into stdout:\n%s", out)
		}
	})

	t.Run("a missing index is refused", func(t *testing.T) {
		f.seed(t, f.standardRows(t))
		manifest := filepath.Join(f.dir, ".hazard", "index", "manifest.json")
		moved := manifest + ".moved"
		if err := os.Rename(manifest, moved); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Rename(moved, manifest) })
		code, out, errOut := f.runTrain(t, "--before", "2026-01-02")
		if code != 1 || !strings.Contains(out+errOut, "hazard index") {
			t.Errorf("code=%d, want 1 and a pointer to hazard index\nstdout %s\nstderr %s", code, out, errOut)
		}
	})
}

// standardRowsOf returns the standard rows of one file.
func (f *trainFixture) standardRowsOf(t *testing.T, file string) []mutantRow {
	t.Helper()
	var out []mutantRow
	for _, r := range f.standardRows(t) {
		if r.m.File == file && (r.status == "killed" || r.status == "survived") {
			out = append(out, r)
		}
	}
	return out
}

func TestTrainUsageErrors(t *testing.T) {
	dir := t.TempDir()
	trainGit(t, dir, trainBase, "init", "-q", "-b", "main")
	writeTrainFiles(t, dir, map[string]string{"bad.yml": "version: 2\n"})
	t.Chdir(dir)
	cases := map[string][]string{
		"no cutoff":           {"train"},
		"cutoff not a date":   {"train", "--before", "yesterday"},
		"impossible date":     {"train", "--before", "2026-13-01"},
		"datetime":            {"train", "--before", "2026-01-01T00:00:00Z"},
		"positional argument": {"train", "--before", "2026-01-01", "extra"},
		"unknown flag":        {"train", "--before", "2026-01-01", "--nope"},
		"invalid config":      {"train", "--before", "2026-01-01", "--config", "bad.yml"},
		"missing config":      {"train", "--before", "2026-01-01", "--config", "nowhere.yml"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			code, out, errOut := invoke(context.Background(), args...)
			if code != 2 || out != "" || errOut == "" {
				t.Errorf("code=%d stdout=%q stderr=%q, want exit 2, an error and no output", code, out, errOut)
			}
		})
	}
	t.Run("outside a repository", func(t *testing.T) {
		t.Chdir(t.TempDir())
		if code, _, _ := invoke(context.Background(), "train", "--before", "2026-01-01"); code != 2 {
			t.Errorf("code=%d, want 2", code)
		}
	})
}

func TestTrainHelpNamesTheSideEffects(t *testing.T) {
	code, out, errOut := invoke(context.Background(), "train", "--help")
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
	for _, want := range []string{"--before", "--out", "--config", "static.json", "logistic.json", "training.json", "exclusive"} {
		if !strings.Contains(out, want) {
			t.Errorf("help lacks %q:\n%s", want, out)
		}
	}
}
