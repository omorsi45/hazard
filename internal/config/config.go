package config

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"reflect"
	"strings"

	"go.yaml.in/yaml/v3"
)

type Config struct {
	Version   int       `yaml:"version"`
	Repo      Repo      `yaml:"repo"`
	Languages Languages `yaml:"languages"`
	Impact    Impact    `yaml:"impact"`
	Predictor Predictor `yaml:"predictor"`
	Control   Control   `yaml:"control"`
	Audit     Audit     `yaml:"audit"`
	Privacy   Privacy   `yaml:"privacy"`
	Telemetry Telemetry `yaml:"telemetry"`
}
type Repo struct {
	DefaultBranch     string   `yaml:"default_branch"`
	FullSuiteTriggers []string `yaml:"full_suite_triggers"`
	GeneratedGlobs    []string `yaml:"generated_globs"`
}
type Languages struct {
	Go     GoLanguage     `yaml:"go"`
	Python PythonLanguage `yaml:"python"`
}
type GoLanguage struct {
	Enabled      bool     `yaml:"enabled"`
	Packages     []string `yaml:"packages"`
	Coverprofile string   `yaml:"coverprofile"`
	Tags         []string `yaml:"tags"`
}
type PythonLanguage struct {
	Enabled    bool     `yaml:"enabled"`
	Testpaths  []string `yaml:"testpaths"`
	CoverageDB string   `yaml:"coverage_db"`
	PytestArgs []string `yaml:"pytest_args"`
}
type Impact struct {
	MaxHops               int      `yaml:"max_hops"`
	CoverageRequired      bool     `yaml:"coverage_required"`
	CoverageMaxAgeCommits int      `yaml:"coverage_max_age_commits"`
	MaxCandidates         int      `yaml:"max_candidates"`
	CandidateRanking      []string `yaml:"candidate_ranking"`
}
type Predictor struct {
	Kind     string   `yaml:"kind"`
	Jev      Jev      `yaml:"jev"`
	Logistic Logistic `yaml:"logistic"`
}
type Jev struct {
	Endpoint            string  `yaml:"endpoint"`
	Model               string  `yaml:"model"`
	APIKeyEnv           string  `yaml:"api_key_env"`
	QuestionsPerRequest int     `yaml:"questions_per_request"`
	MaxStateTokens      int     `yaml:"max_state_tokens"`
	MaxRequestTokens    int     `yaml:"max_request_tokens"`
	TimeoutMS           int     `yaml:"timeout_ms"`
	Retries             int     `yaml:"retries"`
	RetryBackoffMS      int     `yaml:"retry_backoff_ms"`
	Concurrency         int     `yaml:"concurrency"`
	BudgetUSDPerRun     float64 `yaml:"budget_usd_per_run"`
}
type Logistic struct {
	Weights string `yaml:"weights"`
}
type Control struct {
	Alpha              float64 `yaml:"alpha"`
	Delta              float64 `yaml:"delta"`
	Unit               string  `yaml:"unit"`
	RequireLock        bool    `yaml:"require_lock"`
	LockMismatchAction string  `yaml:"lock_mismatch_action"`
	MinSavingRatio     float64 `yaml:"min_saving_ratio"`
	Drift              Drift   `yaml:"drift"`
}
type Drift struct {
	WindowRuns              int  `yaml:"window_runs"`
	EscalateIfUCBAboveAlpha bool `yaml:"escalate_if_ucb_above_alpha"`
}
type Audit struct {
	FullSuiteSchedule         string `yaml:"full_suite_schedule"`
	FullSuiteOnMergeToDefault bool   `yaml:"full_suite_on_merge_to_default"`
	MaxDetectionLatencyHours  int    `yaml:"max_detection_latency_hours"`
}
type Privacy struct {
	Redact                 string `yaml:"redact"`
	SendHunkText           bool   `yaml:"send_hunk_text"`
	SendSymbolNames        bool   `yaml:"send_symbol_names"`
	SendPaths              bool   `yaml:"send_paths"`
	AllowExternalPredictor bool   `yaml:"allow_external_predictor"`
}
type Telemetry struct {
	Path    string `yaml:"path"`
	Enabled bool   `yaml:"enabled"`
}

