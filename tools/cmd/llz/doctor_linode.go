package main

// doctor_linode.go — the one thing `llz doctor` never asked: can this LINODE
// ACCOUNT actually build what the spec describes?
//
// doctor is billed as the single "am I ready to build?" gate, but every check it
// ran was local or GitHub — tooling on PATH, gh auth, spec files, repo config. The
// three prerequisites the quickstart opens with (a Linode account WITH
// LKE-Enterprise, an APL entitlement, a GitHub org) were exactly the ones it could
// not see, and `cluster.k8sVersion` was validated for non-emptiness only.
//
// So an account without LKE-E entitlement, or a spec pinning a `+lke` version that
// has since been retired, got a GREEN doctor — and then `llz up` provisioned real
// cloud resources (state bucket + scoped OBJ key), dispatched a workflow, and
// failed inside terraform apply. Worse, per docs/lessons-learned.md, an LKE-E
// create that cannot be satisfied does not always fail: it can hang on
// "Still creating..." to the job timeout, a mode that doc explicitly calls not
// reliably diagnosable. This turns the most expensive failure in the flow into a
// line of local output before anything is provisioned.
//
// ADVISORY ONLY, on purpose. The route is verified to exist (an unknown Linode
// path 404s before auth; this one 401s) but its response BODY has not been seen
// against an entitled account. Anything unexpected — no token, an auth failure, an
// empty or unparseable list — is reported as UNKNOWN and never as "not offered",
// and none of it changes doctor's exit code. A check that cannot be fully verified
// must not be allowed to block a build that would have worked.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/cigate"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/linode"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/color"
)

// lkeVersionLister is the slice of the Linode client this needs — a seam so the
// tests never touch the network.
type lkeVersionLister interface {
	ListLKEVersions(ctx context.Context, tier string) ([]string, error)
}

// doctorLinodeClient returns a live client, or nil when no token is configured.
var doctorLinodeClient = func() lkeVersionLister {
	tok := firstNonEmpty(os.Getenv("LINODE_TOKEN"), os.Getenv("LINODE_API_TOKEN"))
	if tok == "" {
		return nil
	}
	return linode.NewClient(tok, 20*time.Second)
}

// reportLinodeAccount prints doctor's "Linode account" section for the k8s
// versions the spec pins. Returns nothing: this section never fails the gate.
func reportLinodeAccount(want []string) {
	fmt.Println("\n" + color.Bold("Linode account (advisory — never blocks the build):"))

	c := doctorLinodeClient()
	if c == nil {
		fmt.Printf("  %s  LKE-Enterprise check skipped — set LINODE_TOKEN to verify the account offers your k8sVersion\n", color.Dim("–"))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	got, err := c.ListLKEVersions(ctx, linode.LKETierEnterprise)
	switch {
	case err != nil:
		// Includes 401/403 — which is the interesting case, because it is what a
		// token without the scope, or an account without the entitlement, looks
		// like. Still advisory: it is also what a network blip looks like.
		fmt.Printf("  %s  could not list LKE-Enterprise versions — check the PAT's scope and that the account is LKE-Enterprise entitled\n", color.Yellow("!"))
		fmt.Printf("      %s\n", color.Dim(cigate.FirstLine(err.Error())))
		return
	case len(got) == 0:
		fmt.Printf("  %s  the account reports NO LKE-Enterprise versions — an apply would have nothing to create\n", color.Yellow("!"))
		return
	}

	fmt.Printf("  %s  LKE-Enterprise reachable — %d version(s) offered\n", color.Green("✓"), len(got))
	for _, w := range want {
		if lkeVersionOffered(w, got) {
			fmt.Printf("  %s  k8sVersion %s is offered\n", color.Green("✓"), w)
			continue
		}
		fmt.Printf("  %s  k8sVersion %s is NOT in the account's list — a retired or mistyped pin fails (or HANGS) at apply\n", color.Yellow("!"), w)
		fmt.Printf("      %s\n", color.Dim("offered: "+strings.Join(got, ", ")))
	}
}

// lkeVersionOffered reports whether want matches any offered version.
//
// Matching is deliberately generous. The spec pins a full LKE-E build
// (`v1.33.6+lke7`) while a Linode version catalog may list either that or a bare
// `major.minor` — and the exact enterprise spelling is unverified. An
// over-strict comparison would cry wolf on a perfectly good spec, which for an
// advisory check is worse than saying nothing, so a major.minor agreement counts.
func lkeVersionOffered(want string, offered []string) bool {
	for _, o := range offered {
		if o == want || majorMinor(o) != "" && majorMinor(o) == majorMinor(want) {
			return true
		}
	}
	return false
}

// majorMinor extracts "1.33" from "v1.33.6+lke7", "1.33.6", or "1.33".
func majorMinor(v string) string {
	s := strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(s, "+-"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	return parts[0] + "." + parts[1]
}

// specK8sVersions returns the k8sVersion(s) doctor should check against the
// account: just the named env's when one is given, otherwise every env's,
// de-duplicated. Silent on a repo with no spec — doctor runs there too.
func specK8sVersions(env string) []string {
	lz, present, err := loadSpec()
	if !present || err != nil || lz == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	add := func(v string) {
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	if env != "" {
		if e, ok := lz.Env(env); ok {
			add(e.Cluster.K8sVersion)
			return out
		}
	}
	for _, n := range lz.EnvNames() {
		if e, ok := lz.Env(n); ok {
			add(e.Cluster.K8sVersion)
		}
	}
	return out
}
