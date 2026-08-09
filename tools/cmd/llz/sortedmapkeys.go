package main

// sortedMapKeys returns a map's keys in sorted order.
//
// A DUPLICATE of internal/assertreconciler's, deliberately. It is five lines of
// stdlib shape with no behaviour to drift, and the alternative — the grafana
// dashboard guard importing an assertion extension to sort a map — would create a
// dependency that says something false about how the two relate.
//
// The "two copies, one edited" rule this repo is careful about is a rule about
// FACTS: a credential list, a chart floor, an object name. It is not a rule about
// sorting.

import "sort"

func sortedMapKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
