package events

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/omorsi45/hazard/internal/mutate"
)

// MutationGroup is the split group of a mutation event: the mutated file, so
// every mutant of one file lands on the same side of a split.
func MutationGroup(file string) string { return "mut:" + file }

// MutationEvents turns mutation records into events. Killed records (Failing
// is the confirmed killers) and survived records (nothing failed) become
// events; every other status is counted in skipped by status and never
// becomes an event. For each kept record the mutants of its file at BaseSHA
// are regenerated (the Go operator of the record for .go files, the
// configuration operator otherwise), the record's ID is found among them and
// applied, and the mutated file is returned in contents keyed by event ID.
// Events are ordered by ID and have no HeadSHA: the replay that commits the
// content creates it. A record whose ID cannot be regenerated, a killed record
// without killers and a repeated ID are errors that name the record.
func MutationEvents(ctx context.Context, repoDir string, recs []mutate.Record) (events []Event, contents map[string][]byte, skipped map[string]int, err error) {
	skipped = map[string]int{}
	var kept []mutate.Record
	for _, rec := range recs {
		if rec.Status == "killed" || rec.Status == "survived" {
			kept = append(kept, rec)
		} else {
			skipped[rec.Status]++
		}
	}
	slices.SortStableFunc(kept, func(a, b mutate.Record) int { return strings.Compare(a.ID, b.ID) })

	contents = map[string][]byte{}
	times := map[string]time.Time{}
	sources := map[string][]byte{}
	for _, rec := range kept {
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, err
		}
		if _, dup := contents[rec.ID]; dup {
			return nil, nil, nil, fmt.Errorf("events: mutant %s appears in more than one record", rec.ID)
		}
		if rec.Status == "killed" && len(rec.Killers) == 0 {
			return nil, nil, nil, fmt.Errorf("events: mutant %s is killed but has no killing test", rec.ID)
		}
		if rec.BaseSHA == "" || strings.HasPrefix(rec.BaseSHA, "-") {
			return nil, nil, nil, fmt.Errorf("events: mutant %s has no usable base revision (%q)", rec.ID, rec.BaseSHA)
		}
		at, ok := times[rec.BaseSHA]
		if !ok {
			if at, err = committerTime(ctx, repoDir, rec.BaseSHA); err != nil {
				return nil, nil, nil, fmt.Errorf("events: mutant %s: %w", rec.ID, err)
			}
			times[rec.BaseSHA] = at
		}
		key := rec.BaseSHA + "\x00" + rec.File
		src, ok := sources[key]
		if !ok {
			out, err := runGit(ctx, repoDir, nil, nil, "cat-file", "blob", rec.BaseSHA+":"+rec.File)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("events: mutant %s: read %s at %s: %w", rec.ID, rec.File, rec.BaseSHA, err)
			}
			sources[key] = out
			src = out
		}
		mutated, err := regenerate(rec, src)
		if err != nil {
			return nil, nil, nil, err
		}
		failing := []string{}
		if rec.Status == "killed" {
			failing = slices.Compact(slices.Sorted(slices.Values(rec.Killers)))
		}
		events = append(events, Event{
			ID: rec.ID, Origin: OriginMutation, Group: MutationGroup(rec.File),
			BaseSHA: rec.BaseSHA, Failing: failing, File: rec.File, Operator: rec.Operator, Line: rec.Line, Time: at,
		})
		contents[rec.ID] = mutated
	}
	return events, contents, skipped, nil
}

// regenerate returns the content of src with the mutant of rec applied.
func regenerate(rec mutate.Record, src []byte) ([]byte, error) {
	var mutants []mutate.Mutant
	if strings.HasSuffix(rec.File, ".go") {
		var err error
		if mutants, err = mutate.GoMutants(rec.File, src, []string{rec.Operator}); err != nil {
			return nil, fmt.Errorf("events: mutant %s: %w", rec.ID, err)
		}
	} else {
		mutants = mutate.ConfigMutants(rec.File, src)
	}
	for _, m := range mutants {
		if m.ID != rec.ID {
			continue
		}
		out, err := mutate.Apply(src, m)
		if err != nil {
			return nil, fmt.Errorf("events: mutant %s: %w", rec.ID, err)
		}
		return out, nil
	}
	return nil, fmt.Errorf("events: mutant %s was not regenerated from %s at %s with operator %q", rec.ID, rec.File, rec.BaseSHA, rec.Operator)
}

// committerTime is the committer time of a commit, in UTC.
func committerTime(ctx context.Context, repoDir, rev string) (time.Time, error) {
	out, err := runGit(ctx, repoDir, nil, nil, "show", "-s", "--format=%ct", rev+"^{commit}", "--")
	if err != nil {
		return time.Time{}, fmt.Errorf("committer time of %s: %w", rev, err)
	}
	secs, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("committer time of %s: %w", rev, err)
	}
	return time.Unix(secs, 0).UTC(), nil
}
