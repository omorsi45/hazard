package control

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
	"strconv"
)

// lockVersion is the only lock schema version this package reads or writes.
const lockVersion = 1

// Reasons returned by Lock.Verify.
const (
	reasonLockCorrupt     = "lock_corrupt"
	reasonModelDrift      = "model_drift"
	reasonRubricMismatch  = "rubric_mismatch"
	reasonFeatureMismatch = "feature_mismatch"
	reasonPolicyMismatch  = "policy_mismatch"
)

// The L1 modes a lock may name: the mode the deterministic layer ran in when
// the threshold was calibrated, and the only mode a selection under that lock
// may use.
const (
	L1ModeGraph    = "graph"
	L1ModeCoverage = "coverage"
)

// Lock is the calibration lock (seed 7.1) plus the digests that bind it to the
// feature weights and selection policy, the L1 mode it was calibrated in, and
// the calibration commits behind it. Digest covers every other field.
type Lock struct {
	Version           int                `json:"version"`
	CreatedAt         string             `json:"created_at"`
	Alpha             float64            `json:"alpha"`
	Delta             float64            `json:"delta"`
	Unit              string             `json:"unit"`
	Lambda            float64            `json:"lambda"`
	Predictor         string             `json:"predictor"`
	ModelVersion      string             `json:"model_version"`
	RubricVersion     string             `json:"rubric_version"`
	FeatureVersion    string             `json:"feature_version"`
	WeightsDigest     string             `json:"weights_digest"`
	PolicyDigest      string             `json:"policy_digest"`
	L1Mode            string             `json:"l1_mode"`
	NCal              int                `json:"n_cal"`
	ReplayFailures    int                `json:"replay_failures"` // units among NCal whose replay failed and that count as misses
	EmpiricalMissRate float64            `json:"empirical_miss_rate"`
	Split             string             `json:"split"`
	SplitAt           string             `json:"split_at"`
	Grid              []float64          `json:"grid"`
	PValues           map[string]float64 `json:"p_values"` // keyed by PValueKey(lambda)
	CalibrationIDs    []string           `json:"calibration_ids"`
	Digest            string             `json:"digest"`
}

// Identity is what the running system reports and Verify compares to the lock.
type Identity struct {
	Predictor      string
	ModelRelease   string // the release the predictor actually reports, optionally with a -YYYYMMDD snapshot suffix
	RubricVersion  string
	FeatureVersion string
	WeightsDigest  string
	PolicyDigest   string
}

// PValueKey is the Lock.PValues key for a lambda: the shortest decimal that
// round-trips (strconv 'g', precision -1).
func PValueKey(lambda float64) string {
	return strconv.FormatFloat(lambda, 'g', -1, 64)
}

