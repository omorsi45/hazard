package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/omorsi45/hazard/internal/config"
	"github.com/omorsi45/hazard/internal/control"
)

// driftStateAlarm is the DriftStatus.State of an alarm (see control.Monitor).
const driftStateAlarm = "alarm"

// auditsPath is the audit log of a repository: one JSON line per audited
// selection, appended by `hazard audit`.
func auditsPath(repoDir string) string {
	return filepath.Join(repoDir, ".hazard", "audits.jsonl")
}

// auditRecord is the part of an audits.jsonl line the drift monitor reads. The
// names are those of telemetry.SelectionAudit, which the engine cannot import
// (telemetry imports the engine).
type auditRecord struct {
	SelectionID       string   `json:"selection_id"`
	LockDigest        string   `json:"lock_digest"`
	Missed            []string `json:"missed"`
	RegressionBearing bool     `json:"regression_bearing"`
}

// readObservations reads the audit log into drift observations, oldest first.
// Only regression-bearing audits count: a selection audited against a run
// where nothing failed says nothing about a miss. The log is append-only and
// repeats the lines of a selection that is audited again, so the last line of
// a selection wins. A missing log is no observations. A truncated last line
// (no newline and not valid JSON, the mark of an interrupted append) is
// skipped with a warning; any other line that cannot be read is an error,
// because the history can then no longer be trusted.
func readObservations(path string) (obs []control.Observation, warnings []string, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read audit log: %w", err)
	}
	var entries []auditRecord
	last := map[string]int{} // selection ID -> index in entries of its last line
	lines := bytes.Split(data, []byte("\n"))
	for i, raw := range lines {
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 {
			continue
		}
		rec, err := decodeAudit(raw)
		if err != nil {
			if i == len(lines)-1 && !json.Valid(raw) {
				warnings = append(warnings, fmt.Sprintf("audit log %s: the last line is truncated and was skipped", path))
				continue
			}
			return nil, nil, fmt.Errorf("audit log %s line %d: %w", path, i+1, err)
		}
		last[rec.SelectionID] = len(entries)
		entries = append(entries, rec)
	}
	for i, rec := range entries {
		if last[rec.SelectionID] != i || !rec.RegressionBearing {
			continue
		}
		obs = append(obs, control.Observation{
			SelectionID: rec.SelectionID,
			LockDigest:  rec.LockDigest,
			Missed:      len(rec.Missed) > 0,
		})
	}
	return obs, warnings, nil
}

// decodeAudit decodes one audit line and requires a selection ID.
func decodeAudit(raw []byte) (auditRecord, error) {
	var rec auditRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return auditRecord{}, err
	}
	if rec.SelectionID == "" {
		return auditRecord{}, errors.New("no selection_id")
	}
	return rec, nil
}

// driftFor judges the field miss rate of the selections made under lock l
// from the audit log of repoDir. Drift that cannot be judged (an unreadable
// log) is an alarm, never a quiet pass: unknown never narrows, whatever
// control.drift.escalate_if_ucb_above_alpha says. That flag governs only the
// escalation of an upper bound above alpha: with it off the status of a bound
// is kept for the warnings but Alarm is cleared.
func driftFor(repoDir string, cfg config.Config, l control.Lock) (control.DriftStatus, []string) {
	obs, warnings, err := readObservations(auditsPath(repoDir))
	if err != nil {
		warnings = append(warnings, "drift_unknown: "+err.Error())
		return control.DriftStatus{State: driftStateAlarm, UCB: 1, Alarm: true}, warnings
	}
	status := control.Monitor(obs, l.Digest, cfg.Control.Drift.WindowRuns, l.Alpha, l.Delta)
	if status.Alarm {
		warnings = append(warnings, fmt.Sprintf("drift: alarm after %d audited selections with %d misses (upper bound %.4f, alpha %g)",
			status.N, status.Misses, status.UCB, l.Alpha))
		if !cfg.Control.Drift.EscalateIfUCBAboveAlpha {
			status.Alarm = false
			warnings = append(warnings, "drift: not escalated because control.drift.escalate_if_ucb_above_alpha is false")
		}
	}
	return status, warnings
}
