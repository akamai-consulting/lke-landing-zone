package assertidentity

// openbaoext.go — the live half of spec.openbao, in two lanes: the kubernetes
// roles as `llz ci assert-openbao-extensions` (no human login, so it runs on
// adopter instances from llz-scheduled-checks), and the operator grants inside
// team-login-smoke (which holds the smoke team's Keycloak login).
//
// WHAT IT PROVES. bao-configure renders spec.openbao into mounts, policies and
// Kubernetes-auth roles; a renderer can be byte-perfect and the cluster still not
// hold what was declared (a step skipped as "already in use", a role written to
// the wrong mount, a policy shadowed). So this does what each CONSUMER does:
//
//   - for every declared kubernetesRole, mint a TokenRequest for the bound service
//     account, log in through THAT role, and ask OpenBao (sys/capabilities-self)
//     what the resulting token may do on every rule path its policies declare —
//     ground truth, not the policy text — that the token carries EXACTLY the
//     declared policies (capabilities-self only proves "at least": a role that
//     also carried platform-ci would hold every declared capability too), and that
//     each mount those rules sit under exists with the declared engine type;
//   - for every mount the smoke team operates, ask the same of the team's own
//     login token for every clusterspec.OperatorRules rule, require DENY on every
//     clusterspec.OperatorDeniedPaths entry (sys/mounts/<mount>, key export,
//     issuance, CA destruction), require that every spec-declared config path is
//     READ-ONLY to it, and read back each declared config write. It also warns
//     when the mount has no issuer on THIS cluster: staging the intermediate is a
//     post-bootstrap human step, done once per cluster of an HA pair.
//
// The expected set comes from the spec (the input), never from the cluster.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/openbao"
)

// probeSegment is clusterspec.ProbeSegment: the denied paths clusterspec builds
// and the rule probes built here must use the same stand-in.
const probeSegment = clusterspec.ProbeSegment

// probePath turns a rule path into a concrete path the rule matches.
func probePath(rule string) string {
	segs := strings.Split(rule, "/")
	for i, s := range segs {
		if s == "+" {
			segs[i] = probeSegment
		}
	}
	p := strings.Join(segs, "/")
	if strings.HasSuffix(p, "*") {
		p = strings.TrimSuffix(p, "*") + probeSegment
	}
	return p
}

// roleExpectation is what one declared kubernetes role must yield.
type roleExpectation struct {
	Role, Namespace, ServiceAccount string
	// Policies is the exact policy set the token must carry (besides default).
	Policies []string
	// Caps maps a concrete probe path to the capabilities it must carry.
	Caps map[string][]string
	// Mounts maps each mount the role's rules reach to its declared engine type.
	Mounts map[string]string
}

// roleExpectations derives, from the spec alone, what each role must yield.
func roleExpectations(ext clusterspec.OpenBaoExtensions) []roleExpectation {
	policies := map[string]clusterspec.OpenBaoPolicy{}
	for _, p := range ext.Policies {
		policies[p.Name] = p
	}
	mountType := map[string]string{}
	for _, m := range ext.Mounts {
		mountType[m.Path] = m.Type
	}
	var out []roleExpectation
	for _, r := range ext.KubernetesRoles {
		e := roleExpectation{Role: r.Name, Policies: r.Policies, Caps: map[string][]string{}, Mounts: map[string]string{}}
		if len(r.BoundServiceAccountNamespaces) > 0 && len(r.BoundServiceAccountNames) > 0 {
			e.Namespace, e.ServiceAccount = r.BoundServiceAccountNamespaces[0], r.BoundServiceAccountNames[0]
		}
		for _, pn := range r.Policies {
			for _, rule := range policies[pn].Rules {
				pp := probePath(rule.Path)
				e.Caps[pp] = unionCaps(e.Caps[pp], rule.Capabilities)
				if first, _, ok := strings.Cut(rule.Path, "/"); ok {
					if ty, declared := mountType[first]; declared {
						e.Mounts[first] = ty
					}
				}
			}
		}
		out = append(out, e)
	}
	return out
}

