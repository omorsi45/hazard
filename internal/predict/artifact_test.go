package predict

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/omorsi45/hazard/internal/impact"
)

var digestForm = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func sampleTraining(opts LogisticOptions) TrainingInfo {
	return TrainingInfo{
		Cutoff:      "2026-09-01",
		FitGroups:   []string{"c1", "c2", "c3"},
		ScaleGroups: []string{"c4"},
		Positives:   3,
		Negatives:   9,
		Sources:     map[string]int{"ci": 2, "mutation": 10},
		CreatedAt:   "2026-10-28T12:00:00Z",
		Options:     opts,
	}
}

func sampleStatic() StaticArtifact {
	return StaticArtifact{
		Schema:         ArtifactSchema,
		FeatureVersion: impact.FeatureVersion,
		Model:          IsotonicModel{X: []float64{0.1, 0.5, 2}, Y: []float64{0.05, 0.25, 0.75}},
		Training:       sampleTraining(LogisticOptions{}),
	}
}

func handLogistic() LogisticArtifact {
	return LogisticArtifact{
		Schema:         ArtifactSchema,
		FeatureVersion: impact.FeatureVersion,
		Model:          handModel(),
		Training:       sampleTraining(DefaultLogisticOptions()),
	}
}

// fittedLogistic returns an artifact around a model fitted on the fixture sets.
func fittedLogistic(t *testing.T) LogisticArtifact {
	t.Helper()
	fit, scale := separable()
	opts := DefaultLogisticOptions()
	m, err := FitLogistic(fit, scale, opts)
	if err != nil {
		t.Fatalf("FitLogistic: %v", err)
	}
	return LogisticArtifact{
		Schema:         ArtifactSchema,
		FeatureVersion: impact.FeatureVersion,
		Model:          m,
		Training:       sampleTraining(opts),
	}
}

// forgeStatic writes a with a correct digest but without any validation, so a
// test can present a self-consistent file that only a semantic check rejects.
func forgeStatic(t *testing.T, path string, a StaticArtifact) {
	t.Helper()
	d, err := a.computeDigest()
	if err != nil {
		t.Fatalf("computeDigest: %v", err)
	}
	a.Digest = d
	data, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func forgeLogistic(t *testing.T, path string, a LogisticArtifact) {
	t.Helper()
	d, err := a.computeDigest()
	if err != nil {
		t.Fatalf("computeDigest: %v", err)
	}
	a.Digest = d
	data, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// tamper rewrites the JSON document at path after edit changed it; the digest
// field is left as it was unless edit touches it.
func tamper(t *testing.T, path string, edit func(doc map[string]any)) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	edit(doc)
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
}

func obj(doc map[string]any, key string) map[string]any { return doc[key].(map[string]any) }

func bump(list []any, i int, by float64) { list[i] = list[i].(float64) + by }

func TestArtifactStaticRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "static.json")
	a := sampleStatic()
	if err := WriteStatic(path, a); err != nil {
		t.Fatalf("WriteStatic: %v", err)
	}
	got, err := ReadStatic(path)
	if err != nil {
		t.Fatalf("ReadStatic: %v", err)
	}
	if !digestForm.MatchString(got.Digest) {
		t.Errorf("Digest = %q, want sha256:<64 hex>", got.Digest)
	}
	want := a
	want.Digest = got.Digest
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip changed the artifact:\n got %+v\nwant %+v", got, want)
	}
}

func TestArtifactLogisticRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logistic.json")
	a := fittedLogistic(t)
	if err := WriteLogistic(path, a); err != nil {
		t.Fatalf("WriteLogistic: %v", err)
	}
	got, err := ReadLogistic(path)
	if err != nil {
		t.Fatalf("ReadLogistic: %v", err)
	}
	if !digestForm.MatchString(got.Digest) {
		t.Errorf("Digest = %q, want sha256:<64 hex>", got.Digest)
	}
	want := a
	want.Digest = got.Digest
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip changed the artifact:\n got %+v\nwant %+v", got, want)
	}
}