// ComputeDigest returns "sha256:" followed by the hex SHA-256 of the compact
// JSON of the lock with Digest set to "". Struct fields marshal in
// declaration order and map keys sorted, so the bytes are deterministic. If
// the lock cannot be marshaled (a NaN or infinite number) it returns "",
// which never matches a stored digest.
func (l Lock) ComputeDigest() string {
	l.Digest = ""
	b, err := json.Marshal(l)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// validate reports why l must not be written as a lock, or nil.
func (l Lock) validate() error {
	switch {
	case l.Version != lockVersion:
		return fmt.Errorf("version %d, want %d", l.Version, lockVersion)
	case !(l.Alpha > 0 && l.Alpha < 1):
		return fmt.Errorf("alpha %v outside (0, 1)", l.Alpha)
	case !(l.Delta > 0 && l.Delta < 1):
		return fmt.Errorf("delta %v outside (0, 1)", l.Delta)
	case !(l.Lambda > 0 && l.Lambda < 1):
		return fmt.Errorf("lambda %v outside (0, 1)", l.Lambda)
	case l.Unit == "":
		return errors.New("empty unit")
	case l.Predictor == "":
		return errors.New("empty predictor")
	case l.ModelVersion == "":
		return errors.New("empty model_version")
	case l.RubricVersion == "":
		return errors.New("empty rubric_version")
	case l.FeatureVersion == "":
		return errors.New("empty feature_version")
	case l.L1Mode != L1ModeGraph && l.L1Mode != L1ModeCoverage:
		return fmt.Errorf("l1_mode %q, want %q or %q", l.L1Mode, L1ModeGraph, L1ModeCoverage)
	case l.NCal <= 0:
		return fmt.Errorf("n_cal %d, want > 0", l.NCal)
	case l.ReplayFailures < 0 || l.ReplayFailures > l.NCal:
		return fmt.Errorf("replay_failures %d outside [0, n_cal %d]", l.ReplayFailures, l.NCal)
	case !(l.EmpiricalMissRate >= 0 && l.EmpiricalMissRate <= 1):
		return fmt.Errorf("empirical_miss_rate %v outside [0, 1]", l.EmpiricalMissRate)
	}
	if len(l.Grid) > 0 && !strictlyAscending(l.Grid) {
		return errors.New("grid is not finite and strictly ascending")
	}
	for key, p := range l.PValues {
		lambda, err := strconv.ParseFloat(key, 64)
		if err != nil || PValueKey(lambda) != key {
			return fmt.Errorf("p_values key %q is not a canonical lambda", key)
		}
		if !(p >= 0 && p <= 1) {
			return fmt.Errorf("p_values[%q] = %v outside [0, 1]", key, p)
		}
	}
	return nil
}

// ReadLock parses the lock at path strictly: unknown fields, malformed or
// trailing JSON, and a non-object document are errors. It does not check the
// digest or the identity: a lock whose digest is wrong is returned as parsed
// so the controller can record why through Verify.
func ReadLock(path string) (Lock, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Lock{}, err
	}
	if trimmed := bytes.TrimSpace(data); len(trimmed) == 0 || trimmed[0] != '{' {
		return Lock{}, fmt.Errorf("lock %s: not a JSON object", path)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var l Lock
	if err := dec.Decode(&l); err != nil {
		return Lock{}, fmt.Errorf("lock %s: %w", path, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Lock{}, fmt.Errorf("lock %s: data after the JSON object", path)
	}
	return l, nil
}

// WriteLock validates l, sets its Digest and writes it to path atomically:
// the bytes go to a temporary file in the same directory, are synced, and
// replace path by rename. An invalid lock is refused before anything is
// written, and a failed write leaves any existing lock untouched.
func WriteLock(path string, l Lock) error {
	if err := l.validate(); err != nil {
		return fmt.Errorf("refusing to write an invalid lock: %w", err)
	}
	l.Digest = l.ComputeDigest()
	if l.Digest == "" {
		return errors.New("refusing to write a lock that cannot be digested")
	}
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return fmt.Errorf("encode lock: %w", err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(path), ".hazard-lock-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary lock: %w", err)
	}
	tmpName := tmp.Name()
	if err := writeSyncClose(tmp, data); err != nil {
		return removeAfter(tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return removeAfter(tmpName, err)
	}
	return nil
}

// writeSyncClose writes data to f, makes the file world-readable like a
// committed file, syncs it to disk and closes it. f is closed on every path.
func writeSyncClose(f *os.File, data []byte) error {
	_, err := f.Write(data)
	if err == nil {
		err = f.Chmod(0o644)
	}
	if err == nil {
		err = f.Sync()
	}
	return errors.Join(err, f.Close())
}

// removeAfter deletes the temporary file left by a failed write and returns
// cause, joined with any failure to delete it.
func removeAfter(name string, cause error) error {
	if rmErr := os.Remove(name); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
		return errors.Join(cause, rmErr)
	}
	return cause
}

// Verify compares the lock to the running system's identity and returns "" when
// the guarantee transfers, else the first reason it does not, in this order:
// lock_corrupt (the digest does not match the content), model_drift (the
// model release moved), rubric_mismatch, feature_mismatch (feature version or
// weights digest) and policy_mismatch (predictor or policy digest).
func (l Lock) Verify(id Identity) (reason string) {
	if digest := l.ComputeDigest(); digest == "" || l.Digest != digest {
		return reasonLockCorrupt
	}
	if modelRelease(l.ModelVersion) != modelRelease(id.ModelRelease) {
		return reasonModelDrift
	}
	if l.RubricVersion != id.RubricVersion {
		return reasonRubricMismatch
	}
	if l.FeatureVersion != id.FeatureVersion || l.WeightsDigest != id.WeightsDigest {
		return reasonFeatureMismatch
	}
	if l.Predictor != id.Predictor || l.PolicyDigest != id.PolicyDigest {
		return reasonPolicyMismatch
	}
	return ""
}

// modelRelease strips one trailing "-YYYYMMDD" snapshot suffix, so a dated
// snapshot compares equal to its release and to other snapshots of it.
func modelRelease(s string) string {
	const suffix = len("-20060102")
	if len(s) <= suffix || s[len(s)-suffix] != '-' {
		return s
	}
	for _, c := range []byte(s[len(s)-suffix+1:]) {
		if c < '0' || c > '9' {
			return s
		}
	}
	return s[:len(s)-suffix]
}
