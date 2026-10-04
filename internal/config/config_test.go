package config

import (
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDefaultsAndPartialLoad(t *testing.T) {
	d := Defaults()
	if d.Version != 1 || d.Impact.MaxCandidates != 400 || d.Control.Drift.WindowRuns != 500 || d.Predictor.Jev.Endpoint != "https://openrouter.ai/api/alpha/decisions" {
		t.Fatalf("unexpected defaults: %+v", d)
	}
	p := filepath.Join(t.TempDir(), "partial.yml")
	if err := os.WriteFile(p, []byte("impact:\n  max_candidates: 25\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Impact.MaxCandidates != 25 || c.Predictor.Jev.Model != d.Predictor.Jev.Model || c.Languages.Go.Packages[0] != "./..." {
		t.Fatalf("partial config did not merge defaults: %+v", c)
	}
	if err := os.WriteFile(p, []byte("impact:\n  unknown: 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("unknown field: %v", err)
	}
}

func TestLoadPathAndErrors(t *testing.T) {
	dir := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	d, err := Load("")
	if err != nil || d.Version != 1 {
		t.Fatalf("absent config: %+v %v", d, err)
	}
	if err := os.Mkdir(".hazard", 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(".hazard/hazard.yml", []byte("impact:\n  max_candidates: 21\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("hazard.yml", []byte("impact:\n  max_candidates: 22\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load("")
	if err != nil || c.Impact.MaxCandidates != 22 {
		t.Fatalf("root precedence: %+v %v", c, err)
	}
	if err := os.Remove("hazard.yml"); err != nil {
		t.Fatal(err)
	}
	c, err = Load("")
	if err != nil || c.Impact.MaxCandidates != 21 {
		t.Fatalf("automatic fallback: %+v %v", c, err)
	}
	c, err = Load(".hazard/hazard.yml")
	if err != nil || c.Impact.MaxCandidates != 21 {
		t.Fatalf("explicit precedence: %+v %v", c, err)
	}
	if _, err := Load("missing.yml"); err == nil {
		t.Fatal("explicit missing config accepted")
	}
	if err := os.WriteFile("bad.yml", []byte("impact: ["), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load("bad.yml"); err == nil {
		t.Fatal("invalid YAML accepted")
	}
}

func TestValidation(t *testing.T) {
	cases := map[string]func(*Config){
		"version":          func(c *Config) { c.Version = 2 },
		"no language":      func(c *Config) { c.Languages.Go.Enabled = false; c.Languages.Python.Enabled = false },
		"go packages":      func(c *Config) { c.Languages.Go.Packages = nil },
		"alpha zero":       func(c *Config) { c.Control.Alpha = 0 },
		"alpha nan":        func(c *Config) { c.Control.Alpha = math.NaN() },
		"delta infinity":   func(c *Config) { c.Control.Delta = math.Inf(1) },
		"saving negative":  func(c *Config) { c.Control.MinSavingRatio = -1 },
		"cost nan":         func(c *Config) { c.Predictor.Jev.BudgetUSDPerRun = math.NaN() },
		"bad predictor":    func(c *Config) { c.Predictor.Kind = "mystery" },
		"bad ranking":      func(c *Config) { c.Impact.CandidateRanking = []string{"mystery"} },
		"bad redaction":    func(c *Config) { c.Privacy.Redact = "mystery" },
		"bad unit":         func(c *Config) { c.Control.Unit = "test" },
		"bad action":       func(c *Config) { c.Control.LockMismatchAction = "ignore" },
		"negative hops":    func(c *Config) { c.Impact.MaxHops = -1 },
		"zero candidates":  func(c *Config) { c.Impact.MaxCandidates = 0 },
		"negative retries": func(c *Config) { c.Predictor.Jev.Retries = -1 },
		"zero concurrency": func(c *Config) { c.Predictor.Jev.Concurrency = 0 },
		"bad timeout":      func(c *Config) { c.Predictor.Jev.RetryBackoffMS = c.Predictor.Jev.TimeoutMS },
		"bad endpoint":     func(c *Config) { c.Predictor.Jev.Endpoint = "http://example.com" },
		"bad model":        func(c *Config) { c.Predictor.Jev.Model = "" },
		"bad schedule":     func(c *Config) { c.Audit.FullSuiteSchedule = "" },
		"bad drift window": func(c *Config) { c.Control.Drift.WindowRuns = 0 },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			c := Defaults()
			change(&c)
			if err := c.Validate(); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
	c := Defaults()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestShippedConfigMatchesDefaults(t *testing.T) {
	c, err := Load("../../hazard.yml")
	if err != nil {
		t.Fatal(err)
	}
	d := Defaults()
	if !reflect.DeepEqual(c, d) {
		t.Fatalf("shipped config drift: %+v", c)
	}
}

func TestLoadRejectsFractionalIntegers(t *testing.T) {
	cases := map[string]string{
		"version":       "version: 1.9\n",
		"candidates":    "impact:\n  max_candidates: 2.9\n",
		"negative hops": "impact:\n  max_hops: -0.5\n",
		"retries":       "predictor:\n  jev:\n    retries: 0.5\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "hazard.yml")
			if err := os.WriteFile(p, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(p); err == nil {
				t.Fatal("fractional integer accepted")
			}
		})
	}
}

func TestZeroRetriesAllowedAndZeroBudgetRejected(t *testing.T) {
	c := Defaults()
	c.Predictor.Jev.Retries = 0
	if err := c.Validate(); err != nil {
		t.Fatalf("zero retries rejected: %v", err)
	}
	c.Predictor.Jev.BudgetUSDPerRun = 0
	if err := c.Validate(); err == nil {
		t.Fatal("zero budget accepted")
	}
}

func TestZeroBudgetRejected(t *testing.T) {
	c := Defaults()
	c.Predictor.Jev.BudgetUSDPerRun = 0
	if err := c.Validate(); err == nil {
		t.Fatal("zero budget accepted")
	}
}

func TestLoadRejectsMergeAndAliasSyntax(t *testing.T) {
	cases := map[string]string{
		"merge hides fractional counts":    "impact:\n  <<: {max_hops: -0.5, max_candidates: 2.9}\n",
		"aliased key hides fractional hop": "repo:\n  default_branch: &key max_hops\nimpact:\n  *key: -0.5\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "hazard.yml")
			if err := os.WriteFile(p, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(p); err == nil {
				t.Fatal("YAML merge or alias syntax accepted")
			}
		})
	}
}
