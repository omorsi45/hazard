package engine

import (
	"fmt"
	"sort"
	"strings"

	"github.com/omorsi45/hazard/internal/change"
	"github.com/omorsi45/hazard/internal/index"
)

const (
	reasonPythonInventory = "python_inventory_changed"
	reasonInventoryError  = "inventory_error"
)

// withHeadTests accounts for tests that changed test files define at head and
// the index, taken earlier, does not know (D055).
//
// Go tests are found again statically in every changed Go test file and the
// missing ones are added to a copy of the manifest's inventory, so they take
// part in L1 as direct members and in AllTests. A Python test module cannot be
// rediscovered without running pytest (parametrized node IDs depend on
// collection), so a changed test module that defines a test the inventory does
// not hold for that module makes the inventory untrustworthy: reason is
// reasonPythonInventory. A changed test file that cannot be read or parsed at
// head is reasonInventoryError. detail names the file and the test. With a
// non-empty reason the manifest must not be used for narrowing; m itself is
// never modified.
func withHeadTests(m *index.Manifest, cs *change.ChangeSet, head index.HeadReader) (out *index.Manifest, reason, detail string) {
	known := make(map[string]bool, len(m.Tests))
	for _, t := range m.Tests {
		known[t.ID] = true
	}
	var added []index.Test
	for _, f := range cs.Files {
		if f.Status == "D" || f.Kind != change.KindTest && f.OldKind != change.KindTest {
			continue
		}
		switch f.Language {
		case "go":
			if f.Package == "" {
				continue
			}
			src, err := head.Read(f.Path)
			if err != nil {
				return m, reasonInventoryError, fmt.Sprintf("read %s at head: %v", f.Path, err)
			}
			found, err := index.DiscoverGoTests(f.Package, f.Path, src)
			if err != nil {
				return m, reasonInventoryError, err.Error()
			}
			for _, t := range found {
				if !known[t.ID] {
					known[t.ID] = true
					added = append(added, t)
				}
			}
		case "python":
			src, err := head.Read(f.Path)
			if err != nil {
				return m, reasonInventoryError, fmt.Sprintf("read %s at head: %v", f.Path, err)
			}
			if name := unknownPythonTest(m.Tests, f.Path, index.PythonTestNames(src)); name != "" {
				return m, reasonPythonInventory, fmt.Sprintf("%s defines %s, which the index does not hold; run hazard index again", f.Path, name)
			}
		}
	}
	if len(added) == 0 {
		return m, "", ""
	}
	cp := *m
	cp.Tests = append(append(make([]index.Test, 0, len(m.Tests)+len(added)), m.Tests...), added...)
	sort.Slice(cp.Tests, func(i, j int) bool { return cp.Tests[i].ID < cp.Tests[j].ID })
	return &cp, "", ""
}

// unknownPythonTest returns the first of names ("test_a" or "Class::test_b")
// that no inventory test of the module file holds, or "" when all are known.
func unknownPythonTest(tests []index.Test, file string, names []string) string {
	have := map[string]bool{}
	for _, t := range tests {
		if t.Language == "python" && t.File == file {
			have[nodeSuffix(t)] = true
		}
	}
	for _, name := range names {
		if !have[name] {
			return name
		}
	}
	return ""
}

// nodeSuffix is the part of a Python test's node ID after the module file,
// without a parametrization suffix: "test_a" or "TestK::test_b". A runner ID
// that is not a node ID of the module matches no name.
func nodeSuffix(t index.Test) string {
	id := t.RunnerID
	if rest, ok := strings.CutPrefix(id, t.File+"::"); ok {
		id = rest
	} else if _, rest, ok := strings.Cut(id, "::"); ok {
		id = rest
	}
	if i := strings.IndexByte(id, '['); i >= 0 {
		id = id[:i]
	}
	return id
}

// testIDs is the sorted inventory of m.
func testIDs(m *index.Manifest) []string {
	out := make([]string, len(m.Tests))
	for i, t := range m.Tests {
		out[i] = t.ID
	}
	sort.Strings(out)
	return out
}
