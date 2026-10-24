package labels

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
)

// Candidate kinds and final states. The four states are mutually exclusive.
const (
	kindCandidate  = "candidate"
	kindUnresolved = "unresolved"

	statePending  = "pending"
	stateExcluded = "excluded"
	stateWeak     = "weak"
	stateStrong   = "strong"
)

const (
	effectExclude = "exclude"
	effectPending = "pending"
	effectWeak    = "weak"

	maxEvidenceRefs = 8
)

type groupKey struct{ provider, project, sha, test, env string }

type commitTest struct{ provider, project, sha, test string }

type nodeKey struct{ provider, project, sha string }

type unitKey struct{ provider, project, unit string }

type runKey struct{ provider, project, run string }

type commitKey struct{ provider, project, sha string }

// candidateEvidence is the bounded selected evidence of one decision.
type candidateEvidence struct {
	ParentSHA    string `json:"parent_sha,omitempty"`
	ParentPass   string `json:"parent_pass,omitempty"`
	RecoverySHA  string `json:"recovery_sha,omitempty"`
	RecoveryPass string `json:"recovery_pass,omitempty"`
}

// reason is one applicable reason with bounded opaque evidence references.
type reason struct {
	Name, Effect string
	Evidence     []string
}

type decision struct {
	Key, Kind, State         string
	Provider, Project        string
	SHA, TestID, Env, Branch string
	Members                  []*observation
	Reasons                  []reason
	Evidence                 candidateEvidence
}

// add records a reason once per decision, merging evidence references.
func (d *decision) add(name, effect string, evidence ...string) {
	for i := range d.Reasons {
		if d.Reasons[i].Name == name {
			d.Reasons[i].Evidence = mergeEvidence(d.Reasons[i].Evidence, evidence)
			return
		}
	}
	d.Reasons = append(d.Reasons, reason{Name: name, Effect: effect, Evidence: mergeEvidence(nil, evidence)})
	sort.Slice(d.Reasons, func(i, j int) bool { return d.Reasons[i].Name < d.Reasons[j].Name })
}

func mergeEvidence(have, more []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, ref := range append(append([]string{}, have...), more...) {
		if ref != "" && !seen[ref] {
			seen[ref] = true
			out = append(out, ref)
		}
	}
	sort.Strings(out)
	if len(out) > maxEvidenceRefs {
		out = out[:maxEvidenceRefs]
	}
	return out
}

func (d *decision) has(effect string) bool {
	for _, r := range d.Reasons {
		if r.Effect == effect {
			return true
		}
	}
	return false
}

// resolve derives the single final state. Excluded wins over pending; weak
// exists only when recovery is the sole missing predicate; strong requires
// that no reason of any effect applies.
func (d *decision) resolve() {
	switch {
	case d.has(effectExclude):
		d.State = stateExcluded
	case d.has(effectPending):
		d.State = statePending
	case d.Kind == kindCandidate && d.has(effectWeak):
		d.State = stateWeak
	case d.Kind == kindCandidate:
		d.State = stateStrong
	default:
		d.State = statePending
	}
}

func hasKnownIdentity(o *observation) bool { return o.TestID != "" && o.IdentityError == "" }

func (o *observation) testedSHA() string {
	if known(o.art.TestedSHA) && known(o.art.TestedRevisionEvidence) {
		return *o.art.TestedSHA
	}
	return ""
}

func (o *observation) environment() string {
	if known(o.art.EnvironmentID) && known(o.art.EnvironmentEvidence) {
		return *o.art.EnvironmentID
	}
	return ""
}

// completeWitness reports whether an observation comes from a complete,
// execution-verified report, so a pass in it can serve as comparison evidence.
func (o *observation) completeWitness() bool {
	return o.art.State == "complete" && !o.art.SuiteIncomplete && !o.SuiteIncomplete && known(o.art.JobID) && known(o.art.ExecutionEvidence)
}