// operatorExpectation is what the smoke team's token must yield on one mount.
type operatorExpectation struct {
	Mount, Type string
	Caps        map[string][]string
	// ReadOnly are the spec-owned paths: read, and nothing that writes.
	ReadOnly []string
	// Config are the declared config writes to read back (path → declared data).
	Config []clusterspec.OpenBaoWrite
}

func operatorExpectations(ext clusterspec.OpenBaoExtensions, team string) []operatorExpectation {
	operated := map[string]bool{}
	for _, m := range ext.OperatorMountsFor(team) {
		operated[m] = true
	}
	var out []operatorExpectation
	for _, m := range ext.Mounts {
		if !operated[m.Path] {
			continue
		}
		caps := map[string][]string{}
		for _, r := range clusterspec.OperatorRules(m) {
			caps[probePath(r.Path)] = r.Capabilities
		}
		// Administer the intermediate, never disable/re-tune the mount, export or
		// import a key, issue a certificate, or destroy the CA.
		for _, p := range clusterspec.OperatorDeniedPaths(m) {
			caps[p] = []string{"deny"}
		}
		out = append(out, operatorExpectation{Mount: m.Path, Type: m.Type, Config: m.Config, Caps: caps,
			ReadOnly: clusterspec.SpecOwnedPaths(m)})
	}
	return out
}

// evalCapabilities is the pure judgement: every wanted capability must be held
// on its path; a wanted ["deny"] means the path must be denied outright. Fails
// closed on an empty expectation and on a path the answer does not cover.
func evalCapabilities(subject string, want, got map[string][]string) []string {
	if len(want) == 0 {
		return []string{subject + ": nothing to check — the expected capability set is empty"}
	}
	var problems []string
	for _, p := range sortedKeys(want) {
		have, ok := got[p]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s: no answer for %s", subject, p))
			continue
		}
		if len(want[p]) == 1 && want[p][0] == "deny" {
			if !(len(have) == 1 && have[0] == "deny") {
				problems = append(problems, fmt.Sprintf("SECURITY %s: %s must be denied, token holds %v", subject, p, have))
			}
			continue
		}
		for _, c := range want[p] {
			if c != "deny" && !containsString(have, c) {
				problems = append(problems, fmt.Sprintf("%s: %s lacks %q (holds %v)", subject, p, c, have))
			}
		}
	}
	return problems
}

// evalReadOnly: each spec-owned path must hold read and NOTHING that writes —
// evalCapabilities only proves "at least", which `roles/+` would satisfy. Pure.
func evalReadOnly(subject string, paths []string, got map[string][]string) []string {
	var problems []string
	for _, p := range paths {
		have, ok := got[p]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s: no answer for %s", subject, p))
			continue
		}
		if !containsString(have, "read") {
			problems = append(problems, fmt.Sprintf("%s: spec-owned %s is not readable (holds %v)", subject, p, have))
		}
		for _, c := range have {
			if c != "read" && c != "list" {
				problems = append(problems, fmt.Sprintf("SECURITY %s: spec-owned %s must be read-only, token holds %q", subject, p, c))
			}
		}
	}
	return problems
}

// issuerWarning is the HA-pair hint: a pki mount with no issuer on this cluster
// cannot issue, and staging one is a per-cluster human step. "" when fine. Pure.
func issuerWarning(mount string, issuers []string) string {
	if len(issuers) > 0 {
		return ""
	}
	return fmt.Sprintf("::warning::pki mount %s has no issuer on this cluster — it cannot issue until an operator stages the intermediate here (docs/runbooks/openbao-team-login.md; once per cluster of an HA pair)", mount)
}

