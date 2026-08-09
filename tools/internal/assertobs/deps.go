package assertobs

import (
	"os"
	"sort"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/objenc"
)

// Deps carries what this package cannot reach for itself.
//
// FOUR FIELDS FOR TWELVE FILES AND ~2,700 LINES, which is the shape the closure
// measurement predicted: 14, and adding ci_readiness.go to the set LOWERED it from
// 20. That is worth noting — the Loki readiness helpers looked like a separate
// concern and were in fact the other half of this one, so pulling them in removed
// more edges than it added.
type Deps struct {
	// Exec captures a command's stdout. The classified reads go through
	// internal/kubectlprobe; this is for calls wanting raw output.
	Exec func(name string, args ...string) ([]byte, error)

	// KubectlOut runs kubectl and returns stdout as a string, erroring on non-zero.
	// Distinct from Exec because the Loki pod inspection wants the string form and
	// the error, not bytes.
	KubectlOut func(args ...string) (string, error)

	// Summary appends to a GitHub Actions file. The scrape and alert lanes publish
	// their verdicts there, so it must do real work — see the note on caps.
	Summary func(envVar string, lines ...string) error

	// ObjEncDeps hands internal/objenc its capabilities, for reading the Loki
	// object-store credentials the prove-writes lane needs. Passed through rather
	// than rebuilt: objenc owns what it needs and this package only asks.
	ObjEncDeps func() objenc.Deps
}

// caps is the installed capability set. Summary defaults to a REAL GHA append,
// not a no-op — an installed default is a fixture too, and defaulting this one to
// `return nil` has broken a test in four separate extractions.
var caps = Deps{
	Exec:       func(string, ...string) ([]byte, error) { return nil, nil },
	KubectlOut: func(...string) (string, error) { return "", nil },
	Summary:    realGHAAppend,
	ObjEncDeps: func() objenc.Deps { return objenc.Deps{} },
}

// Install wires the capabilities main owns. Call once, before any lane runs.
func Install(d Deps) { caps = d }

func realGHAAppend(envVar string, lines ...string) error {
	path := os.Getenv(envVar)
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(strings.Join(lines, "\n") + "\n")
	return err
}

// defaultAuditLokiService is the Loki gateway the audit lane port-forwards to.
// A const copied rather than imported: package main's lives in the openbao-audit
// verb, and this package needs the VALUE for a flag default, not a dependency on
// that verb.
const defaultAuditLokiService = "monitoring/loki-gateway:80"

// sortedMapKeys returns a map's keys in sorted order. Three lines of stdlib shape;
// see cmd/llz/sortedmapkeys.go for why this is a copy rather than a shared helper.
func sortedMapKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