// graph holds verified parent edges taken from acquired commit provenance.
// A commit with unparseable or contradictory parent facts has no usable edges.
type graph struct {
	parents  map[nodeKey][]string
	conflict map[nodeKey]bool
	children map[nodeKey][]string
}

func buildGraph(f *facts) *graph {
	g := &graph{parents: map[nodeKey][]string{}, conflict: map[nodeKey]bool{}, children: map[nodeKey][]string{}}
	for _, a := range f.Artifacts {
		if !known(a.TestedSHA) || !known(a.TestedRevisionEvidence) || a.TestedParentsJSON == nil {
			continue
		}
		node := nodeKey{a.Provider, a.Project, *a.TestedSHA}
		var parents []string
		if err := json.Unmarshal([]byte(*a.TestedParentsJSON), &parents); err != nil || parents == nil {
			g.conflict[node] = true
			continue
		}
		sort.Strings(parents)
		if have, ok := g.parents[node]; ok && !reflect.DeepEqual(have, parents) {
			g.conflict[node] = true
		}
		g.parents[node] = parents
	}
	for node, parents := range g.parents {
		if g.conflict[node] {
			continue
		}
		for _, p := range parents {
			parent := nodeKey{node.provider, node.project, p}
			g.children[parent] = append(g.children[parent], node.sha)
		}
	}
	return g
}

const (
	parentsUnknown  = "unknown"
	parentsConflict = "conflict"
	parentsRoot     = "root"
	parentsSingle   = "single"
	parentsMulti    = "multi"
)

func (g *graph) parentsOf(n nodeKey) ([]string, string) {
	if g.conflict[n] {
		return nil, parentsConflict
	}
	parents, ok := g.parents[n]
	switch {
	case !ok:
		return nil, parentsUnknown
	case len(parents) == 0:
		return nil, parentsRoot
	case len(parents) == 1:
		return parents, parentsSingle
	}
	return parents, parentsMulti
}

// descendants returns, sorted, every commit reachable from n through verified edges.
func (g *graph) descendants(n nodeKey) []string {
	seen := map[string]bool{}
	queue := []nodeKey{n}
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		for _, child := range g.children[next] {
			if !seen[child] {
				seen[child] = true
				queue = append(queue, nodeKey{next.provider, next.project, child})
			}
		}
	}
	out := make([]string, 0, len(seen))
	for sha := range seen {
		out = append(out, sha)
	}
	sort.Strings(out)
	return out
}

// index holds the comparison witnesses of the retained observations.
type index struct {
	// passes: complete, execution-verified passes with known identity, tested
	// revision and environment, keyed by the full comparison identity.
	passes map[groupKey][]*observation
	// byCommit: every observation of a test at a tested revision, any status.
	byCommit map[commitTest][]*observation
}

func buildIndex(f *facts) *index {
	idx := &index{passes: map[groupKey][]*observation{}, byCommit: map[commitTest][]*observation{}}
	for _, o := range f.Observations {
		sha := o.testedSHA()
		if !hasKnownIdentity(o) || sha == "" {
			continue
		}
		idx.byCommit[commitTest{o.Provider, o.Project, sha, o.TestID}] = append(idx.byCommit[commitTest{o.Provider, o.Project, sha, o.TestID}], o)
		if env := o.environment(); o.Status == "pass" && env != "" && o.completeWitness() {
			key := groupKey{o.Provider, o.Project, sha, o.TestID, env}
			idx.passes[key] = append(idx.passes[key], o)
		}
	}
	return idx
}

// infraPolicyID versions the fixed case-insensitive literal phrase list. Generic
// words such as "error" or "failure" alone are never infrastructure evidence.
const infraPolicyID = "infra-phrases/1"

var infraPhrases = []string{"network unreachable", "no space left", "docker daemon", "registry auth", "oom kill", "out of memory", "job timed out", "runner failure"}

// lookup holds the per-test and per-unit annotations consulted by predicates.
type lookup struct {
	quarantined   map[string]bool
	unstable      map[string]bool
	conflictUnits map[unitKey]bool
	collisions    map[obsKey]bool
	runsByID      map[runKey][]*run
	runsByHead    map[commitKey][]*run
	activeGaps    []*gap
}