// evalConfigReadback checks the declared booleans of a config write against what
// the mount returns. Only booleans are compared: OpenBao normalises durations
// ("720h" reads back as 2592000) and lists, so comparing those would test the
// normaliser, not the write. Presence of the path is the rest of the proof.
func evalConfigReadback(subject string, declared map[string]any, got map[string]any, ok bool) []string {
	if !ok {
		return []string{fmt.Sprintf("%s: declared config is absent (404) — the bao-configure write did not land", subject)}
	}
	var problems []string
	for _, k := range sortedKeys(declared) {
		want, isBool := declared[k].(bool)
		if !isBool {
			continue
		}
		if have, present := got[k].(bool); !present || have != want {
			problems = append(problems, fmt.Sprintf("%s: %s=%v declared, mount holds %v", subject, k, want, got[k]))
		}
	}
	return problems
}

// runRoleChecks is the kubernetes-role half, run as its own lane
// (assert-openbao-extensions) because it needs no human login: it runs on
// adopter instances from llz-scheduled-checks, not only in release-e2e.
//
// A bound service account that does not exist yet is the normal state between
// bootstrap and the app's first deploy, so it is a WARNING unless requireBound
// (the e2e lane, whose fixture binds the always-present ESO controller — there an
// absent account means the binding broke). Any other mint failure is an error.
func runRoleChecks(ctx context.Context, addr string, ext clusterspec.OpenBaoExtensions, requireBound bool) error {
	if len(ext.KubernetesRoles) == 0 {
		fmt.Println("No spec.openbao.kubernetesRoles declared — role checks skipped.")
		return nil
	}
	hc := openbao.HTTPClientLoopback(30 * time.Second)
	var problems []string
	verified := 0
	for _, e := range roleExpectations(ext) {
		subject := "kubernetes role " + e.Role
		jwt, err := mintServiceAccountTokenVia(caps.ExtW(), e.Namespace, e.ServiceAccount)
		if err != nil {
			if serviceAccountAbsent(err) && !requireBound {
				fmt.Printf("::warning::%s: %s/%s does not exist yet — not verified until the workload is deployed (%v)\n", subject, e.Namespace, e.ServiceAccount, err)
				continue
			}
			problems = append(problems, fmt.Sprintf("%s: mint %s/%s token: %v", subject, e.Namespace, e.ServiceAccount, err))
			continue
		}
		tok, err := openbao.KubernetesLogin(ctx, hc, addr, "kubernetes", e.Role, jwt)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: login as %s/%s failed — role absent or not bound to that service account: %v", subject, e.Namespace, e.ServiceAccount, err))
			continue
		}
		c := openbao.NewWithClient(addr, tok, "", hc)
		if got, err := c.TokenPolicies(ctx); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", subject, err))
		} else {
			problems = append(problems, evalPolicySet(subject, e.Policies, got)...)
		}
		problems = append(problems, checkCaps(ctx, c, subject, e.Caps)...)
		problems = append(problems, checkMounts(ctx, c, subject, e.Mounts)...)
		verified++
	}
	if len(problems) > 0 {
		return fmt.Errorf("spec.openbao kubernetes roles are not what OpenBao enforces:\n  - %s\nCheck: the bao-configure log for a failed (spec.openbao) step; `bao policy read <name>`; `bao read auth/kubernetes/role/<name>`; `bao secrets list`", strings.Join(problems, "\n  - "))
	}
	fmt.Printf("✓ spec.openbao: %d of %d kubernetes role(s) verified against what OpenBao enforces\n", verified, len(ext.KubernetesRoles))
	return nil
}

// serviceAccountAbsent: the TokenRequest failed because the service account (or
// its namespace) does not exist — kubectl reports `… "<name>" not found`. Pure.
func serviceAccountAbsent(err error) bool {
	return err != nil && strings.Contains(err.Error(), "not found")
}

