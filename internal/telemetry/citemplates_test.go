package telemetry

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// The CI templates under docs/ci are documentation people copy into their
// pipelines, so their load-bearing lines are tested: a shallow clone has no
// merge base and Hazard would then run the full suite on every pull request.

type ghStep struct {
	Name string         `yaml:"name"`
	Uses string         `yaml:"uses"`
	Run  string         `yaml:"run"`
	If   string         `yaml:"if"`
	With map[string]any `yaml:"with"`
}

type ghJob struct {
	If    string   `yaml:"if"`
	Steps []ghStep `yaml:"steps"`
}

type ghWorkflow struct {
	On   map[string]any   `yaml:"on"`
	Jobs map[string]ghJob `yaml:"jobs"`
}

func readTemplate(t *testing.T, name string, into any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "ci", name))
	if err != nil {
		t.Fatalf("the CI template must exist: %v", err)
	}
	if err := yaml.Unmarshal(data, into); err != nil {
		t.Fatalf("%s is not valid YAML: %v", name, err)
	}
}

// stepIndex is the index of the first step matching pred, or -1.
func stepIndex(steps []ghStep, pred func(ghStep) bool) int {
	for i, s := range steps {
		if pred(s) {
			return i
		}
	}
	return -1
}

func usesAction(action string) func(ghStep) bool {
	return func(s ghStep) bool { return strings.HasPrefix(s.Uses, action+"@") }
}

func runsCommand(substr string) func(ghStep) bool {
	return func(s ghStep) bool { return strings.Contains(s.Run, substr) }
}

func TestGitHubActionsTemplate(t *testing.T) {
	var wf ghWorkflow
	readTemplate(t, "github-actions.yml", &wf)
	for _, trigger := range []string{"pull_request", "push", "schedule"} {
		if _, ok := wf.On[trigger]; !ok {
			t.Errorf("the workflow must trigger on %s: %v", trigger, wf.On)
		}
	}

	// Every job that runs hazard needs the full history.
	for name, job := range wf.Jobs {
		i := stepIndex(job.Steps, usesAction("actions/checkout"))
		if i < 0 {
			t.Errorf("job %s has no checkout step", name)
			continue
		}
		if depth, ok := job.Steps[i].With["fetch-depth"]; !ok || fmt.Sprint(depth) != "0" {
			t.Errorf("job %s: checkout must set fetch-depth: 0 (a shallow clone has no merge base), got %v", name, job.Steps[i].With)
		}
		if stepIndex(job.Steps, usesAction("actions/setup-go")) < 0 {
			t.Errorf("job %s has no setup-go step", name)
		}
		if stepIndex(job.Steps, runsCommand("go install github.com/omorsi45/hazard/cmd/hazard@")) < 0 {
			t.Errorf("job %s does not install hazard", name)
		}
		c := stepIndex(job.Steps, usesAction("actions/cache"))
		if c < 0 {
			t.Errorf("job %s does not cache .hazard", name)
			continue
		}
		with := job.Steps[c].With
		if with["path"] != ".hazard" || !strings.Contains(fmt.Sprint(with["key"]), "hashFiles('**/go.sum')") {
			t.Errorf("job %s: the cache must hold .hazard keyed on go.sum, got %v", name, with)
		}
	}

	sel, ok := wf.Jobs["selected"]
	if !ok {
		t.Fatal("missing the selected job")
	}
	if !strings.Contains(sel.If, "pull_request") {
		t.Errorf("selected runs on pull requests, got if %q", sel.If)
	}
	run := stepIndex(sel.Steps, runsCommand("hazard run"))
	if run < 0 || !strings.Contains(sel.Steps[run].Run, "--junit .hazard/junit.xml") || strings.Contains(sel.Steps[run].Run, "--full") {
		t.Fatalf("selected must run `hazard run ... --junit .hazard/junit.xml` without --full: %+v", sel.Steps)
	}
	up := stepIndex(sel.Steps, usesAction("actions/upload-artifact"))
	if up < 0 || sel.Steps[up].If != "always()" || sel.Steps[up].With["path"] != ".hazard/junit.xml" {
		t.Errorf("selected must upload the JUnit report even when tests fail: %+v", sel.Steps)
	}

	full, ok := wf.Jobs["full"]
	if !ok {
		t.Fatal("missing the full job")
	}
	if !strings.Contains(full.If, "schedule") || !strings.Contains(full.If, "refs/heads/main") {
		t.Errorf("full runs on the schedule and on the default branch, got if %q", full.If)
	}
	fullRun := stepIndex(full.Steps, runsCommand("hazard run --full --junit .hazard/junit.xml"))
	audit := stepIndex(full.Steps, runsCommand("hazard audit --full-run .hazard/junit.xml --since-run"))
	if fullRun < 0 || audit < 0 || audit <= fullRun {
		t.Fatalf("full must run the full suite, then audit it (run at %d, audit at %d)", fullRun, audit)
	}
	if full.Steps[audit].If != "always()" {
		t.Errorf("the audit must run even when the full suite fails, that is when it matters: if %q", full.Steps[audit].If)
	}
}