func goldenDigest(canonical string) string {
	sum := sha256.Sum256([]byte(canonical))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestArtifactDigestIsTheHashOfTheCanonicalJSON(t *testing.T) {
	dir := t.TempDir()
	const staticJSON = `{"schema":1,"feature_version":"f2","model":{"x":[0.1,0.5,2],"y":[0.05,0.25,0.75]},` +
		`"training":{"cutoff":"2026-09-01","fit_groups":["c1","c2","c3"],"scale_groups":["c4"],"positives":3,"negatives":9,` +
		`"sources":{"ci":2,"mutation":10},"created_at":"2026-10-28T12:00:00Z",` +
		`"options":{"l2":0,"ci_weight":0,"max_iter":0,"tol":0,"learning_rate":0}},"digest":""}`
	const logisticJSON = `{"schema":1,"feature_version":"f2","model":{"feature_version":"f2","means":[1,2],"scales":[2,4],` +
		`"weights":[1,-1],"bias":0.5,"platt_a":2,"platt_b":-1,"iterations":10,"converged":true},` +
		`"training":{"cutoff":"2026-09-01","fit_groups":["c1","c2","c3"],"scale_groups":["c4"],"positives":3,"negatives":9,` +
		`"sources":{"ci":2,"mutation":10},"created_at":"2026-10-28T12:00:00Z",` +
		`"options":{"l2":1,"ci_weight":10,"max_iter":5000,"tol":1e-9,"learning_rate":0.1}},"digest":""}`

	sp := filepath.Join(dir, "static.json")
	if err := WriteStatic(sp, sampleStatic()); err != nil {
		t.Fatal(err)
	}
	s, err := ReadStatic(sp)
	if err != nil {
		t.Fatal(err)
	}
	if want := goldenDigest(staticJSON); s.Digest != want {
		t.Errorf("static digest = %s, want %s", s.Digest, want)
	}

	lp := filepath.Join(dir, "logistic.json")
	if err := WriteLogistic(lp, handLogistic()); err != nil {
		t.Fatal(err)
	}
	l, err := ReadLogistic(lp)
	if err != nil {
		t.Fatal(err)
	}
	if want := goldenDigest(logisticJSON); l.Digest != want {
		t.Errorf("logistic digest = %s, want %s", l.Digest, want)
	}
}

func TestArtifactWriteIgnoresASuppliedDigest(t *testing.T) {
	dir := t.TempDir()
	bogus := "sha256:" + strings.Repeat("0", 64)
	a := sampleStatic()
	a.Digest = bogus
	if err := WriteStatic(filepath.Join(dir, "static.json"), a); err != nil {
		t.Fatalf("WriteStatic: %v", err)
	}
	got, err := ReadStatic(filepath.Join(dir, "static.json"))
	if err != nil {
		t.Fatalf("ReadStatic: %v", err)
	}
	if got.Digest == bogus || !digestForm.MatchString(got.Digest) {
		t.Errorf("Digest = %q, want a computed digest", got.Digest)
	}
}

func TestArtifactBytesAreDeterministic(t *testing.T) {
	dir := t.TempDir()
	read := func(name string) []byte {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	for _, name := range []string{"one.json", "two.json"} {
		if err := WriteLogistic(filepath.Join(dir, name), fittedLogistic(t)); err != nil {
			t.Fatalf("WriteLogistic: %v", err)
		}
	}
	if one, two := read("one.json"), read("two.json"); !bytes.Equal(one, two) {
		t.Errorf("two fits of the same inputs wrote different bytes:\n%s\n%s", one, two)
	}
	for _, name := range []string{"s1.json", "s2.json"} {
		if err := WriteStatic(filepath.Join(dir, name), sampleStatic()); err != nil {
			t.Fatalf("WriteStatic: %v", err)
		}
	}
	if one, two := read("s1.json"), read("s2.json"); !bytes.Equal(one, two) {
		t.Errorf("the same static artifact wrote different bytes:\n%s\n%s", one, two)
	}
	if data := read("s1.json"); !bytes.HasSuffix(data, []byte("}\n")) || bytes.Contains(data, []byte("\r")) {
		t.Errorf("artifact file should end in a single LF and hold no CR: %q", data[len(data)-4:])
	}
}

func TestArtifactTamperedIsRejected(t *testing.T) {
	logisticEdits := map[string]func(doc map[string]any){
		"weight":     func(d map[string]any) { bump(obj(d, "model")["weights"].([]any), 0, 0.001) },
		"mean":       func(d map[string]any) { bump(obj(d, "model")["means"].([]any), 1, 0.5) },
		"scale":      func(d map[string]any) { bump(obj(d, "model")["scales"].([]any), 0, 0.5) },
		"bias":       func(d map[string]any) { obj(d, "model")["bias"] = obj(d, "model")["bias"].(float64) + 0.5 },
		"platt a":    func(d map[string]any) { obj(d, "model")["platt_a"] = obj(d, "model")["platt_a"].(float64) + 0.5 },
		"platt b":    func(d map[string]any) { obj(d, "model")["platt_b"] = obj(d, "model")["platt_b"].(float64) + 0.5 },
		"converged":  func(d map[string]any) { obj(d, "model")["converged"] = !obj(d, "model")["converged"].(bool) },
		"iterations": func(d map[string]any) { obj(d, "model")["iterations"] = obj(d, "model")["iterations"].(float64) + 1 },
		"positives": func(d map[string]any) {
			obj(d, "training")["positives"] = obj(d, "training")["positives"].(float64) + 1
		},
		"cutoff":  func(d map[string]any) { obj(d, "training")["cutoff"] = "2026-09-02" },
		"options": func(d map[string]any) { obj(obj(d, "training"), "options")["l2"] = 2.0 },
		"digest of another artifact": func(d map[string]any) {
			d["digest"] = goldenDigest("another artifact")
		},
		"digest cleared": func(d map[string]any) { d["digest"] = "" },
	}
	for name, edit := range logisticEdits {
		t.Run("logistic "+name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "logistic.json")
			if err := WriteLogistic(path, fittedLogistic(t)); err != nil {
				t.Fatalf("WriteLogistic: %v", err)
			}
			if _, err := ReadLogistic(path); err != nil {
				t.Fatalf("untouched artifact rejected: %v", err)
			}
			tamper(t, path, edit)
			_, err := ReadLogistic(path)
			if err == nil {
				t.Fatal("ReadLogistic accepted a tampered artifact")
			}
			if !strings.Contains(err.Error(), "digest") {
				t.Errorf("error %q does not name the digest", err)
			}
		})
	}
	staticEdits := map[string]func(doc map[string]any){
		"x":        func(d map[string]any) { bump(obj(d, "model")["x"].([]any), 0, 0.01) },
		"y":        func(d map[string]any) { bump(obj(d, "model")["y"].([]any), 2, -0.01) },
		"group":    func(d map[string]any) { obj(d, "training")["fit_groups"] = []any{"c1", "c2"} },
		"sources":  func(d map[string]any) { obj(obj(d, "training"), "sources")["ci"] = 3.0 },
		"created":  func(d map[string]any) { obj(d, "training")["created_at"] = "2026-10-29T00:00:00Z" },
		"negative": func(d map[string]any) { obj(d, "training")["negatives"] = 8.0 },
	}
	for name, edit := range staticEdits {
		t.Run("static "+name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "static.json")
			if err := WriteStatic(path, sampleStatic()); err != nil {
				t.Fatalf("WriteStatic: %v", err)
			}
			tamper(t, path, edit)
			_, err := ReadStatic(path)
			if err == nil {
				t.Fatal("ReadStatic accepted a tampered artifact")
			}
			if !strings.Contains(err.Error(), "digest") {
				t.Errorf("error %q does not name the digest", err)
			}
		})
	}
}