func buildLookup(f *facts) *lookup {
	lk := &lookup{quarantined: map[string]bool{}, unstable: map[string]bool{}, conflictUnits: map[unitKey]bool{}, collisions: map[obsKey]bool{},
		runsByID: map[runKey][]*run{}, runsByHead: map[commitKey][]*run{}}
	for _, r := range f.Runs {
		lk.runsByID[runKey{r.Provider, r.Project, r.RunID}] = append(lk.runsByID[runKey{r.Provider, r.Project, r.RunID}], r)
		lk.runsByHead[commitKey{r.Provider, r.Project, r.SourceHeadSHA}] = append(lk.runsByHead[commitKey{r.Provider, r.Project, r.SourceHeadSHA}], r)
	}
	for _, t := range f.Flagged {
		if t.Quarantined {
			lk.quarantined[t.ID] = true
		}
		if t.UnstableID {
			lk.unstable[t.ID] = true
		}
	}
	for _, g := range f.Gaps {
		if !g.Active {
			continue
		}
		lk.activeGaps = append(lk.activeGaps, g)
		if g.Reason == "acquisition_conflict" {
			lk.conflictUnits[unitKey{g.Provider, g.Project, g.UnitKey}] = true
		}
	}
	for _, e := range f.Exclusions {
		if e.Reason == "duplicate_test_identity" {
			lk.collisions[e.obsKey] = true
		}
	}
	return lk
}

// applyMemberDisqualifiers records every known positive disqualifier of the
// members: quarantine, unstable identity, execution error, bounded
// infrastructure evidence, and acquisition conflict or collision.
func applyMemberDisqualifiers(d *decision, lk *lookup) {
	if d.TestID != "" && lk.quarantined[d.TestID] {
		d.add("quarantined", effectExclude, "tests:"+d.TestID)
	}
	if d.TestID != "" && lk.unstable[d.TestID] {
		d.add("unstable_identity", effectExclude, "tests:"+d.TestID)
	}
	for _, m := range d.Members {
		if m.Unstable {
			d.add("unstable_identity", effectExclude, m.ref())
		}
		if m.Status == "error" {
			d.add("execution_error", effectExclude, m.ref())
		}
		if m.InfraReason != "" {
			d.add("infra", effectExclude, "infra_reason@"+m.ref())
		}
		for _, phrase := range infraPhrases {
			if strings.Contains(m.message, phrase) {
				d.add("infra", effectExclude, "policy:"+infraPolicyID+":"+phrase+"@"+m.ref())
			}
		}
		if lk.collisions[m.obsKey] {
			d.add("acquisition_collision", effectExclude, m.ref())
		}
		if lk.conflictUnits[unitKey{m.Provider, m.Project, m.UnitKey}] {
			d.add("acquisition_conflict", effectExclude, m.ref())
		}
	}
}

