package identityconfig

// openbao_ext_prune.go — revoking what spec.openbao no longer declares.
//
// WHY. bao-configure only ever WROTE spec.openbao objects, so deleting an entry
// from the spec revoked nothing: a removed kubernetesRoles entry kept logging its
// service account in with the old grants, indefinitely, and no check could tell
// it from a declared one. For a security surface that is the wrong default — the
// spec reads as the declared state, so removing a grant from it must remove it.
//
// HOW IT KNOWS WHAT IS ITS OWN. OpenBao policies and kubernetes roles carry no
// owner metadata, and an adopter may hold hand-made ones that LLZ must never
// touch. So bao-configure records what it created in a MANIFEST and prunes only
// from that record: names in the previous manifest that the current spec no
// longer declares. A name it never recorded is never deleted, whatever it is
// called. The manifest lives in OpenBao itself, as the comment lines of a
// deny-only policy (extManifestPolicy): the only store the root-holding run can
// both read and write through the BaoAdmin handle (its kv fence keeps KV out of
// reach by construction), and one that is per-cluster exactly as the objects are.
//
// WHAT IT NEVER DOES. It never disables a mount: that destroys the CA's keys and
// every certificate record, which is a decision for a human. A mount dropped from
// the spec is reported (::warning::) and left; with its policies and roles
// revoked it grants nothing but root access anyway. It never deletes a reserved
// platform name or a team -reader/-writer policy, even if a tampered manifest
// lists one.
//
// FIRST RUN. With no manifest yet (every instance before this change), nothing
// is pruned — the previous state is unknown — and the manifest is written. An
// instance that has never declared spec.openbao reads "no manifest" and writes
// nothing, so its OpenBao is unchanged.

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
)

// extManifestPolicy names the manifest. Reserved in clusterspec so no spec entry
// can overwrite it.
const extManifestPolicy = clusterspec.OpenBaoManifestPolicy

// extManifest is the set of objects bao-configure created for spec.openbao.
type extManifest struct {
	Policies, Roles, Mounts []string
}

func (m extManifest) empty() bool {
	return len(m.Policies) == 0 && len(m.Roles) == 0 && len(m.Mounts) == 0
}

// desiredManifest is what the current spec makes bao-configure create — the
// same names openbaoExtensionSteps writes (TestManifestCoversEveryExtensionStep
// holds the two together). Pure.
func desiredManifest(ext clusterspec.OpenBaoExtensions) extManifest {
	var m extManifest
	for _, mnt := range ext.Mounts {
		m.Mounts = append(m.Mounts, mnt.Path)
		if len(mnt.Operators) > 0 {
			m.Policies = append(m.Policies, clusterspec.OperatorPolicyName(mnt.Path))
		}
	}
	for _, p := range ext.Policies {
		m.Policies = append(m.Policies, p.Name)
	}
	for _, r := range ext.KubernetesRoles {
		m.Roles = append(m.Roles, r.Name)
	}
	sort.Strings(m.Policies)
	sort.Strings(m.Roles)
	sort.Strings(m.Mounts)
	return m
}

const manifestHeader = "# llz spec.openbao manifest — written by `llz ci bao-configure`; do not edit.\n" +
	"# What bao-configure created for spec.openbao, so an entry removed from the\n" +
	"# spec is revoked on the next run. The path below grants nothing.\n"

// renderManifest is the policy text. Pure.
func renderManifest(m extManifest) string {
	var b strings.Builder
	b.WriteString(manifestHeader)
	for _, kind := range []struct {
		key   string
		names []string
	}{{"policy", m.Policies}, {"role", m.Roles}, {"mount", m.Mounts}} {
		for _, n := range kind.names {
			fmt.Fprintf(&b, "# %s: %s\n", kind.key, n)
		}
	}
	b.WriteString(`path "` + extManifestPolicy + `/none" { capabilities = ["deny"] }` + "\n")
	return b.String()
}

var manifestLineRe = regexp.MustCompile(`^# (policy|role|mount): (\S+)$`)

// parseManifest reads renderManifest's output back. A record line whose name is
// not a valid extension name is an error, not a skip: the manifest decides what
// gets DELETED, so one that does not parse cleanly must stop the prune. Pure.
func parseManifest(text string) (extManifest, error) {
	var m extManifest
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		g := manifestLineRe.FindStringSubmatch(line)
		if g == nil {
			if strings.HasPrefix(line, "# policy:") || strings.HasPrefix(line, "# role:") || strings.HasPrefix(line, "# mount:") {
				return extManifest{}, fmt.Errorf("manifest line %q is malformed", line)
			}
			continue
		}
		if !clusterspec.ValidExtensionName(g[2]) {
			return extManifest{}, fmt.Errorf("manifest records %s %q, which is not a valid spec.openbao name", g[1], g[2])
		}
		switch g[1] {
		case "policy":
			m.Policies = append(m.Policies, g[2])
		case "role":
			m.Roles = append(m.Roles, g[2])
		case "mount":
			m.Mounts = append(m.Mounts, g[2])
		}
	}
	return m, nil
}

