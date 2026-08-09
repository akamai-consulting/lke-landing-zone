package onboard

// deps.go — the localisable edges, and the one helper this package keeps its own
// copy of.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/kubectlprobe"

// execOutput and execLookPath delegate to kubectlprobe's seams through CLOSURES,
// never by assignment — a direct assignment snapshots the seam before any test can
// swap it, and there must be exactly ONE swappable var per underlying call.
func execOutput(name string, args ...string) ([]byte, error) { return kubectlprobe.Exec(name, args...) }
func execLookPath(file string) (string, error)               { return kubectlprobe.LookPathFn(file) }

// firstNonEmpty is copied, not shared. Seventeenth package in this campaign to
// keep its own three lines; package main kept a copy too, because three files
// there still use it.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