// runOperatorChecks is the operator half, run inside team-login-smoke because it
// needs the smoke team's own Keycloak login token. team/teamToken are that login.
func runOperatorChecks(ctx context.Context, addr, team, teamToken string, ext clusterspec.OpenBaoExtensions) error {
	exps := operatorExpectations(ext, team)
	if len(exps) == 0 {
		fmt.Printf("Team %q operates no spec.openbao mount — operator checks skipped.\n", team)
		return nil
	}
	hc := openbao.HTTPClientLoopback(30 * time.Second)
	c := openbao.NewWithClient(addr, teamToken, "", hc)
	var problems []string
	for _, e := range exps {
		subject := fmt.Sprintf("team %s operating %s", team, e.Mount)
		problems = append(problems, checkCaps(ctx, c, subject, e.Caps)...)
		if len(e.ReadOnly) > 0 {
			if got, err := c.CapabilitiesSelf(ctx, e.ReadOnly); err != nil {
				problems = append(problems, fmt.Sprintf("%s: capabilities-self: %v", subject, err))
			} else {
				problems = append(problems, evalReadOnly(subject, e.ReadOnly, got)...)
			}
		}
		problems = append(problems, checkMounts(ctx, c, subject, map[string]string{e.Mount: e.Type})...)
		if issuers, err := c.List(ctx, e.Mount+"/issuers"); err != nil {
			problems = append(problems, fmt.Sprintf("%s: list issuers: %v", subject, err))
		} else if w := issuerWarning(e.Mount, issuers); w != "" {
			fmt.Println(w)
		}
		for _, w := range e.Config {
			got, ok, err := c.Read(ctx, e.Mount+"/"+w.Path)
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s: read back %s/%s: %v", subject, e.Mount, w.Path, err))
				continue
			}
			problems = append(problems, evalConfigReadback(subject+" "+w.Path, w.Data, got, ok)...)
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("spec.openbao operator grants are not what OpenBao enforces:\n  - %s\nCheck: `bao policy read <mount>-operator`; `bao read auth/keycloak/role/%s`", strings.Join(problems, "\n  - "), team)
	}
	fmt.Printf("✓ spec.openbao: team %q operates %d mount(s) exactly as declared\n", team, len(exps))
	return nil
}

func checkCaps(ctx context.Context, c *openbao.Client, subject string, want map[string][]string) []string {
	got, err := c.CapabilitiesSelf(ctx, sortedKeys(want))
	if err != nil {
		return []string{fmt.Sprintf("%s: capabilities-self: %v", subject, err)}
	}
	return evalCapabilities(subject, want, got)
}

func checkMounts(ctx context.Context, c *openbao.Client, subject string, mounts map[string]string) []string {
	var problems []string
	for _, m := range sortedKeys(mounts) {
		ty, ok, err := c.MountType(ctx, m)
		switch {
		case err != nil:
			problems = append(problems, fmt.Sprintf("%s: mount %s: %v", subject, m, err))
		case !ok:
			problems = append(problems, fmt.Sprintf("%s: mount %s does not exist", subject, m))
		case ty != mounts[m]:
			problems = append(problems, fmt.Sprintf("%s: mount %s is %s, declared %s", subject, m, ty, mounts[m]))
		}
	}
	return problems
}

// evalPolicySet: the token must carry exactly the declared policies. `default`
// is attached by OpenBao to every token and is ignored. Pure.
func evalPolicySet(subject string, declared, got []string) []string {
	want := map[string]bool{}
	for _, p := range declared {
		want[p] = true
	}
	have := map[string]bool{}
	var problems []string
	for _, p := range got {
		if p == "default" {
			continue
		}
		have[p] = true
		if !want[p] {
			problems = append(problems, fmt.Sprintf("SECURITY %s: token carries undeclared policy %q", subject, p))
		}
	}
	for _, p := range sortedKeys(want) {
		if !have[p] {
			problems = append(problems, fmt.Sprintf("%s: token lacks declared policy %q", subject, p))
		}
	}
	return problems
}

// unionCaps merges two rules' capabilities on one path. deny wins, as it does in
// OpenBao: a path some policy denies is denied whatever another grants.
func unionCaps(a, b []string) []string {
	if containsString(a, "deny") || containsString(b, "deny") {
		return []string{"deny"}
	}
	out := append([]string{}, a...)
	for _, c := range b {
		if !containsString(out, c) {
			out = append(out, c)
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