func Defaults() Config {
	return Config{
		Version: 1,
		Repo: Repo{
			DefaultBranch:     "main",
			FullSuiteTriggers: []string{"go.mod", "go.sum", "pyproject.toml", "uv.lock", "Dockerfile*", ".gitlab-ci.yml", ".github/workflows/**", "Makefile", "**/conftest.py", "**/testdata/**"},
			GeneratedGlobs:    []string{"**/*.pb.go", "**/generated.py", "**/*_gen.go"},
		},
		Languages: Languages{
			Go:     GoLanguage{Enabled: true, Packages: []string{"./..."}, Coverprofile: ".hazard/cover.out", Tags: []string{}},
			Python: PythonLanguage{Enabled: true, Testpaths: []string{"tests"}, CoverageDB: ".coverage", PytestArgs: []string{"-q"}},
		},
		Impact: Impact{MaxHops: 3, CoverageMaxAgeCommits: 200, MaxCandidates: 400, CandidateRanking: []string{"coverage_overlap", "hops", "cofailure", "recency"}},
		Predictor: Predictor{
			Kind:     "ensemble",
			Jev:      Jev{Endpoint: "https://openrouter.ai/api/alpha/decisions", Model: "typesafe/jev-1.13", APIKeyEnv: "OPENROUTER_API_KEY", QuestionsPerRequest: 200, MaxStateTokens: 12000, MaxRequestTokens: 28000, TimeoutMS: 3000, Retries: 2, RetryBackoffMS: 200, Concurrency: 4, BudgetUSDPerRun: 0.01},
			Logistic: Logistic{Weights: ".hazard/logistic.json"},
		},
		Control:   Control{Alpha: 0.01, Delta: 0.05, Unit: "commit", RequireLock: true, LockMismatchAction: "full", MinSavingRatio: 0.15, Drift: Drift{WindowRuns: 500, EscalateIfUCBAboveAlpha: true}},
		Audit:     Audit{FullSuiteSchedule: "0 3 * * *", FullSuiteOnMergeToDefault: true, MaxDetectionLatencyHours: 24},
		Privacy:   Privacy{Redact: "structure", SendSymbolNames: true, SendPaths: true, AllowExternalPredictor: true},
		Telemetry: Telemetry{Path: ".hazard/runs.jsonl", Enabled: true},
	}
}

func Load(path string) (Config, error) {
	if path == "" {
		for _, candidate := range []string{"hazard.yml", ".hazard/hazard.yml"} {
			_, err := os.Stat(candidate)
			if err == nil {
				path = candidate
				break
			}
			if !os.IsNotExist(err) {
				return Config{}, fmt.Errorf("stat %s: %w", candidate, err)
			}
		}
		if path == "" {
			return Defaults(), nil
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("open config %s: %w", path, err)
	}
	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		return Config{}, fmt.Errorf("decode config %s: %w", path, err)
	}
	if err := rejectAliasesAndMerges(&node); err != nil {
		return Config{}, fmt.Errorf("decode config %s: %w", path, err)
	}
	if err := checkIntegerScalars(&node, reflect.TypeFor[Config]()); err != nil {
		return Config{}, fmt.Errorf("decode config %s: %w", path, err)
	}
	c := Defaults()
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("decode config %s: %w", path, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple YAML documents")
		}
		return Config{}, fmt.Errorf("decode config %s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return Config{}, fmt.Errorf("validate config %s: %w", path, err)
	}
	return c, nil
}