func TestArtifactWrongFeatureVersionIsRejected(t *testing.T) {
	logistic := map[string]func(a *LogisticArtifact){
		"artifact only": func(a *LogisticArtifact) { a.FeatureVersion = "f1" },
		"artifact empty": func(a *LogisticArtifact) {
			a.FeatureVersion = ""
		},
		"model only": func(a *LogisticArtifact) { a.Model.FeatureVersion = "f1" },
		"both":       func(a *LogisticArtifact) { a.FeatureVersion, a.Model.FeatureVersion = "f1", "f1" },
	}
	for name, edit := range logistic {
		t.Run("logistic "+name, func(t *testing.T) {
			a := handLogistic()
			edit(&a)
			path := filepath.Join(t.TempDir(), "logistic.json")
			forgeLogistic(t, path, a) // self-consistent: only the version check can refuse it
			_, err := ReadLogistic(path)
			if err == nil {
				t.Fatal("ReadLogistic accepted a foreign feature version")
			}
			if !strings.Contains(err.Error(), "feature version") {
				t.Errorf("error %q does not name the feature version", err)
			}
			if err := WriteLogistic(filepath.Join(t.TempDir(), "out.json"), a); err == nil {
				t.Error("WriteLogistic accepted a foreign feature version")
			}
		})
	}
	t.Run("static", func(t *testing.T) {
		a := sampleStatic()
		a.FeatureVersion = "f1"
		path := filepath.Join(t.TempDir(), "static.json")
		forgeStatic(t, path, a)
		_, err := ReadStatic(path)
		if err == nil {
			t.Fatal("ReadStatic accepted a foreign feature version")
		}
		if !strings.Contains(err.Error(), "feature version") {
			t.Errorf("error %q does not name the feature version", err)
		}
		if err := WriteStatic(filepath.Join(t.TempDir(), "out.json"), a); err == nil {
			t.Error("WriteStatic accepted a foreign feature version")
		}
	})
}

