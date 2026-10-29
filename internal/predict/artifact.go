package predict

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/omorsi45/hazard/internal/impact"
)

// ArtifactSchema is the artifact layout version Read accepts.
const ArtifactSchema = 1

// TrainingInfo records how an artifact was fitted: the cutoff, the exact
// source groups on each side of the split, the class counts, the label
// sources, the creation time and the fit options.
type TrainingInfo struct {
	Cutoff      string          `json:"cutoff"`
	FitGroups   []string        `json:"fit_groups"`
	ScaleGroups []string        `json:"scale_groups"`
	Positives   int             `json:"positives"`
	Negatives   int             `json:"negatives"`
	Sources     map[string]int  `json:"sources"`
	CreatedAt   string          `json:"created_at"`
	Options     LogisticOptions `json:"options"`
}

// StaticArtifact is the P0 isotonic model with its provenance. Digest is
// "sha256:" and the hex SHA-256 of the artifact's compact JSON encoding with
// Digest empty; WriteStatic sets it and ReadStatic verifies it.
type StaticArtifact struct {
	Schema         int           `json:"schema"`
	FeatureVersion string        `json:"feature_version"`
	Model          IsotonicModel `json:"model"`
	Training       TrainingInfo  `json:"training"`
	Digest         string        `json:"digest"`
}

// LogisticArtifact is the P1 logistic model with its provenance, digested
// the same way as StaticArtifact.
type LogisticArtifact struct {
	Schema         int           `json:"schema"`
	FeatureVersion string        `json:"feature_version"`
	Model          LogisticModel `json:"model"`
	Training       TrainingInfo  `json:"training"`
	Digest         string        `json:"digest"`
}

// digestOf returns the artifact digest of v, which must hold an empty Digest.
func digestOf(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("encode artifact: %w", err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func (a StaticArtifact) computeDigest() (string, error) {
	a.Digest = ""
	return digestOf(a)
}

func (a LogisticArtifact) computeDigest() (string, error) {
	a.Digest = ""
	return digestOf(a)
}

// validate reports whether the isotonic model is a usable step function:
// ascending finite breakpoints and non-decreasing values within [0,1].
func (m IsotonicModel) validate() error {
	if len(m.X) == 0 {
		return errors.New("isotonic model has no breakpoints")
	}
	if len(m.X) != len(m.Y) {
		return fmt.Errorf("isotonic model has %d breakpoints and %d values", len(m.X), len(m.Y))
	}
	for i := range m.X {
		if !isFinite(m.X[i]) || !isFinite(m.Y[i]) {
			return fmt.Errorf("isotonic model point %d is not finite", i)
		}
		if m.Y[i] < 0 || m.Y[i] > 1 {
			return fmt.Errorf("isotonic model value %d is %v, want within [0,1]", i, m.Y[i])
		}
		if i > 0 && !(m.X[i] > m.X[i-1]) {
			return fmt.Errorf("isotonic model breakpoints are not strictly ascending at %d", i)
		}
		if i > 0 && m.Y[i] < m.Y[i-1] {
			return fmt.Errorf("isotonic model values decrease at %d", i)
		}
	}
	return nil
}

// checkHeader verifies the schema and the feature version an artifact claims.
func checkHeader(schema int, featureVersion string) error {
	if schema != ArtifactSchema {
		return fmt.Errorf("unsupported artifact schema %d, want %d", schema, ArtifactSchema)
	}
	if featureVersion != impact.FeatureVersion {
		return fmt.Errorf("artifact feature version %q, want %q", featureVersion, impact.FeatureVersion)
	}
	return nil
}

// validate checks the schema, the feature version and the model, but not the
// digest.
func (a StaticArtifact) validate() error {
	if err := checkHeader(a.Schema, a.FeatureVersion); err != nil {
		return err
	}
	return a.Model.validate()
}

// verify is validate plus the check that Digest matches the content.
func (a StaticArtifact) verify() error {
	if err := a.validate(); err != nil {
		return err
	}
	digest, err := a.computeDigest()
	if err != nil {
		return err
	}
	if digest != a.Digest {
		return fmt.Errorf("digest %q does not match the content (%s)", a.Digest, digest)
	}
	return nil
}

// validate checks the schema, the feature version and the model, but not the
// digest.
func (a LogisticArtifact) validate() error {
	if err := checkHeader(a.Schema, a.FeatureVersion); err != nil {
		return err
	}
	if err := a.Model.validate(); err != nil {
		return fmt.Errorf("logistic model: %w", err)
	}
	return nil
}

// verify is validate plus the check that Digest matches the content.
func (a LogisticArtifact) verify() error {
	if err := a.validate(); err != nil {
		return err
	}
	digest, err := a.computeDigest()
	if err != nil {
		return err
	}
	if digest != a.Digest {
		return fmt.Errorf("digest %q does not match the content (%s)", a.Digest, digest)
	}
	return nil
}

// WriteStatic validates a, sets its digest and atomically replaces the file
// at path with the indented JSON. An invalid artifact is refused and the
// existing file is left untouched.
func WriteStatic(path string, a StaticArtifact) error {
	if err := a.validate(); err != nil {
		return fmt.Errorf("refusing to write static artifact: %w", err)
	}
	digest, err := a.computeDigest()
	if err != nil {
		return err
	}
	a.Digest = digest
	return writeJSON(path, a)
}

// ReadStatic reads the artifact at path and verifies its schema, feature
// version, digest and model. Unknown fields and trailing data are errors.
func ReadStatic(path string) (StaticArtifact, error) {
	var a StaticArtifact
	if err := readJSON(path, &a); err != nil {
		return StaticArtifact{}, err
	}
	if err := a.verify(); err != nil {
		return StaticArtifact{}, fmt.Errorf("static artifact %s: %w", path, err)
	}
	return a, nil
}

// WriteLogistic validates a, sets its digest and atomically replaces the file
// at path with the indented JSON. An invalid artifact is refused and the
// existing file is left untouched.
func WriteLogistic(path string, a LogisticArtifact) error {
	if err := a.validate(); err != nil {
		return fmt.Errorf("refusing to write logistic artifact: %w", err)
	}
	digest, err := a.computeDigest()
	if err != nil {
		return err
	}
	a.Digest = digest
	return writeJSON(path, a)
}

// ReadLogistic reads the artifact at path and verifies its schema, feature
// version, digest and model. Unknown fields and trailing data are errors.
func ReadLogistic(path string) (LogisticArtifact, error) {
	var a LogisticArtifact
	if err := readJSON(path, &a); err != nil {
		return LogisticArtifact{}, err
	}
	if err := a.verify(); err != nil {
		return LogisticArtifact{}, fmt.Errorf("logistic artifact %s: %w", path, err)
	}
	return a, nil
}

// readJSON strictly decodes the single JSON document at path into v.
func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read artifact: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("decode artifact %s: %w", path, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("decode artifact %s: unexpected data after the JSON document", path)
	}
	return nil
}

// writeJSON encodes v with indentation and a final newline, writes it to a
// temporary file next to path, syncs it and renames it over path.
func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("encode artifact: %w", err)
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+"-*.tmp")
	if err != nil {
		return fmt.Errorf("write artifact: %w", err)
	}
	cleanup := func(cause error) error {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("write artifact: %w", cause)
	}
	if _, err := tmp.Write(data); err != nil {
		return cleanup(err)
	}
	if err := tmp.Sync(); err != nil {
		return cleanup(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("write artifact: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("write artifact: %w", err)
	}
	return nil
}
