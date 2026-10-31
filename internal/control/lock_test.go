package control

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// testLock returns a valid lock (without a digest), modelled on seed 7.1.
func testLock() Lock {
	return Lock{
		Version:           1,
		CreatedAt:         "2026-10-14T18:22:04Z",
		Alpha:             0.01,
		Delta:             0.05,
		Unit:              "commit",
		Lambda:            0.035,
		Predictor:         "ensemble",
		ModelVersion:      "typesafe/jev-1.13",
		RubricVersion:     "r3",
		FeatureVersion:    "f2",
		WeightsDigest:     "sha256:w",
		PolicyDigest:      "sha256:p",
		L1Mode:            "graph",
		NCal:              1184,
		ReplayFailures:    2,
		EmpiricalMissRate: 0.0034,
		Split:             "temporal",
		SplitAt:           "2026-06-01",
		Grid:              []float64{0.005, 0.01, 0.035},
		PValues:           map[string]float64{"0.005": 0.00012, "0.01": 0.0021, "0.035": 0.043},
		CalibrationIDs:    []string{"a", "b"},
	}
}

// signedLock is testLock with its digest set.
func signedLock() Lock {
	l := testLock()
	l.Digest = l.ComputeDigest()
	return l
}

func testIdentity() Identity {
	return Identity{
		Predictor:      "ensemble",
		ModelRelease:   "typesafe/jev-1.13",
		RubricVersion:  "r3",
		FeatureVersion: "f2",
		WeightsDigest:  "sha256:w",
		PolicyDigest:   "sha256:p",
	}
}