func rejectAliasesAndMerges(node *yaml.Node) error {
	if node.Kind == yaml.AliasNode {
		return fmt.Errorf("YAML aliases are unsupported")
	}
	if node.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(node.Content); i += 2 {
			if node.Content[i].Tag == "!!merge" || node.Content[i].Value == "<<" {
				return fmt.Errorf("YAML merge keys are unsupported")
			}
		}
	}
	for _, child := range node.Content {
		if err := rejectAliasesAndMerges(child); err != nil {
			return err
		}
	}
	return nil
}
func checkIntegerScalars(node *yaml.Node, target reflect.Type) error {
	if node.Kind == yaml.DocumentNode {
		if len(node.Content) == 0 {
			return nil
		}
		return checkIntegerScalars(node.Content[0], target)
	}
	if target.Kind() == reflect.Int {
		if node.Kind != yaml.ScalarNode || node.Tag != "!!int" {
			return fmt.Errorf("expected integer, got %q", node.Value)
		}
		return nil
	}
	if target.Kind() != reflect.Struct || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		for j := 0; j < target.NumField(); j++ {
			field := target.Field(j)
			if field.Tag.Get("yaml") == node.Content[i].Value {
				if err := checkIntegerScalars(node.Content[i+1], field.Type); err != nil {
					return fmt.Errorf("%s: %w", field.Tag.Get("yaml"), err)
				}
				break
			}
		}
	}
	return nil
}
func (c Config) Validate() error {
	invalid := func(field string) error { return fmt.Errorf("invalid %s", field) }
	positive := func(n int) bool { return n > 0 }
	finite := func(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
	probability := func(v float64) bool { return finite(v) && v > 0 && v < 1 }
	oneOf := func(v string, options ...string) bool {
		for _, option := range options {
			if v == option {
				return true
			}
		}
		return false
	}
	if c.Version != 1 {
		return invalid("version")
	}
	if strings.TrimSpace(c.Repo.DefaultBranch) == "" {
		return invalid("repo.default_branch")
	}
	if !c.Languages.Go.Enabled && !c.Languages.Python.Enabled {
		return invalid("languages")
	}
	if c.Languages.Go.Enabled && (len(c.Languages.Go.Packages) == 0 || c.Languages.Go.Coverprofile == "") {
		return invalid("languages.go")
	}
	if c.Languages.Python.Enabled && (len(c.Languages.Python.Testpaths) == 0 || c.Languages.Python.CoverageDB == "") {
		return invalid("languages.python")
	}
	if c.Impact.MaxHops < 0 || !positive(c.Impact.CoverageMaxAgeCommits) || !positive(c.Impact.MaxCandidates) {
		return invalid("impact limits")
	}
	if len(c.Impact.CandidateRanking) == 0 {
		return invalid("impact.candidate_ranking")
	}
	seen := map[string]bool{}
	for _, rank := range c.Impact.CandidateRanking {
		if !oneOf(rank, "coverage_overlap", "hops", "cofailure", "recency") || seen[rank] {
			return invalid("impact.candidate_ranking")
		}
		seen[rank] = true
	}
	if !oneOf(c.Predictor.Kind, "static", "logistic", "jev", "ensemble") {
		return invalid("predictor.kind")
	}
	j := c.Predictor.Jev
	endpoint, err := url.Parse(j.Endpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil {
		return invalid("predictor.jev.endpoint")
	}
	if j.Model == "" || j.APIKeyEnv == "" || c.Predictor.Logistic.Weights == "" {
		return invalid("predictor identity")
	}
	if !positive(j.QuestionsPerRequest) || !positive(j.MaxStateTokens) || !positive(j.MaxRequestTokens) || !positive(j.TimeoutMS) || j.Retries < 0 || !positive(j.RetryBackoffMS) || !positive(j.Concurrency) || j.RetryBackoffMS >= j.TimeoutMS {
		return invalid("predictor.jev limits")
	}
	if !finite(j.BudgetUSDPerRun) || j.BudgetUSDPerRun <= 0 {
		return invalid("predictor.jev.budget_usd_per_run")
	}
	if !probability(c.Control.Alpha) || !probability(c.Control.Delta) {
		return invalid("control probabilities")
	}
	if !oneOf(c.Control.Unit, "commit") || !oneOf(c.Control.LockMismatchAction, "full") {
		return invalid("control policy")
	}
	if !finite(c.Control.MinSavingRatio) || c.Control.MinSavingRatio < 0 || c.Control.MinSavingRatio >= 1 {
		return invalid("control.min_saving_ratio")
	}
	if !positive(c.Control.Drift.WindowRuns) {
		return invalid("control.drift.window_runs")
	}
	if c.Audit.FullSuiteSchedule == "" || !positive(c.Audit.MaxDetectionLatencyHours) {
		return invalid("audit")
	}
	if !oneOf(c.Privacy.Redact, "none", "structure", "hash") {
		return invalid("privacy.redact")
	}
	if c.Telemetry.Enabled && c.Telemetry.Path == "" {
		return invalid("telemetry.path")
	}
	return nil
}
