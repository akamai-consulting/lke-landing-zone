package versionpins

// sortedKeys is copied, not shared: three lines beat importing a helper package
// for a map-key sort.

import "sort"

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
