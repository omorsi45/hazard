package engine

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omorsi45/hazard/internal/config"
	"github.com/omorsi45/hazard/internal/control"
	"github.com/omorsi45/hazard/internal/impact"
	"github.com/omorsi45/hazard/internal/predict"
)

// featureReachable is the index of the "reachable" feature in
// impact.Features.Vector.
const featureReachable = 1

// writeStaticArtifact writes a sealed isotonic artifact that maps a rank of 0
// to low and a rank of 1 or more to high, and returns it as read back.
func writeStaticArtifact(t *testing.T, path string, low, high float64) predict.StaticArtifact {
	t.Helper()
	a := predict.StaticArtifact{
		Schema: predict.ArtifactSchema, FeatureVersion: impact.FeatureVersion,
		Model: predict.IsotonicModel{X: []float64{0, 1}, Y: []float64{low, high}},
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := predict.WriteStatic(path, a); err != nil {
		t.Fatal(err)
	}
	read, err := predict.ReadStatic(path)
	if err != nil {
		t.Fatal(err)
	}
	return read
}

// writeLogisticArtifact writes a sealed logistic artifact whose probability is
// sigmoid(reachable*weight + bias): a reachable test scores sigmoid(weight+bias)
// and an unreachable one sigmoid(bias). It returns the artifact as read back.
func writeLogisticArtifact(t *testing.T, path string, weight, bias float64) predict.LogisticArtifact {
	t.Helper()
	d := len(impact.Features{}.Vector())
	means, scales, weights := make([]float64, d), make([]float64, d), make([]float64, d)
	for i := range scales {
		scales[i] = 1
	}
	weights[featureReachable] = weight
	a := predict.LogisticArtifact{
		Schema: predict.ArtifactSchema, FeatureVersion: impact.FeatureVersion,
		Model: predict.LogisticModel{
			FeatureVersion: impact.FeatureVersion, Means: means, Scales: scales, Weights: weights,
			Bias: bias, PlattA: 1, PlattB: 0, Converged: true,
		},
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := predict.WriteLogistic(path, a); err != nil {
		t.Fatal(err)
	}
	read, err := predict.ReadLogistic(path)
	if err != nil {
		t.Fatal(err)
	}
	return read
}

// configIn is the default configuration with the artifacts under dir.
func configIn(dir string) config.Config {
	cfg := config.Defaults()
	cfg.Predictor.Static.Calibration = filepath.Join(dir, "static.json")
	cfg.Predictor.Logistic.Weights = filepath.Join(dir, "logistic.json")
	return cfg
}

func TestLoadPredictorStatic(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a := writeStaticArtifact(t, filepath.Join(dir, "static.json"), 0.01, 0.5)

	p, id, err := LoadPredictor(dir, configIn(dir), "static")

	if err != nil || p == nil {
		t.Fatalf("LoadPredictor = %v, %v; want a predictor", p, err)
	}
	want := control.Identity{Predictor: "static", ModelRelease: "p0", RubricVersion: "none", FeatureVersion: impact.FeatureVersion, WeightsDigest: a.Digest}
	if id != want {
		t.Errorf("identity\n got: %+v\nwant: %+v", id, want)
	}
	if p.Name() != "static" || !strings.HasSuffix(p.Version(), a.Digest) {
		t.Errorf("predictor %s %s, want the static predictor of the artifact", p.Name(), p.Version())
	}
}

func TestLoadPredictorLogistic(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a := writeLogisticArtifact(t, filepath.Join(dir, "logistic.json"), 4, -6)

	p, id, err := LoadPredictor(dir, configIn(dir), "logistic")

	if err != nil || p == nil {
		t.Fatalf("LoadPredictor = %v, %v; want a predictor", p, err)
	}
	want := control.Identity{Predictor: "logistic", ModelRelease: "p1", RubricVersion: "none", FeatureVersion: impact.FeatureVersion, WeightsDigest: a.Digest}
	if id != want {
		t.Errorf("identity\n got: %+v\nwant: %+v", id, want)
	}
	if p.Name() != "logistic" {
		t.Errorf("predictor %s", p.Name())
	}
}

func TestLoadPredictorResolvesRelativeArtifactPathsAgainstTheRepository(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	writeLogisticArtifact(t, filepath.Join(repo, ".hazard", "logistic.json"), 4, -6)

	if _, _, err := LoadPredictor(repo, config.Defaults(), "logistic"); err != nil {
		t.Errorf("the default relative path .hazard/logistic.json was not found under the repository: %v", err)
	}
}

func TestLoadPredictorArtifactFailuresAreUnavailable(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		kind  string
		setup func(t *testing.T, dir string)
	}{
		"static missing":                         {"static", func(*testing.T, string) {}},
		"logistic missing":                       {"logistic", func(*testing.T, string) {}},
		"ensemble without its logistic artifact": {"ensemble", func(*testing.T, string) {}},
		"static unreadable": {"static", func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "static.json"), []byte("{not json"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		"logistic tampered": {"logistic", func(t *testing.T, dir string) {
			path := filepath.Join(dir, "logistic.json")
			writeLogisticArtifact(t, path, 4, -6)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			data = []byte(strings.Replace(string(data), `"bias": -6`, `"bias": -5`, 1))
			if err := os.WriteFile(path, data, 0o644); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			tc.setup(t, dir)

			p, id, err := LoadPredictor(dir, configIn(dir), tc.kind)

			if !errors.Is(err, ErrPredictorUnavailable) {
				t.Fatalf("err = %v, want ErrPredictorUnavailable", err)
			}
			if p != nil || id != (control.Identity{}) {
				t.Errorf("a failed load returned a predictor %v or an identity %+v", p, id)
			}
		})
	}
}

func TestLoadPredictorUnknownKindIsNotUnavailable(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"", "xgboost", "Logistic"} {
		_, _, err := LoadPredictor(t.TempDir(), config.Defaults(), kind)
		if err == nil || errors.Is(err, ErrPredictorUnavailable) {
			t.Errorf("kind %q: err = %v, want an error that is not ErrPredictorUnavailable", kind, err)
		}
	}
}

func TestLoadPredictorJev(t *testing.T) {
	cfg := config.Defaults()
	t.Setenv(cfg.Predictor.Jev.APIKeyEnv, "sk-test-engine-key")

	p, id, err := LoadPredictor(t.TempDir(), cfg, "jev")

	if err != nil || p == nil {
		t.Fatalf("LoadPredictor = %v, %v; want a predictor", p, err)
	}
	want := control.Identity{Predictor: "jev", ModelRelease: cfg.Predictor.Jev.Model, RubricVersion: predict.RubricR1, FeatureVersion: impact.FeatureVersion}
	if id != want {
		t.Errorf("identity\n got: %+v\nwant: %+v", id, want)
	}
	if p.Name() != "jev" {
		t.Errorf("predictor %s", p.Name())
	}
	if strings.Contains(strings.Join([]string{id.Predictor, id.ModelRelease, id.WeightsDigest}, ""), "sk-test") {
		t.Error("the key leaked into the identity")
	}
}

func TestLoadPredictorEnsemble(t *testing.T) {
	dir := t.TempDir()
	cfg := configIn(dir)
	a := writeLogisticArtifact(t, filepath.Join(dir, "logistic.json"), 4, -6)
	t.Setenv(cfg.Predictor.Jev.APIKeyEnv, "sk-test-engine-key")

	p, id, err := LoadPredictor(dir, cfg, "ensemble")

	if err != nil || p == nil {
		t.Fatalf("LoadPredictor = %v, %v; want a predictor", p, err)
	}
	want := control.Identity{Predictor: "ensemble", ModelRelease: cfg.Predictor.Jev.Model, RubricVersion: predict.RubricR1, FeatureVersion: impact.FeatureVersion, WeightsDigest: a.Digest}
	if id != want {
		t.Errorf("identity\n got: %+v\nwant: %+v", id, want)
	}
	if p.Name() != "ensemble" {
		t.Errorf("predictor %s", p.Name())
	}
}

func TestLoadPredictorMissingKeyIsUnavailableNotReplaced(t *testing.T) {
	dir := t.TempDir()
	cfg := configIn(dir)
	writeLogisticArtifact(t, filepath.Join(dir, "logistic.json"), 4, -6)
	t.Setenv(cfg.Predictor.Jev.APIKeyEnv, "")

	for _, kind := range []string{"jev", "ensemble"} {
		p, _, err := LoadPredictor(dir, cfg, kind)

		if !errors.Is(err, ErrPredictorUnavailable) || p != nil {
			t.Fatalf("%s: err = %v, predictor %v; want ErrPredictorUnavailable and no predictor, never a substitute", kind, err, p)
		}
		want := "OPENROUTER_API_KEY unset: the locked predictor " + kind + " is unavailable; calibrate a logistic lock to select offline"
		if err.Error() != want {
			t.Errorf("%s: message %q, want %q", kind, err.Error(), want)
		}
	}
}

func TestLoadPredictorMissingKeyNamesTheConfiguredVariable(t *testing.T) {
	cfg := config.Defaults()
	cfg.Predictor.Jev.APIKeyEnv = "MY_JEV_KEY_FOR_TEST"
	t.Setenv("MY_JEV_KEY_FOR_TEST", "")

	_, _, err := LoadPredictor(t.TempDir(), cfg, "jev")

	if err == nil || !strings.HasPrefix(err.Error(), "MY_JEV_KEY_FOR_TEST unset:") {
		t.Errorf("err = %v, want one naming the configured variable", err)
	}
}

func TestLoadPredictorPrivacyForbidsTheExternalPredictor(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*config.Config)
		key    string
		want   string
	}{
		"disabled, with a key":       {func(c *config.Config) { c.Privacy.AllowExternalPredictor = false }, "sk-test-engine-key", "allow_external_predictor"},
		"disabled, without a key":    {func(c *config.Config) { c.Privacy.AllowExternalPredictor = false }, "", "allow_external_predictor"},
		"hash redaction, with a key": {func(c *config.Config) { c.Privacy.Redact = "hash" }, "sk-test-engine-key", "privacy.redact"},
		"hash redaction, no key":     {func(c *config.Config) { c.Privacy.Redact = "hash" }, "", "privacy.redact"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := config.Defaults()
			tc.mutate(&cfg)
			t.Setenv(cfg.Predictor.Jev.APIKeyEnv, tc.key)

			for _, kind := range []string{"jev", "ensemble"} {
				dir := t.TempDir()
				cfg.Predictor.Logistic.Weights = filepath.Join(dir, "logistic.json")
				writeLogisticArtifact(t, cfg.Predictor.Logistic.Weights, 4, -6)

				p, _, err := LoadPredictor(dir, cfg, kind)

				if !errors.Is(err, ErrPredictorUnavailable) || p != nil || !strings.Contains(err.Error(), tc.want) {
					t.Errorf("%s: err = %v, predictor %v; want ErrPredictorUnavailable naming %q", kind, err, p, tc.want)
				}
			}
		})
	}
}