func TestArtifactWrongSchemaIsRejected(t *testing.T) {
	for _, schema := range []int{0, 2, -1} {
		sa := sampleStatic()
		sa.Schema = schema
		sp := filepath.Join(t.TempDir(), "static.json")
		forgeStatic(t, sp, sa)
		if _, err := ReadStatic(sp); err == nil || !strings.Contains(err.Error(), "schema") {
			t.Errorf("ReadStatic with schema %d: err = %v, want a schema error", schema, err)
		}
		if err := WriteStatic(filepath.Join(t.TempDir(), "o.json"), sa); err == nil {
			t.Errorf("WriteStatic accepted schema %d", schema)
		}
		la := handLogistic()
		la.Schema = schema
		lp := filepath.Join(t.TempDir(), "logistic.json")
		forgeLogistic(t, lp, la)
		if _, err := ReadLogistic(lp); err == nil || !strings.Contains(err.Error(), "schema") {
			t.Errorf("ReadLogistic with schema %d: err = %v, want a schema error", schema, err)
		}
		if err := WriteLogistic(filepath.Join(t.TempDir(), "o.json"), la); err == nil {
			t.Errorf("WriteLogistic accepted schema %d", schema)
		}
	}
}

func TestArtifactInvalidModelIsRejected(t *testing.T) {
	static := map[string]func(m *IsotonicModel){
		"empty":              func(m *IsotonicModel) { m.X, m.Y = nil, nil },
		"lengths differ":     func(m *IsotonicModel) { m.Y = m.Y[:2] },
		"X repeated":         func(m *IsotonicModel) { m.X[1] = m.X[0] },
		"X descending":       func(m *IsotonicModel) { m.X[0], m.X[2] = m.X[2], m.X[0] },
		"Y decreasing":       func(m *IsotonicModel) { m.Y[0], m.Y[2] = m.Y[2], m.Y[0] },
		"Y above one":        func(m *IsotonicModel) { m.Y[2] = 1.5 },
		"Y below zero":       func(m *IsotonicModel) { m.Y[0] = -0.1 },
		"Y large and finite": func(m *IsotonicModel) { m.Y[2] = 1e300 },
	}
	for name, edit := range static {
		t.Run("static "+name, func(t *testing.T) {
			a := sampleStatic()
			a.Model = IsotonicModel{X: append([]float64(nil), a.Model.X...), Y: append([]float64(nil), a.Model.Y...)}
			edit(&a.Model)
			path := filepath.Join(t.TempDir(), "static.json")
			forgeStatic(t, path, a)
			if _, err := ReadStatic(path); err == nil {
				t.Error("ReadStatic accepted an invalid model")
			}
			if err := WriteStatic(filepath.Join(t.TempDir(), "o.json"), a); err == nil {
				t.Error("WriteStatic accepted an invalid model")
			}
		})
	}
	logistic := map[string]func(m *LogisticModel){
		"no weights":     func(m *LogisticModel) { m.Weights, m.Means, m.Scales = nil, nil, nil },
		"means length":   func(m *LogisticModel) { m.Means = m.Means[:1] },
		"zero scale":     func(m *LogisticModel) { m.Scales[0] = 0 },
		"negative scale": func(m *LogisticModel) { m.Scales[1] = -1 },
		"iterations":     func(m *LogisticModel) { m.Iterations = -3 },
	}
	for name, edit := range logistic {
		t.Run("logistic "+name, func(t *testing.T) {
			a := handLogistic()
			m := a.Model
			m.Means = append([]float64(nil), m.Means...)
			m.Scales = append([]float64(nil), m.Scales...)
			m.Weights = append([]float64(nil), m.Weights...)
			edit(&m)
			a.Model = m
			path := filepath.Join(t.TempDir(), "logistic.json")
			forgeLogistic(t, path, a)
			if _, err := ReadLogistic(path); err == nil {
				t.Error("ReadLogistic accepted an invalid model")
			}
			if err := WriteLogistic(filepath.Join(t.TempDir(), "o.json"), a); err == nil {
				t.Error("WriteLogistic accepted an invalid model")
			}
		})
	}
}

