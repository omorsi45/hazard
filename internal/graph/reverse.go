package graph

import "sort"

// ReverseHops returns, for each package that can observe a change in one of the
// changed packages, the shortest number of reverse import edges from the
// nearest changed package. The changed package itself is 0. Test-only imports
// count as edges, and the walk continues through them, which can only widen
// the result. Unknown changed IDs are ignored. The distance is unbounded here.
func (g *Graph) ReverseHops(changed []string) map[string]int {
	hops := map[string]int{}
	if g == nil {
		return hops
	}
	importers := map[string][]string{}
	for id, p := range g.Packages {
		seen := map[string]bool{}
		for _, list := range [][]string{p.Imports, p.TestImports} {
			for _, dep := range list {
				if _, local := g.Packages[dep]; local && !seen[dep] {
					seen[dep] = true
					importers[dep] = append(importers[dep], id)
				}
			}
		}
	}
	for dep := range importers {
		sort.Strings(importers[dep])
	}
	var queue []string
	for _, id := range changed {
		if _, known := g.Packages[id]; !known {
			continue
		}
		if _, dup := hops[id]; !dup {
			hops[id] = 0
			queue = append(queue, id)
		}
	}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, importer := range importers[id] {
			if _, done := hops[importer]; !done {
				hops[importer] = hops[id] + 1
				queue = append(queue, importer)
			}
		}
	}
	return hops
}