type glJob struct {
	Extends   string           `yaml:"extends"`
	Variables map[string]any   `yaml:"variables"`
	Rules     []map[string]any `yaml:"rules"`
	Script    []string         `yaml:"script"`
	Artifacts glArtifacts      `yaml:"artifacts"`
	Before    []string         `yaml:"before_script"`
	Cache     map[string]any   `yaml:"cache"`
}

type glArtifacts struct {
	When    string            `yaml:"when"`
	Reports map[string]string `yaml:"reports"`
}

func TestGitLabCITemplate(t *testing.T) {
	var raw map[string]yaml.Node
	readTemplate(t, "gitlab-ci.yml", &raw)
	jobs := map[string]glJob{}
	for name, node := range raw {
		if name == "stages" {
			continue
		}
		var job glJob
		if err := node.Decode(&job); err != nil {
			t.Fatalf("job %s: %v", name, err)
		}
		jobs[name] = job
	}
	base, ok := jobs[".hazard-base"]
	if !ok {
		t.Fatal("missing the .hazard-base job")
	}
	paths := fmt.Sprint(base.Cache["paths"])
	if !strings.Contains(paths, ".hazard/") || !strings.Contains(fmt.Sprint(base.Cache["key"]), "CI_COMMIT_REF_SLUG") {
		t.Errorf("the cache must hold .hazard/ per ref, got %v", base.Cache)
	}
	if !strings.Contains(strings.Join(base.Before, "\n"), "go install github.com/omorsi45/hazard/cmd/hazard@") {
		t.Errorf("the base must install hazard: %v", base.Before)
	}

	for _, name := range []string{"selected-tests", "full-suite"} {
		job, ok := jobs[name]
		if !ok {
			t.Fatalf("missing the %s job", name)
		}
		if job.Extends != ".hazard-base" {
			t.Errorf("%s must extend .hazard-base, got %q", name, job.Extends)
		}
		if depth, ok := job.Variables["GIT_DEPTH"]; !ok || fmt.Sprint(depth) != "0" {
			t.Errorf("%s: GIT_DEPTH must be 0 (a shallow clone has no merge base), got %v", name, job.Variables)
		}
		if job.Artifacts.When != "always" || job.Artifacts.Reports["junit"] != ".hazard/junit.xml" {
			t.Errorf("%s must publish the JUnit report even when tests fail: %+v", name, job.Artifacts)
		}
	}

	sel := jobs["selected-tests"]
	if !strings.Contains(fmt.Sprint(sel.Rules), "merge_request_event") {
		t.Errorf("selected-tests runs on merge requests, got rules %v", sel.Rules)
	}
	if !strings.Contains(strings.Join(sel.Script, "\n"), `hazard run --base "$CI_MERGE_REQUEST_DIFF_BASE_SHA" --junit .hazard/junit.xml`) {
		t.Errorf("selected-tests script = %v", sel.Script)
	}

	full := jobs["full-suite"]
	rules := fmt.Sprint(full.Rules)
	if !strings.Contains(rules, "schedule") || !strings.Contains(rules, "CI_DEFAULT_BRANCH") {
		t.Errorf("full-suite runs on schedules and on the default branch, got rules %v", full.Rules)
	}
	runAt, auditAt := -1, -1
	for i, line := range full.Script {
		switch {
		case strings.Contains(line, "hazard run --full --junit .hazard/junit.xml"):
			runAt = i
		case strings.Contains(line, "hazard audit --full-run .hazard/junit.xml --since-run"):
			auditAt = i
		}
	}
	if runAt < 0 || auditAt <= runAt {
		t.Errorf("full-suite must run the full suite, then audit it: %v", full.Script)
	}
	if !strings.Contains(strings.Join(full.Script, "\n"), "full_status") {
		t.Errorf("full-suite must audit even when the full suite fails and still fail the job: %v", full.Script)
	}
}
