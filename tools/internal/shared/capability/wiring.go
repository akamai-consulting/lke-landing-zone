package capability

// wiring.go — the ASSEMBLY layer's guard rail.
//
// ────────────────────────────────────────────────────────────────────────────
// A REFUSING HANDLE AND A MIS-WIRED ONE LOOK IDENTICAL, AND THAT SHIPPED.
//
// This package hands back a non-nil refusing handle rather than nil, deliberately:
// nil "would panic at the call site and be reported as a crash rather than as a
// permission fault, and — worse — would tempt callers into `if w != nil` guards
// that silently skip the work." Right, and it has a cost nobody had paid. A Deps
// struct built from the WRONG binding is indistinguishable from one built
// correctly: both fields are non-nil, both satisfy the interface, and the
// difference only appears when the code path runs, as a permission error naming a
// grant nobody forgot.
//
// internal/cli built assert-identity's and assert-secrets' Writers from
// `Extension().Bindings[0]`, an ASSERTION in both, while the transitions carrying
// cluster-write sat at index 1 and index 3. Both lanes run in
// `llz ci assert-suite`, both call the Writer for real (CreateToken, Delete,
// ApplyStdin), and both got a refusal. Everything was green.
//
// SO ASSEMBLY ASKS A QUESTION THE CALL SITE MUST NOT: is this handle live? Nothing
// here changes what a caller does with a refusal — a refusal stays a refusal. It
// changes WHEN a wiring mistake is discovered, from whatever hour the lane runs to
// the moment the binary starts, with the selected binding named.
//
// THAT IS RepoForGate's PRECEDENT. It panics rather than returning a refusing
// reader, because "a guard whose declaration lost its gate is a wiring bug, and
// the refusing reader would express it as every file being unreadable" — which
// reads as a broken checkout, not as a missing declaration.
//
// A PANIC IS RIGHT HERE AND NOWHERE ELSE. These are called from init() off
// compiled-in declarations, so the panic is unreachable in a built binary the
// moment the wiring is right, and internal/cli's TestEveryInstalledCapabilityIsLive
// proves it by exercising every installer.
// ────────────────────────────────────────────────────────────────────────────

import (
	"fmt"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

// MustWriter builds the Writer a binding entitles it to, and refuses to install a
// refusing one.
func MustWriter(b extension.Binding) Writer {
	w := For(b).Writer
	if IsRefusing(w) {
		panic(wiringBug(b, "Writer", extension.ClusterWrite))
	}
	return w
}

// MustCluster is mustWriter for the read handle. A lane wired to a refusing
// Cluster reads nothing and reports that as the platform's fault.
func MustCluster(b extension.Binding) Cluster {
	c := For(b).Cluster
	if IsRefusing(c) {
		panic(wiringBug(b, "Cluster", extension.ClusterRead))
	}
	return c
}

// wiringBug is the one message, so every instance of this mistake reads the same
// and names the binding that was actually selected — which is the fact the reader
// needs and the one a permission error omits.
func wiringBug(b extension.Binding, handle string, need extension.Grant) string {
	return fmt.Sprintf("capability wiring: this Deps asked for a %s from binding %s, which does "+
		"not declare %q — so the handle would be present, refusing, and indistinguishable from a "+
		"working one until the lane ran and reported a permission error. Select the binding that "+
		"declares it, by NAME (Extension().MustBinding(...)), not by position.",
		handle, b, need)
}