// evaluateCompleteness keeps a candidate pending while any relevant execution,
// suite, diagnostic or acquisition fact is incomplete or unverified. Gaps are
// scoped to the evidenced run or unit; an active discovery gap, which prevents
// complete attempt enumeration, blocks every candidate.
func evaluateCompleteness(d *decision, lk *lookup) {
	branches := map[string]bool{}
	for _, m := range d.Members {
		a := m.art
		if !known(a.JobID) || !known(a.ExecutionEvidence) || (a.Provider == "github" && (a.Attempt == nil || *a.Attempt < 1)) {
			d.add("unverified_execution", effectPending, m.ref())
		}
		if a.State != "complete" {
			d.add("incomplete_execution", effectPending, m.ref())
		}
		runs := lk.runsByID[runKey{a.Provider, a.Project, a.RunID}]
		if len(runs) == 0 {
			d.add("incomplete_execution", effectPending, m.ref())
		}
		for _, r := range runs {
			if r.State != "complete" {
				d.add("incomplete_execution", effectPending, "run:"+r.RunID)
			}
		}
		if a.SuiteIncomplete || m.SuiteIncomplete {
			d.add("incomplete_suite", effectPending, m.ref())
		}
		if a.DiagnosticsTruncated || m.DiagnosticsTruncated {
			d.add("incomplete_diagnostics", effectPending, m.ref())
		}
		branches[a.Branch] = true
		for _, g := range lk.activeGaps {
			switch {
			case g.Reason == "acquisition_conflict" && g.UnitKey == a.UnitKey:
			case (g.RunID != "" && g.RunID == a.RunID) || g.UnitKey == a.UnitKey:
				d.add("active_gap", effectPending, "gap:"+g.Reason+"@"+g.UnitKey)
			case g.RunID == "" && g.Reason != "acquisition_conflict":
				d.add("discovery_gap", effectPending, "gap:"+g.Reason+"@"+g.UnitKey)
			}
		}
	}
	for _, r := range lk.runsByHead[commitKey{d.Provider, d.Project, d.SHA}] {
		if r.State != "complete" {
			d.add("incomplete_execution", effectPending, "run:"+r.RunID)
		}
	}
	if len(branches) != 1 || branches[""] {
		d.add("branch_unverified", effectPending, refs(d.Members)...)
	}
}

// evaluateSameCommit applies the flaky rule: a pass of the same test at the
// same tested revision in the same evidenced environment excludes the group.
// A pass whose environment is unknown can neither prove nor clear it.
func evaluateSameCommit(d *decision, idx *index) {
	for _, o := range idx.byCommit[commitTest{d.Provider, d.Project, d.SHA, d.TestID}] {
		if o.Status != "pass" {
			continue
		}
		switch o.environment() {
		case d.Env:
			d.add("flaky_same_commit", effectExclude, o.ref())
		case "":
			d.add("unknown_environment_pass", effectPending, o.ref())
		}
	}
}

func refs(os []*observation) []string {
	out := make([]string, 0, len(os))
	for _, o := range os {
		out = append(out, o.ref())
	}
	return out
}

func firstByRef(os []*observation) *observation {
	var best *observation
	for _, o := range os {
		if best == nil || obsSortKey(o.obsKey) < obsSortKey(best.obsKey) {
			best = o
		}
	}
	return best
}