func TestPValueKeyUsesShortestRoundTripFormat(t *testing.T) {
	tests := map[float64]string{0.035: "0.035", 0.1: "0.1", 0.005: "0.005", 0.5: "0.5", 1e-7: "1e-07", 0.3: "0.3"}
	for in, want := range tests {
		if got := PValueKey(in); got != want {
			t.Errorf("PValueKey(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestComputeDigestGolden(t *testing.T) {
	// The digest is sha256 over the compact JSON of the lock with digest "".
	// The expected string and hash were produced independently with Python's
	// hashlib, so a renamed field, a reordered field or a changed number
	// format fails here.
	const wantJSON = `{"version":1,"created_at":"2026-10-14T18:22:04Z","alpha":0.01,"delta":0.05,"unit":"commit","lambda":0.035,"predictor":"ensemble","model_version":"typesafe/jev-1.13","rubric_version":"r3","feature_version":"f2","weights_digest":"sha256:w","policy_digest":"sha256:p","l1_mode":"graph","n_cal":1184,"replay_failures":2,"empirical_miss_rate":0.0034,"split":"temporal","split_at":"2026-06-01","grid":[0.005,0.01,0.035],"p_values":{"0.005":0.00012,"0.01":0.0021,"0.035":0.043},"calibration_ids":["a","b"],"digest":""}`
	const wantDigest = "sha256:7eeedf927b1bb13cb870ef6d6e5a2b154c9b203c8b3d7964ae9745f6ff388d77"

	l := testLock()
	got, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != wantJSON {
		t.Errorf("canonical JSON =\n%s\nwant\n%s", got, wantJSON)
	}
	if d := l.ComputeDigest(); d != wantDigest {
		t.Errorf("ComputeDigest = %s, want %s", d, wantDigest)
	}
}

func TestComputeDigestIgnoresDigestFieldAndCoversEveryOtherField(t *testing.T) {
	base := testLock()
	want := base.ComputeDigest()
	if !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(want) {
		t.Fatalf("digest %q is not sha256:<64 hex>", want)
	}
	withDigest := base
	withDigest.Digest = "sha256:anything"
	if got := withDigest.ComputeDigest(); got != want {
		t.Errorf("Digest field leaked into the digest: %s != %s", got, want)
	}

	mutations := map[string]func(*Lock){
		"Version":           func(l *Lock) { l.Version = 2 },
		"CreatedAt":         func(l *Lock) { l.CreatedAt = "2026-10-15T00:00:00Z" },
		"Alpha":             func(l *Lock) { l.Alpha = 0.02 },
		"Delta":             func(l *Lock) { l.Delta = 0.1 },
		"Unit":              func(l *Lock) { l.Unit = "test" },
		"Lambda":            func(l *Lock) { l.Lambda = 0.01 },
		"Predictor":         func(l *Lock) { l.Predictor = "logistic" },
		"ModelVersion":      func(l *Lock) { l.ModelVersion = "typesafe/jev-1.14" },
		"RubricVersion":     func(l *Lock) { l.RubricVersion = "r4" },
		"FeatureVersion":    func(l *Lock) { l.FeatureVersion = "f3" },
		"WeightsDigest":     func(l *Lock) { l.WeightsDigest = "sha256:x" },
		"PolicyDigest":      func(l *Lock) { l.PolicyDigest = "sha256:y" },
		"L1Mode":            func(l *Lock) { l.L1Mode = "coverage" },
		"NCal":              func(l *Lock) { l.NCal = 1185 },
		"ReplayFailures":    func(l *Lock) { l.ReplayFailures = 3 },
		"EmpiricalMissRate": func(l *Lock) { l.EmpiricalMissRate = 0.0035 },
		"Split":             func(l *Lock) { l.Split = "random" },
		"SplitAt":           func(l *Lock) { l.SplitAt = "2026-07-01" },
		"Grid":              func(l *Lock) { l.Grid = []float64{0.005, 0.01, 0.036} },
		"PValues":           func(l *Lock) { l.PValues = map[string]float64{"0.005": 0.00012, "0.01": 0.0021, "0.035": 0.044} },
		"PValuesKey":        func(l *Lock) { l.PValues = map[string]float64{"0.005": 0.00012, "0.01": 0.0021, "0.036": 0.043} },
		"CalibrationIDs":    func(l *Lock) { l.CalibrationIDs = []string{"a", "c"} },
	}
	for name, mutate := range mutations {
		l := testLock()
		mutate(&l)
		if got := l.ComputeDigest(); got == want {
			t.Errorf("changing %s did not change the digest", name)
		}
	}
}

func TestComputeDigestIsStableAcrossMapOrderAndRoundTrip(t *testing.T) {
	l := testLock()
	want := l.ComputeDigest()
	for i := 0; i < 20; i++ {
		if got := l.ComputeDigest(); got != want {
			t.Fatalf("digest not deterministic: %s != %s", got, want)
		}
	}
	// A nil slice and map survive a write/read cycle with the same digest.
	empty := testLock()
	empty.Grid, empty.PValues, empty.CalibrationIDs = nil, nil, nil
	path := filepath.Join(t.TempDir(), "hazard.lock")
	if err := WriteLock(path, empty); err != nil {
		t.Fatal(err)
	}
	got, err := ReadLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Digest != empty.ComputeDigest() || got.ComputeDigest() != got.Digest {
		t.Errorf("digest changed across a write/read round trip: stored %s, recomputed %s, original %s", got.Digest, got.ComputeDigest(), empty.ComputeDigest())
	}
}

func TestWriteLockSetsDigestAndRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hazard.lock")
	in := testLock()
	in.Digest = "sha256:stale" // WriteLock must overwrite whatever the caller passed
	if err := WriteLock(path, in); err != nil {
		t.Fatalf("WriteLock: %v", err)
	}
	got, err := ReadLock(path)
	if err != nil {
		t.Fatalf("ReadLock: %v", err)
	}
	want := testLock()
	want.Digest = want.ComputeDigest()
	wantJSON, _ := json.Marshal(want)
	gotJSON, _ := json.Marshal(got)
	if !bytes.Equal(gotJSON, wantJSON) {
		t.Errorf("round trip changed the lock:\n got %s\nwant %s", gotJSON, wantJSON)
	}
	if reason := got.Verify(testIdentity()); reason != "" {
		t.Errorf("Verify of a freshly written lock = %q, want \"\"", reason)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.HasPrefix(raw, []byte{0xEF, 0xBB, 0xBF}) || bytes.Contains(raw, []byte("\r")) || !bytes.HasSuffix(raw, []byte("\n")) {
		t.Errorf("lock file must be BOM-free LF text ending in a newline: %q", raw[:min(len(raw), 40)])
	}
}

func TestWriteLockReplacesAtomicallyAndLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hazard.lock")
	if err := WriteLock(path, testLock()); err != nil {
		t.Fatal(err)
	}
	second := testLock()
	second.Lambda = 0.01
	if err := WriteLock(path, second); err != nil {
		t.Fatal(err)
	}
	got, err := ReadLock(path)
	if err != nil || got.Lambda != 0.01 {
		t.Fatalf("overwrite not applied: lambda %v, err %v", got.Lambda, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "hazard.lock" {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("directory holds %v, want only hazard.lock", names)
	}
}

func TestWriteLockFailureKeepsTheExistingLockAndCleansUp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hazard.lock")
	if err := WriteLock(path, testLock()); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	bad := testLock()
	bad.Lambda = math.NaN()
	if err := WriteLock(path, bad); err == nil {
		t.Fatal("WriteLock accepted an invalid lock")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Errorf("an invalid lock overwrote the existing one (err %v)", err)
	}

	// A rename that cannot succeed (the target is a directory) must clean up.
	blocked := filepath.Join(dir, "blocked")
	if err := os.Mkdir(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteLock(blocked, testLock()); err == nil {
		t.Error("WriteLock onto a directory succeeded")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "hazard.lock" && e.Name() != "blocked" {
			t.Errorf("stray file left behind: %s", e.Name())
		}
	}
}

func TestWriteLockRefusesInvalidLocks(t *testing.T) {
	nan := math.NaN()
	tests := map[string]func(*Lock){
		"version zero":          func(l *Lock) { l.Version = 0 },
		"version two":           func(l *Lock) { l.Version = 2 },
		"alpha zero":            func(l *Lock) { l.Alpha = 0 },
		"alpha one":             func(l *Lock) { l.Alpha = 1 },
		"alpha NaN":             func(l *Lock) { l.Alpha = nan },
		"delta zero":            func(l *Lock) { l.Delta = 0 },
		"delta one":             func(l *Lock) { l.Delta = 1 },
		"lambda zero":           func(l *Lock) { l.Lambda = 0 },
		"lambda one":            func(l *Lock) { l.Lambda = 1 },
		"lambda NaN":            func(l *Lock) { l.Lambda = nan },
		"lambda inf":            func(l *Lock) { l.Lambda = math.Inf(1) },
		"empty unit":            func(l *Lock) { l.Unit = "" },
		"empty predictor":       func(l *Lock) { l.Predictor = "" },
		"empty model version":   func(l *Lock) { l.ModelVersion = "" },
		"empty rubric version":  func(l *Lock) { l.RubricVersion = "" },
		"empty feature version": func(l *Lock) { l.FeatureVersion = "" },
		"empty l1 mode":         func(l *Lock) { l.L1Mode = "" },
		"unknown l1 mode":       func(l *Lock) { l.L1Mode = "hybrid" },
		"zero n_cal":            func(l *Lock) { l.NCal = 0 },
		"negative failures":     func(l *Lock) { l.ReplayFailures = -1 },
		"failures above n_cal":  func(l *Lock) { l.ReplayFailures = l.NCal + 1 },
		"negative miss rate":    func(l *Lock) { l.EmpiricalMissRate = -0.1 },
		"miss rate above one":   func(l *Lock) { l.EmpiricalMissRate = 1.1 },
		"miss rate NaN":         func(l *Lock) { l.EmpiricalMissRate = nan },
		"descending grid":       func(l *Lock) { l.Grid = []float64{0.02, 0.01} },
		"duplicate grid":        func(l *Lock) { l.Grid = []float64{0.01, 0.01} },
		"NaN grid":              func(l *Lock) { l.Grid = []float64{0.01, nan} },
		"p-value NaN":           func(l *Lock) { l.PValues = map[string]float64{"0.01": nan} },
		"p-value above one":     func(l *Lock) { l.PValues = map[string]float64{"0.01": 1.5} },
		"p-value negative":      func(l *Lock) { l.PValues = map[string]float64{"0.01": -0.5} },
		"p-value key not float": func(l *Lock) { l.PValues = map[string]float64{"abc": 0.5} },
		"p-value key not canon": func(l *Lock) { l.PValues = map[string]float64{"0.0100": 0.5} },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "hazard.lock")
			l := testLock()
			mutate(&l)
			if err := WriteLock(path, l); err == nil {
				t.Fatal("WriteLock accepted the invalid lock")
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("an invalid lock left a file behind (stat err %v)", err)
			}
		})
	}
}

func TestReadLockIsStrict(t *testing.T) {
	good, err := json.MarshalIndent(signedLock(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	bad := map[string][]byte{
		"unknown field":  bytes.Replace(good, []byte(`"unit"`), []byte(`"extra": 1,`+"\n  "+`"unit"`), 1),
		"malformed":      good[:len(good)/2],
		"empty":          nil,
		"null":           []byte("null"),
		"array":          []byte("[]"),
		"trailing data":  append(append([]byte(nil), good...), []byte(`{"version":1}`)...),
		"trailing text":  append(append([]byte(nil), good...), []byte("garbage")...),
		"wrong type":     bytes.Replace(good, []byte(`"version": 1`), []byte(`"version": "1"`), 1),
		"fractional int": bytes.Replace(good, []byte(`"n_cal": 1184`), []byte(`"n_cal": 1184.5`), 1),
		"BOM":            append([]byte{0xEF, 0xBB, 0xBF}, good...),
	}
	for name, data := range bad {
		if got, err := ReadLock(write(strings.ReplaceAll(name, " ", "_"), data)); err == nil {
			t.Errorf("%s: ReadLock accepted it: %+v", name, got)
		}
	}

	if _, err := ReadLock(filepath.Join(dir, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing file: err = %v, want os.ErrNotExist", err)
	}

	// Trailing whitespace is fine.
	if _, err := ReadLock(write("ws", append(append([]byte(nil), good...), []byte("\n\n  \n")...))); err != nil {
		t.Errorf("trailing whitespace rejected: %v", err)
	}
}

func TestReadLockReturnsTheLockEvenWhenItsDigestIsWrong(t *testing.T) {
	l := signedLock()
	l.Digest = flipHex(l.Digest)
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "hazard.lock")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadLock(path)
	if err != nil {
		t.Fatalf("ReadLock must not fail on a bad digest: %v", err)
	}
	if got.Lambda != 0.035 || got.Digest != l.Digest {
		t.Errorf("parsed lock differs from the file: %+v", got)
	}
	if reason := got.Verify(testIdentity()); reason != "lock_corrupt" {
		t.Errorf("Verify = %q, want lock_corrupt", reason)
	}
}

// flipHex changes the last hex digit of a digest.
func flipHex(d string) string {
	last := d[len(d)-1]
	repl := byte('0')
	if last == '0' {
		repl = '1'
	}
	return d[:len(d)-1] + string(repl)
}

func TestVerifyAcceptsAMatchingIdentity(t *testing.T) {
	if reason := signedLock().Verify(testIdentity()); reason != "" {
		t.Errorf("Verify = %q, want \"\"", reason)
	}
}

// The four tests below are the lock-level guard proofs of seed 13.2: lock
// digest, model version, rubric version and feature version.

func TestVerifyDigestCorruption(t *testing.T) {
	good := signedLock()

	corrupt := good
	corrupt.Digest = flipHex(good.Digest)
	if reason := corrupt.Verify(testIdentity()); reason != "lock_corrupt" {
		t.Errorf("one hex digit flipped: Verify = %q, want lock_corrupt", reason)
	}

	empty := good
	empty.Digest = ""
	if reason := empty.Verify(testIdentity()); reason != "lock_corrupt" {
		t.Errorf("empty digest: Verify = %q, want lock_corrupt", reason)
	}

	// A field edited after signing: the stored digest no longer matches.
	edited := good
	edited.Lambda = 0.04
	if reason := edited.Verify(testIdentity()); reason != "lock_corrupt" {
		t.Errorf("edited lambda: Verify = %q, want lock_corrupt", reason)
	}
	edited = good
	edited.PValues = map[string]float64{"0.005": 0.00012, "0.01": 0.0021, "0.035": 0.0001}
	if reason := edited.Verify(testIdentity()); reason != "lock_corrupt" {
		t.Errorf("edited p-value: Verify = %q, want lock_corrupt", reason)
	}
}

func TestVerifyModelRelease(t *testing.T) {
	tests := []struct {
		name        string
		lockVersion string
		release     string
		want        string
	}{
		{"same release", "typesafe/jev-1.13", "typesafe/jev-1.13", ""},
		{"dated snapshot of the locked release", "typesafe/jev-1.13", "typesafe/jev-1.13-20261001", ""},
		{"locked snapshot, served undated", "typesafe/jev-1.13-20260901", "typesafe/jev-1.13", ""},
		{"different snapshot of the same release", "typesafe/jev-1.13-20260901", "typesafe/jev-1.13-20261001", ""},
		{"newer release", "typesafe/jev-1.13", "typesafe/jev-1.14", "model_drift"},
		{"newer release with snapshot", "typesafe/jev-1.13", "typesafe/jev-1.14-20261001", "model_drift"},
		{"older release", "typesafe/jev-1.13", "typesafe/jev-1.12", "model_drift"},
		{"release prefix only", "typesafe/jev-1.13", "typesafe/jev-1.1", "model_drift"},
		{"different vendor", "typesafe/jev-1.13", "other/jev-1.13", "model_drift"},
		{"empty release", "typesafe/jev-1.13", "", "model_drift"},
		{"short date is not a snapshot", "typesafe/jev-1.13", "typesafe/jev-1.13-2026100", "model_drift"},
		{"non-digit date is not a snapshot", "typesafe/jev-1.13", "typesafe/jev-1.13-2026100a", "model_drift"},
		{"only one suffix is stripped", "typesafe/jev-1.13", "typesafe/jev-1.13-20261001-20261002", "model_drift"},
		{"snapshot on a different release", "typesafe/jev-1.13-20260901", "typesafe/jev-1.14-20260901", "model_drift"},
	}
	for _, tc := range tests {
		l := testLock()
		l.ModelVersion = tc.lockVersion
		l.Digest = l.ComputeDigest()
		id := testIdentity()
		id.ModelRelease = tc.release
		if got := l.Verify(id); got != tc.want {
			t.Errorf("%s: lock %q vs served %q: Verify = %q, want %q", tc.name, tc.lockVersion, tc.release, got, tc.want)
		}
	}
}

func TestVerifyRubricVersion(t *testing.T) {
	// Bump the rubric version in the lock only (re-signed so the digest is
	// intact): the embedded rubric still says r3.
	l := testLock()
	l.RubricVersion = "r4"
	l.Digest = l.ComputeDigest()
	if got := l.Verify(testIdentity()); got != "rubric_mismatch" {
		t.Errorf("Verify = %q, want rubric_mismatch", got)
	}
}

func TestVerifyFeatureVersionAndWeights(t *testing.T) {
	good := signedLock()

	// Only the logistic weights hash in the identity changes.
	id := testIdentity()
	id.WeightsDigest = "sha256:other"
	if got := good.Verify(id); got != "feature_mismatch" {
		t.Errorf("weights digest changed: Verify = %q, want feature_mismatch", got)
	}

	id = testIdentity()
	id.FeatureVersion = "f3"
	if got := good.Verify(id); got != "feature_mismatch" {
		t.Errorf("feature version changed: Verify = %q, want feature_mismatch", got)
	}
}

func TestVerifyPolicy(t *testing.T) {
	good := signedLock()

	id := testIdentity()
	id.Predictor = "logistic"
	if got := good.Verify(id); got != "policy_mismatch" {
		t.Errorf("predictor changed: Verify = %q, want policy_mismatch", got)
	}

	id = testIdentity()
	id.PolicyDigest = "sha256:other"
	if got := good.Verify(id); got != "policy_mismatch" {
		t.Errorf("policy digest changed: Verify = %q, want policy_mismatch", got)
	}
}

func TestVerifyReportsTheMostFundamentalMismatchFirst(t *testing.T) {
	// Order: lock_corrupt, model_drift, rubric_mismatch, feature_mismatch, policy_mismatch.
	all := Identity{Predictor: "x", ModelRelease: "typesafe/jev-9", RubricVersion: "r9", FeatureVersion: "f9", WeightsDigest: "w9", PolicyDigest: "p9"}
	corrupt := signedLock()
	corrupt.Digest = flipHex(corrupt.Digest)
	if got := corrupt.Verify(all); got != "lock_corrupt" {
		t.Errorf("corrupt + everything else: %q, want lock_corrupt", got)
	}
	good := signedLock()
	if got := good.Verify(all); got != "model_drift" {
		t.Errorf("everything but corruption: %q, want model_drift", got)
	}
	all.ModelRelease = "typesafe/jev-1.13"
	if got := good.Verify(all); got != "rubric_mismatch" {
		t.Errorf("rubric+feature+policy: %q, want rubric_mismatch", got)
	}
	all.RubricVersion = "r3"
	if got := good.Verify(all); got != "feature_mismatch" {
		t.Errorf("feature+policy: %q, want feature_mismatch", got)
	}
	all.FeatureVersion, all.WeightsDigest = "f2", "sha256:w"
	if got := good.Verify(all); got != "policy_mismatch" {
		t.Errorf("policy only: %q, want policy_mismatch", got)
	}
}
