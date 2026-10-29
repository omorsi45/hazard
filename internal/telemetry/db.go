package telemetry

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/omorsi45/hazard/internal/store"
)

// Write appends r to the JSONL file at jsonlPath and, when the database file
// at dbPath already exists, mirrors it into the selections tables. The
// database is never created here. Both sinks are tried and every failure is
// returned, joined: a caller reports it as a warning and never lets it change
// an exit code.
func Write(jsonlPath, dbPath string, r Record) error {
	err := Append(jsonlPath, r)
	if _, statErr := os.Stat(dbPath); statErr != nil {
		if errors.Is(statErr, fs.ErrNotExist) {
			return err
		}
		return errors.Join(err, fmt.Errorf("telemetry: inspect database %s: %w", dbPath, statErr))
	}
	return errors.Join(err, mirror(dbPath, r))
}

func mirror(dbPath string, r Record) (err error) {
	s, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("telemetry: open database: %w", err)
	}
	defer func() {
		if cerr := s.Close(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("telemetry: close database: %w", cerr))
		}
	}()
	return insertSelection(s.DB(), r)
}

// insertSelection stores r in one transaction. Rows of a full-suite decision
// have no members: the full suite is everything, and the JSONL line keeps the
// hypothetical selection.
func insertSelection(db *sql.DB, r Record) (err error) {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("telemetry: begin: %w", err)
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("telemetry: rollback: %w", rbErr))
		}
	}()
	var reason any
	if r.AbstainReason != "" {
		reason = r.AbstainReason
	}
	// The table has no always-run column; that tier is part of the
	// deterministic floor, like certain.
	if _, err := tx.Exec(`INSERT INTO selections (id, commit_sha, created_at, mode, tier, abstained, abstain_reason, lambda, lock_digest,
		n_total, n_certain, n_added, est_ms_sel, est_ms_full, overhead_ms, jev_cost_usd) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.Commit, r.Time, r.Mode, tierOf(r), b2i(r.FullSuite), reason, r.Lambda, r.LockDigest,
		r.NTotal, r.NCertain+r.NAlwaysRun, r.NAdded, r.EstMSSelected, r.EstMSFull, r.OverheadMS, r.CostUSD); err != nil {
		return fmt.Errorf("telemetry: insert selection %s: %w", r.ID, err)
	}
	if !r.FullSuite {
		for _, m := range r.Members {
			source := m.Source
			if source == "always_run" {
				source = "certain"
			}
			if _, err := tx.Exec(`INSERT OR IGNORE INTO selection_members (selection_id, test_id, source, p_break) VALUES (?, ?, ?, ?)`,
				r.ID, m.TestID, source, m.P); err != nil {
				return fmt.Errorf("telemetry: insert member %s of %s: %w", m.TestID, r.ID, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("telemetry: commit selection %s: %w", r.ID, err)
	}
	return nil
}

func tierOf(r Record) string {
	switch {
	case r.FullSuite:
		return "suite"
	case r.NTier > 0:
		return "module"
	}
	return "targeted"
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