// alwaysRun returns the sorted union of every identified flaky, unstable or
// quarantined test: excluded and unresolved records and manual inventory flags,
// independent of any label eligibility. No runnable ID is invented for an
// unknown identity.
func alwaysRun(f *facts, decisions []*decision) []string {
	set := map[string]bool{}
	for _, t := range f.Flagged {
		set[t.ID] = true
	}
	for _, d := range decisions {
		if d.TestID == "" {
			continue
		}
		for _, r := range d.Reasons {
			if r.Name == "flaky_same_commit" || r.Name == "quarantined" || r.Name == "unstable_identity" {
				set[d.TestID] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func classify(f *facts) []*decision {
	groups := map[groupKey][]*observation{}
	var unresolved []*decision
	for _, o := range f.Observations {
		if o.Status != "fail" && o.Status != "error" {
			continue
		}
		sha, env := o.testedSHA(), o.environment()
		if !hasKnownIdentity(o) || sha == "" || env == "" {
			unresolved = append(unresolved, unresolvedDecision(o, sha, env))
			continue
		}
		key := groupKey{o.Provider, o.Project, sha, o.TestID, env}
		groups[key] = append(groups[key], o)
	}
	g, idx, lk := buildGraph(f), buildIndex(f), buildLookup(f)
	for _, d := range unresolved {
		applyMemberDisqualifiers(d, lk)
		d.resolve()
	}
	var out []*decision
	for key, members := range groups {
		d := &decision{Kind: kindCandidate, Provider: key.provider, Project: key.project, SHA: key.sha, TestID: key.test, Env: key.env, Members: members}
		d.Branch = members[0].art.Branch
		applyMemberDisqualifiers(d, lk)
		evaluateCompleteness(d, lk)
		evaluateSameCommit(d, idx)
		evaluateParent(d, g, idx)
		evaluateRecovery(d, g, idx)
		d.resolve()
		out = append(out, d)
	}
	out = append(out, unresolved...)
	sort.Slice(out, func(i, j int) bool { return out[i].sortKey() < out[j].sortKey() })
	for _, d := range out {
		d.Key = d.Kind[:1] + ":" + hashText(d.sortKey())[:32]
	}
	return out
}

// evaluateParent requires a comparable complete pass at a single known,
// unambiguous immediate parent. Unknown or contradictory parent facts, a root,
// a merge, or an unavailable comparison keep the candidate pending.
func evaluateParent(d *decision, g *graph, idx *index) {
	parents, state := g.parentsOf(nodeKey{d.Provider, d.Project, d.SHA})
	own := refs(d.Members)
	switch state {
	case parentsUnknown:
		d.add("parent_unknown", effectPending, own...)
	case parentsConflict:
		d.add("parent_conflict", effectPending, own...)
	case parentsRoot:
		d.add("no_parent", effectPending, own...)
	case parentsMulti:
		d.add("merge_ambiguity", effectPending, own...)
	default:
		parent := parents[0]
		if witnesses := idx.passes[groupKey{d.Provider, d.Project, parent, d.TestID, d.Env}]; len(witnesses) > 0 {
			d.Evidence.ParentSHA, d.Evidence.ParentPass = parent, firstByRef(witnesses).ref()
			return
		}
		observed := idx.byCommit[commitTest{d.Provider, d.Project, parent, d.TestID}]
		switch {
		case len(observed) == 0:
			d.add("missing_parent_observation", effectPending, own...)
		case comparableFailure(observed, d.Env):
			d.add("parent_not_passing", effectPending, refs(observed)...)
		default:
			d.add("parent_comparison_unavailable", effectPending, refs(observed)...)
		}
	}
}

func comparableFailure(observed []*observation, env string) bool {
	for _, o := range observed {
		if (o.Status == "fail" || o.Status == "error") && o.environment() == env {
			return true
		}
	}
	return false
}

// evaluateRecovery looks for a later comparable complete pass at a verified
// descendant on the same nonempty observed branch. Its absence is the only
// predicate whose failure leaves a candidate weak rather than pending.
func evaluateRecovery(d *decision, g *graph, idx *index) {
	if d.Branch != "" {
		for _, sha := range g.descendants(nodeKey{d.Provider, d.Project, d.SHA}) {
			var onBranch []*observation
			for _, w := range idx.passes[groupKey{d.Provider, d.Project, sha, d.TestID, d.Env}] {
				if w.art.Branch == d.Branch {
					onBranch = append(onBranch, w)
				}
			}
			if len(onBranch) > 0 {
				d.Evidence.RecoverySHA, d.Evidence.RecoveryPass = sha, firstByRef(onBranch).ref()
				return
			}
		}
	}
	d.add("no_passing_descendant", effectWeak, refs(d.Members)...)
}

// unresolvedDecision keeps a failing case whose grouping facts are missing as
// its own record: missing facts are never merged or invented.
func unresolvedDecision(o *observation, sha, env string) *decision {
	d := &decision{Kind: kindUnresolved, Provider: o.Provider, Project: o.Project, SHA: sha, Env: env, Members: []*observation{o}}
	if hasKnownIdentity(o) {
		d.TestID = o.TestID
	} else {
		d.add("missing_identity", effectPending, o.ref())
	}
	if sha == "" {
		d.add("missing_tested_revision", effectPending, o.ref())
	}
	if env == "" {
		d.add("missing_environment", effectPending, o.ref())
	}
	return d
}

func (d *decision) sortKey() string {
	if d.Kind == kindUnresolved {
		return d.Kind + "\x00" + obsSortKey(d.Members[0].obsKey)
	}
	return d.Kind + "\x00" + d.Provider + "\x00" + d.Project + "\x00" + d.SHA + "\x00" + d.TestID + "\x00" + d.Env
}
