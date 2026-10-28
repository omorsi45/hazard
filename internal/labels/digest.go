package labels

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
)

// digestOf hashes the canonical JSON of v. Every digested value is a struct or
// a slice sorted before encoding, so equal content always encodes identically.
func digestOf(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// inputSnapshot is the canonical form of every classification dependency.
// Acquisition maintenance clocks and counters are deliberately absent.
type inputSnapshot struct {
	Algorithm    string            `json:"algorithm"`
	InfraPolicy  []string          `json:"infra_policy"`
	Provider     string            `json:"provider"`
	Project      string            `json:"project"`
	Artifacts    []*artifact       `json:"artifacts"`
	Observations []*observation    `json:"observations"`
	Runs         []json.RawMessage `json:"runs"`
	Windows      []json.RawMessage `json:"requested_windows"`
	Gaps         []json.RawMessage `json:"gaps"`
	Exclusions   []json.RawMessage `json:"exclusions"`
	Flagged      []json.RawMessage `json:"flagged"`
}

// canonical encodes each item and sorts the encodings, so equal sets always
// digest identically regardless of read order.
func canonical[T any](items []*T) ([]json.RawMessage, error) {
	encoded := make([]string, 0, len(items))
	for _, item := range items {
		data, err := json.Marshal(item)
		if err != nil {
			return nil, err
		}
		encoded = append(encoded, string(data))
	}
	sort.Strings(encoded)
	out := make([]json.RawMessage, 0, len(encoded))
	for _, e := range encoded {
		out = append(out, json.RawMessage(e))
	}
	return out, nil
}

func (f *facts) inputDigest() (string, error) {
	snapshot := inputSnapshot{Algorithm: AlgorithmVersion, InfraPolicy: append([]string{infraPolicyID}, infraPhrases...), Provider: f.Provider, Project: f.Project}
	snapshot.Artifacts = append(snapshot.Artifacts, f.Artifacts...)
	sort.Slice(snapshot.Artifacts, func(i, j int) bool {
		return artifactSortKey(snapshot.Artifacts[i].artifactKey) < artifactSortKey(snapshot.Artifacts[j].artifactKey)
	})
	snapshot.Observations = append(snapshot.Observations, f.Observations...)
	sort.Slice(snapshot.Observations, func(i, j int) bool {
		return obsSortKey(snapshot.Observations[i].obsKey) < obsSortKey(snapshot.Observations[j].obsKey)
	})
	var err error
	for _, item := range []struct {
		target *[]json.RawMessage
		build  func() ([]json.RawMessage, error)
	}{
		{&snapshot.Runs, func() ([]json.RawMessage, error) { return canonical(f.Runs) }},
		{&snapshot.Windows, func() ([]json.RawMessage, error) { return canonical(f.Windows) }},
		{&snapshot.Gaps, func() ([]json.RawMessage, error) { return canonical(f.Gaps) }},
		{&snapshot.Exclusions, func() ([]json.RawMessage, error) { return canonical(f.Exclusions) }},
		{&snapshot.Flagged, func() ([]json.RawMessage, error) { return canonical(f.Flagged) }},
	} {
		if *item.target, err = item.build(); err != nil {
			return "", err
		}
	}
	return digestOf(snapshot)
}

func artifactSortKey(k artifactKey) string {
	return k.Provider + "\x00" + k.Project + "\x00" + k.UnitKey + "\x00" + k.ContentSHA256
}

func obsSortKey(k obsKey) string {
	return artifactSortKey(k.artifactKey) + "\x00" + k.ArtifactEntry + "\x00" + strconv.Itoa(k.CaseOrdinal)
}

type decisionMember struct {
	Key    obsKey `json:"key"`
	Status string `json:"status"`
}

type decisionRecord struct {
	Key      string            `json:"key"`
	Kind     string            `json:"kind"`
	State    string            `json:"state"`
	SHA      string            `json:"sha"`
	TestID   string            `json:"test_id"`
	Env      string            `json:"env"`
	Branch   string            `json:"branch"`
	Members  []decisionMember  `json:"members"`
	Reasons  []reason          `json:"reasons"`
	Evidence candidateEvidence `json:"evidence"`
}

// decisionDigest binds ordered membership, final states and selected evidence.
func decisionDigest(decisions []*decision) (string, error) {
	records := make([]decisionRecord, 0, len(decisions))
	for _, d := range decisions {
		record := decisionRecord{Key: d.Key, Kind: d.Kind, State: d.State, SHA: d.SHA, TestID: d.TestID, Env: d.Env, Branch: d.Branch, Reasons: d.Reasons, Evidence: d.Evidence}
		for _, m := range d.Members {
			record.Members = append(record.Members, decisionMember{Key: m.obsKey, Status: m.Status})
		}
		records = append(records, record)
	}
	return digestOf(struct {
		Algorithm string           `json:"algorithm"`
		Decisions []decisionRecord `json:"decisions"`
	}{AlgorithmVersion, records})
}
