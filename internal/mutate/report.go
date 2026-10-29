package mutate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/omorsi45/hazard/internal/store"
)

// normalize returns r with nil lists replaced by empty ones, so they encode as
// [] rather than null.
func normalize(r Record) Record {
	if r.Killers == nil {
		r.Killers = []string{}
	}
	if r.UnreachableKillers == nil {
		r.UnreachableKillers = []string{}
	}
	if r.BeyondMaxHopsKillers == nil {
		r.BeyondMaxHopsKillers = []string{}
	}
	return r
}

// sortedRecords returns normalized copies of recs ordered by ID.
func sortedRecords(recs []Record) []Record {
	out := make([]Record, len(recs))
	for i, r := range recs {
		out[i] = normalize(r)
	}
	slices.SortFunc(out, func(a, b Record) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// summarize builds the Summary of a run from its records, ordered by ID.
func summarize(baseSHA string, candidates int, baselineSeconds float64, recs []Record) Summary {
	s := Summary{
		BaseSHA:         baseSHA,
		Candidates:      candidates,
		Executed:        len(recs),
		ByStatus:        map[string]int{},
		ByOperator:      map[string]int{},
		BaselineSeconds: baselineSeconds,
		Records:         sortedRecords(recs),
	}
	for _, r := range s.Records {
		s.ByStatus[r.Status]++
		s.ByOperator[r.Operator]++
		if r.Status != "killed" {
			continue
		}
		s.Usable++
		if len(r.UnreachableKillers) > 0 {
			s.UnreachableMutants++
			if len(r.UnreachableKillers) == len(r.Killers) {
				s.FullyUnreachableMutants++
			}
		}
		if len(r.BeyondMaxHopsKillers) > 0 {
			s.BeyondMaxHopsMutants++
		}
	}
	return s
}

// jsonlWriter writes records progressively to <out>.partial and publishes the
// final, ordered file atomically.
type jsonlWriter struct {
	out     string
	partial *os.File
	closed  bool
}

func newJSONLWriter(out string) (*jsonlWriter, error) {
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return nil, fmt.Errorf("mutate: create the output directory: %w", err)
	}
	f, err := os.OpenFile(out+".partial", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, fmt.Errorf("mutate: create %s.partial: %w", out, err)
	}
	return &jsonlWriter{out: out, partial: f}, nil
}

func encodeRecord(r Record) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(normalize(r)); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Append writes one record to the partial file.
func (w *jsonlWriter) Append(r Record) error {
	line, err := encodeRecord(r)
	if err != nil {
		return fmt.Errorf("mutate: encode record %s: %w", r.ID, err)
	}
	if _, err := w.partial.Write(line); err != nil {
		return fmt.Errorf("mutate: write %s.partial: %w", w.out, err)
	}
	return nil
}

// Commit writes recs ordered by ID to a temporary file, renames it onto the
// output path and removes the partial file.
func (w *jsonlWriter) Commit(recs []Record) (err error) {
	if err := w.Close(); err != nil {
		return err
	}
	tmp := w.out + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("mutate: create %s: %w", tmp, err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, os.Remove(tmp))
		}
	}()
	for _, r := range sortedRecords(recs) {
		line, encErr := encodeRecord(r)
		if encErr == nil {
			_, encErr = f.Write(line)
		}
		if encErr != nil {
			return errors.Join(fmt.Errorf("mutate: write %s: %w", tmp, encErr), f.Close())
		}
	}
	if err := f.Sync(); err != nil {
		return errors.Join(fmt.Errorf("mutate: sync %s: %w", tmp, err), f.Close())
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("mutate: close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, w.out); err != nil {
		return fmt.Errorf("mutate: publish %s: %w", w.out, err)
	}
	if err := os.Remove(w.out + ".partial"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("mutate: remove %s.partial: %w", w.out, err)
	}
	return nil
}

// Close closes the partial file and leaves it on disk as evidence.
func (w *jsonlWriter) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	return w.partial.Close()
}

// persistMutants upserts mutants and their kills in one transaction. An
// unconfirmed mutant is not a label and is never stored; the schema forbids
// that status.
func persistMutants(ctx context.Context, dbPath string, recs []Record, now time.Time) (err error) {
	st, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("mutate: open the database: %w", err)
	}
	defer func() {
		if closeErr := st.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("mutate: close the database: %w", closeErr))
		}
	}()
	tx, err := st.DB().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("mutate: begin transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			if rbErr := tx.Rollback(); rbErr != nil {
				err = errors.Join(err, fmt.Errorf("mutate: rollback: %w", rbErr))
			}
		}
	}()
	created := now.UTC().Format("2006-01-02T15:04:05Z")
	for _, r := range sortedRecords(recs) {
		if r.Status == "unconfirmed" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO mutants(id,file,line,operator,original,mutated,base_sha,status,created_at)
			VALUES(?,?,?,?,?,?,?,?,?)
			ON CONFLICT(id) DO UPDATE SET file=excluded.file,line=excluded.line,operator=excluded.operator,
				original=excluded.original,mutated=excluded.mutated,base_sha=excluded.base_sha,
				status=excluded.status,created_at=excluded.created_at`,
			r.ID, r.File, r.Line, r.Operator, r.Original, r.Mutated, r.BaseSHA, r.Status, created); err != nil {
			return fmt.Errorf("mutate: store mutant %s: %w", r.ID, err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM mutant_kills WHERE mutant_id = ?`, r.ID); err != nil {
			return fmt.Errorf("mutate: clear kills of %s: %w", r.ID, err)
		}
		if r.Status != "killed" {
			continue
		}
		for _, test := range r.Killers {
			if _, err := tx.ExecContext(ctx, `INSERT INTO mutant_kills(mutant_id,test_id) VALUES(?,?) ON CONFLICT DO NOTHING`, r.ID, test); err != nil {
				return fmt.Errorf("mutate: store kill %s of %s: %w", test, r.ID, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mutate: commit: %w", err)
	}
	committed = true
	return nil
}