// prunePlan is what a run revokes, and the mounts it only reports.
type prunePlan struct {
	Policies, Roles, OrphanMounts []string
}

// planPrune: recorded before, not desired now. Reserved platform names and team
// -reader/-writer policies are never planned, whatever the manifest says. Pure.
func planPrune(prev, cur extManifest) prunePlan {
	minus := func(a, b []string, keep func(string) bool) []string {
		in := map[string]bool{}
		for _, x := range b {
			in[x] = true
		}
		var out []string
		for _, x := range a {
			if !in[x] && keep(x) {
				out = append(out, x)
			}
		}
		sort.Strings(out)
		return out
	}
	return prunePlan{
		Policies: minus(prev.Policies, cur.Policies, func(n string) bool {
			return !clusterspec.ReservedOpenBaoPolicies[n] && n != extManifestPolicy &&
				!strings.HasSuffix(n, "-reader") && !strings.HasSuffix(n, "-writer")
		}),
		Roles: minus(prev.Roles, cur.Roles, func(n string) bool {
			return !clusterspec.ReservedOpenBaoKubernetesRoles[n]
		}),
		OrphanMounts: minus(prev.Mounts, cur.Mounts, func(string) bool { return true }),
	}
}

// pruneSteps renders a plan. Roles first, so no login can mint a token naming a
// policy mid-delete. Fatal: a revocation that failed must fail the run. Pure.
func pruneSteps(p prunePlan) []baoConfigStep {
	var steps []baoConfigStep
	for _, r := range p.Roles {
		steps = append(steps, baoConfigStep{desc: "revoke kubernetes auth role " + r + " (removed from spec.openbao)", fatal: true,
			args: []string{"delete", "auth/kubernetes/role/" + r}})
	}
	for _, pol := range p.Policies {
		steps = append(steps, baoConfigStep{desc: "revoke policy " + pol + " (removed from spec.openbao)", fatal: true,
			args: []string{"policy", "delete", pol}})
	}
	return steps
}

// baoRunner is the slice of the BaoAdmin handle the prune needs.
type baoRunner interface {
	Run(pod, token, stdin string, args ...string) (stdout, stderr string, err error)
}

// noManifestRe is OpenBao's answer for a policy that does not exist.
var noManifestRe = regexp.MustCompile(`No policy named`)

// reconcileExtensionManifest revokes what spec.openbao no longer declares and
// records what it now does. Runs AFTER the configure steps succeeded, so a run
// that failed to apply the new spec never prunes against it. The manifest is
// written only after every revocation succeeded, so a failed revoke is retried
// on the next run rather than forgotten.
func reconcileExtensionManifest(bao baoRunner, pod, token string, ext clusterspec.OpenBaoExtensions) error {
	cur := desiredManifest(ext)
	out, errOut, err := bao.Run(pod, token, "", "policy", "read", extManifestPolicy)
	var prev extManifest
	switch {
	case err == nil:
		if prev, err = parseManifest(out); err != nil {
			return fmt.Errorf("spec.openbao manifest (%s) is unreadable, so nothing removed from the spec can be safely revoked: %w", extManifestPolicy, err)
		}
	case noManifestRe.MatchString(errOut + out):
		// First run, or an instance that never declared spec.openbao.
	default:
		return fmt.Errorf("read spec.openbao manifest (%s): %s", extManifestPolicy, strings.TrimSpace(firstNonEmpty(errOut, out)))
	}
	if prev.empty() && cur.empty() {
		return nil
	}

	plan := planPrune(prev, cur)
	for _, m := range plan.OrphanMounts {
		fmt.Fprintf(os.Stderr, "::warning::spec.openbao no longer declares mount %s/ — its policies and roles are revoked, but the mount is NOT disabled (that destroys its keys). If it is truly retired: `bao secrets disable %s/`.\n", m, m)
	}
	for _, step := range pruneSteps(plan) {
		o, e, err := bao.Run(pod, token, step.stdin, step.args...)
		if err != nil {
			return fmt.Errorf("%s: %s", step.desc, strings.TrimSpace(firstNonEmpty(e, o)))
		}
		fmt.Printf("%s: done\n", step.desc)
	}

	if cur.empty() {
		// Nothing declared any more: remove the record too, back to an OpenBao
		// with no trace of spec.openbao.
		if o, e, err := bao.Run(pod, token, "", "policy", "delete", extManifestPolicy); err != nil {
			return fmt.Errorf("delete spec.openbao manifest: %s", strings.TrimSpace(firstNonEmpty(e, o)))
		}
		return nil
	}
	if o, e, err := bao.Run(pod, token, renderManifest(cur), "policy", "write", extManifestPolicy, "-"); err != nil {
		return fmt.Errorf("write spec.openbao manifest: %s", strings.TrimSpace(firstNonEmpty(e, o)))
	}
	fmt.Printf("spec.openbao manifest recorded (%d policies, %d roles, %d mounts).\n", len(cur.Policies), len(cur.Roles), len(cur.Mounts))
	return nil
}
