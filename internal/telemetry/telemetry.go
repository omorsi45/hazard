// Package telemetry records one line per selection or run and audits those
// records against a full run. A record holds test IDs, probabilities and
// timings only: never payload bytes, hunk text, test purposes or assertions.
package telemetry

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/omorsi45/hazard/internal/engine"
)

// Modes of a Record.
const (
	ModeSelect = "select"
	ModeRun    = "run"
	ModeShadow = "shadow"
)

// Member is one test of a selection.
type Member struct {
	TestID string   `json:"test_id"`
	Source string   `json:"source"`
	P      *float64 `json:"p,omitempty"`
}

// Record is one telemetry line. For a full-suite decision Members is the
// hypothetical selection: what narrowing would have chosen.
type Record struct {
	ID            string   `json:"id"`
	Time          string   `json:"time"` // UTC, RFC 3339
	Commit        string   `json:"commit"`
	Mode          string   `json:"mode"`
	FullSuite     bool     `json:"full_suite"`
	AbstainReason string   `json:"abstain_reason,omitempty"`
	Lambda        float64  `json:"lambda"`
	LockDigest    string   `json:"lock_digest"`
	NTotal        int      `json:"n_total"`
	NCertain      int      `json:"n_certain"`
	NAdded        int      `json:"n_added"`
	NAlwaysRun    int      `json:"n_always_run"`
	NTier         int      `json:"n_tier"`
	Members       []Member `json:"members"`
	EstMSSelected int      `json:"est_ms_selected"`
	EstMSFull     int      `json:"est_ms_full"`
	OverheadMS    int      `json:"overhead_ms"`
	CostUSD       float64  `json:"cost_usd"`
	RunnerExit    *int     `json:"runner_exit,omitempty"` // run mode only
	JUnit         string   `json:"junit,omitempty"`
	HazardVersion string   `json:"hazard_version"`
}

// NewID returns 16 random bytes as hexadecimal, the form of a decision ID.
func NewID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("telemetry: generate ID: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// FromDecision builds the record of d. It copies a fixed set of fields, so
// nothing else of the decision (payload, hunk text, purposes, assertions,
// warnings, reasons) can reach the file.
func FromDecision(d engine.Decision, mode, version string, at time.Time) Record {
	picked := d.Tests
	if d.FullSuite {
		picked = d.Hypothetical
	}
	r := Record{
		ID:            d.ID,
		Time:          at.UTC().Format(time.RFC3339),
		Mode:          mode,
		FullSuite:     d.FullSuite,
		AbstainReason: d.AbstainReason,
		Lambda:        d.Lambda,
		LockDigest:    d.LockDigest,
		NTotal:        len(d.AllTests),
		Members:       make([]Member, 0, len(picked)),
		EstMSSelected: d.EstMSSelected,
		EstMSFull:     d.EstMSFull,
		OverheadMS:    d.OverheadMS,
		CostUSD:       d.CostUSD,
		HazardVersion: version,
	}
	if d.Change != nil {
		r.Commit = d.Change.HeadSHA
	}
	for _, s := range picked {
		r.Members = append(r.Members, Member{TestID: s.TestID, Source: s.Source, P: s.P})
		switch s.Source {
		case "certain":
			r.NCertain++
		case "added":
			r.NAdded++
		case "always_run":
			r.NAlwaysRun++
		case "tier":
			r.NTier++
		}
	}
	return r
}

// marshalLine is the newline-terminated JSON of r.
func marshalLine(r Record) ([]byte, error) {
	data, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("telemetry: encode record %s: %w", r.ID, err)
	}
	return append(data, '\n'), nil
}

// Append writes r as one JSON line to path, creating the directory and the
// file, and syncs it. A previous crash can leave an incomplete last line:
// Append drops that fragment first, because Read would skip it anyway and a
// new record must not be glued to it.
func Append(path string, r Record) (err error) {
	line, err := marshalLine(r)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("telemetry: create directory: %w", err)
	}
	prefix, err := repairTail(path)
	if err != nil {
		return fmt.Errorf("telemetry: inspect %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return fmt.Errorf("telemetry: open %s: %w", path, err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("telemetry: close %s: %w", path, cerr))
		}
	}()
	if _, err := f.Write(append(prefix, line...)); err != nil {
		return fmt.Errorf("telemetry: write %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("telemetry: sync %s: %w", path, err)
	}
	return nil
}

// repairTail makes the end of the file at path safe to append to. A missing
// or empty file, or one that ends in a newline, needs nothing. A complete last
// record without its newline gets one (returned as the prefix to write). An
// incomplete last record is truncated away. The file is opened without
// O_APPEND here because Windows refuses to truncate an append-only handle.
func repairTail(path string) (prefix []byte, err error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			err = errors.Join(err, cerr)
		}
	}()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() == 0 {
		return nil, nil
	}
	last := make([]byte, 1)
	if _, err := f.ReadAt(last, info.Size()-1); err != nil {
		return nil, err
	}
	if last[0] == '\n' {
		return nil, nil
	}
	data := make([]byte, info.Size())
	if _, err := f.ReadAt(data, 0); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	start := bytes.LastIndexByte(data, '\n') + 1
	if json.Valid(data[start:]) {
		return []byte("\n"), nil
	}
	if err := f.Truncate(int64(start)); err != nil {
		return nil, err
	}
	return nil, nil
}

// Read returns the records of path in file order. A truncated last line is
// skipped; use Load to learn that it happened.
func Read(path string) ([]Record, error) {
	records, _, err := Load(path)
	return records, err
}

// Load is Read plus the number of truncated last lines it skipped (0 or 1).
// A last line is truncated when it has no newline and is not valid JSON; any
// other malformed line is an error that names the line.
func Load(path string) ([]Record, int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	var records []Record
	skipped := 0
	lines := bytes.Split(data, []byte("\n"))
	for i, raw := range lines {
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 {
			continue
		}
		var r Record
		err := json.Unmarshal(raw, &r)
		if err == nil && r.ID == "" {
			err = errors.New("record has no id")
		}
		if err != nil {
			if i == len(lines)-1 && !json.Valid(raw) {
				skipped++
				continue
			}
			return nil, 0, fmt.Errorf("telemetry: %s line %d: %w", path, i+1, err)
		}
		records = append(records, r)
	}
	return records, skipped, nil
}