func TestArtifactMalformedFilesAreRejected(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.json")
	if err := WriteStatic(good, sampleStatic()); err != nil {
		t.Fatal(err)
	}
	goodData, err := os.ReadFile(good)
	if err != nil {
		t.Fatal(err)
	}
	logisticPath := filepath.Join(dir, "logistic.json")
	if err := WriteLogistic(logisticPath, handLogistic()); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func() string{
		"missing file": func() string { return filepath.Join(dir, "absent.json") },
		"empty file":   func() string { return write(t, dir, "empty.json", nil) },
		"not json":     func() string { return write(t, dir, "junk.json", []byte("not json")) },
		"trailing data": func() string {
			return write(t, dir, "trail.json", append(append([]byte(nil), goodData...), []byte("{}")...))
		},
		"unknown field": func() string {
			p := write(t, dir, "extra.json", goodData)
			tamper(t, p, func(d map[string]any) { d["note"] = "x" })
			return p
		},
		"unknown nested field": func() string {
			p := write(t, dir, "nested.json", goodData)
			tamper(t, p, func(d map[string]any) { obj(d, "model")["z"] = []any{1.0} })
			return p
		},
		"logistic artifact read as static": func() string { return logisticPath },
	}
	for name, path := range cases {
		t.Run("static "+name, func(t *testing.T) {
			if _, err := ReadStatic(path()); err == nil {
				t.Error("ReadStatic returned no error")
			}
		})
	}
	t.Run("logistic reads a static artifact", func(t *testing.T) {
		if _, err := ReadLogistic(good); err == nil {
			t.Error("ReadLogistic accepted a static artifact")
		}
	})
	t.Run("logistic missing file", func(t *testing.T) {
		if _, err := ReadLogistic(filepath.Join(dir, "absent.json")); err == nil {
			t.Error("ReadLogistic returned no error")
		}
	})
}

func write(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestArtifactWriteLeavesNoPartialFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "static.json")
	if err := WriteStatic(path, sampleStatic()); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	names := func() []string {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range entries {
			out = append(out, e.Name())
		}
		return out
	}

	bad := sampleStatic()
	bad.Model.Y = []float64{0.9, 0.5, 0.1}
	if err := WriteStatic(path, bad); err == nil {
		t.Fatal("WriteStatic accepted an invalid artifact")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("a refused write changed the existing artifact")
	}
	if got := names(); !reflect.DeepEqual(got, []string{"static.json"}) {
		t.Errorf("directory after a refused write = %v, want only static.json", got)
	}

	// A write that fails at the final rename must not leave its temp file.
	if err := os.Mkdir(filepath.Join(dir, "target"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteStatic(filepath.Join(dir, "target"), sampleStatic()); err == nil {
		t.Fatal("WriteStatic replaced a directory")
	}
	if got := names(); !reflect.DeepEqual(got, []string{"static.json", "target"}) {
		t.Errorf("directory after a failed rename = %v, want static.json and target only", got)
	}
}
