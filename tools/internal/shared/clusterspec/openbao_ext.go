package clusterspec

// openbao_ext.go — spec.openbao: instance-declared OpenBao extensions.
//
// WHY THIS EXISTS. `llz ci bao-configure` writes a FIXED set of mounts, policies
// and Kubernetes-auth roles. An instance that needs one more — the motivating case
// is an application-owned PKI issuing tier (an OHTTP relay's mTLS client CA) —
// had to run its own script with the break-glass root token after bootstrap, outside
// every gate, and re-run it by hand after any re-configure. spec.openbao declares
// those additions so bao-configure applies them in the same root-holding run, and
// spec.openbao.mounts[].operators lets a declared team administer a pki mount's
// intermediate lifecycle with a team login instead of root (OperatorRules).
//
// WHAT IT DELIBERATELY CANNOT DO. Every field is structured rather than raw HCL or
// raw `bao write` argv, so the validator can refuse the things an extension must
// never reach:
//   - a policy rule outside a declared mount or a declared team's KV subtree
//     (platform paths, sys/, auth/, another mount) — that would be a team-
//     claimable privilege escalation through the spec;
//   - `sudo` / `root` capabilities;
//   - a Kubernetes role carrying a PLATFORM policy (platform-ci, eso-pusher, …)
//     or reusing a platform role name — bao-configure would overwrite the one or
//     be overwritten by the other;
//   - a config write that RETURNS key material (pki root/intermediate generation,
//     issue/sign). Those responses would land in the bootstrap
//     log. The per-type allowlist below admits only configuration endpoints.
//
// LIFECYCLE matches spec.teams: bao-configure is a root-holding bootstrap step, so a
// change here reaches a cluster on the next bootstrap or deliberate re-configure
// dispatch, not on render.

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// OpenBaoExtensions is spec.openbao.
type OpenBaoExtensions struct {
	// Mounts are additional secrets engines (pki) enabled at their path.
	Mounts []OpenBaoMount `json:"mounts,omitempty"`
	// Policies are additional ACL policies, as structured rules.
	Policies []OpenBaoPolicy `json:"policies,omitempty"`
	// KubernetesRoles are additional roles on the `kubernetes` auth mount.
	KubernetesRoles []OpenBaoKubernetesRole `json:"kubernetesRoles,omitempty"`
}

// Empty reports whether nothing is declared.
func (e OpenBaoExtensions) Empty() bool {
	return len(e.Mounts) == 0 && len(e.Policies) == 0 && len(e.KubernetesRoles) == 0
}

// OpenBaoMount is one secrets engine.
type OpenBaoMount struct {
	// Path is the mount path, lowercase kebab, no slashes (e.g. "pki-ohttp-relay").
	Path string `json:"path"`
	// Type is the engine. Only "pki" — see allowedTypes for why not transit.
	Type string `json:"type"`
	// Description is shown in `bao secrets list`.
	Description string `json:"description,omitempty"`
	// MaxLeaseTTL caps every lease/certificate issued under the mount (e.g.
	// "26280h"). For pki it must cover the intermediate's validity.
	MaxLeaseTTL string `json:"maxLeaseTTL,omitempty"`
	// Config is written under the mount after it is enabled, in order. Paths are
	// relative to the mount and restricted to configuration endpoints (see
	// mountConfigAllow).
	Config []OpenBaoWrite `json:"config,omitempty"`
	// Operators are spec.teams names whose `llz openbao login --team <name>` token
	// gets full administration of THIS mount (policy OperatorPolicyName(path)) —
	// e.g. importing a root-signed intermediate — without the root token.
	Operators []string `json:"operators,omitempty"`
}

// OpenBaoWrite is one `bao write <mount>/<path>` with a JSON body.
type OpenBaoWrite struct {
	Path string         `json:"path"`
	Data map[string]any `json:"data,omitempty"`
}

// OpenBaoPolicy is one ACL policy.
type OpenBaoPolicy struct {
	Name  string              `json:"name"`
	Rules []OpenBaoPolicyRule `json:"rules"`
}

// OpenBaoPolicyRule is one `path "<path>" { capabilities = [...] }` stanza.
type OpenBaoPolicyRule struct {
	Path         string   `json:"path"`
	Capabilities []string `json:"capabilities"`
}

// OpenBaoKubernetesRole is one role on the `kubernetes` auth mount.
type OpenBaoKubernetesRole struct {
	Name                          string   `json:"name"`
	BoundServiceAccountNames      []string `json:"boundServiceAccountNames"`
	BoundServiceAccountNamespaces []string `json:"boundServiceAccountNamespaces"`
	// Policies must name spec.openbao.policies entries — never a platform policy.
	Policies []string `json:"policies"`
	// TTL of issued tokens; default 15m, max 1h.
	TTL string `json:"ttl,omitempty"`
}

// EffectiveTTL is the role's token TTL with the default applied.
func (r OpenBaoKubernetesRole) EffectiveTTL() string {
	if r.TTL == "" {
		return "15m"
	}
	return r.TTL
}

// OperatorPolicyName is the policy bao-configure writes for a mount's operators
// and attaches to each operator team's keycloak role. Shared by the writer
// (identityconfig) and the live gate (assertidentity) so the two cannot disagree.
func OperatorPolicyName(mount string) string { return mount + "-operator" }

// OperatorRules is the operator policy for a mount: the intermediate-CA
// lifecycle and day-2 administration, as an ALLOWLIST. Shared by the writer
// (identityconfig renders it) and the live gate (assertidentity probes every rule
// and every OperatorDeniedPaths entry), so the two cannot disagree.
//
// SCAR: this was `<mount>/*` with create/read/update/delete/list. On a pki mount
// that is root/generate/exported (a private key in the response), sign-verbatim
// (any certificate), config/ca and keys/import (a key from outside), and
// `DELETE <mount>/root` (every issuer and key, irrecoverably) — full CA custody
// handed to every member of a team, when the motivating flow needs a CSR and an
// import. Returns nil for any engine but pki.
//
// SPEC-OWNED CONFIG IS READ-ONLY. Every path the mount's config[] writes gets an
// exact `[read]` rule, which outranks `roles/+` (OpenBao prefers the rule with no
// wildcard). Otherwise an operator could rewrite a declared role to
// allow_any_name — operators cannot issue, but the workload granted
// issue/<role> then mints whatever it asks for, until the next re-configure
// silently resets it. Operators own the roles they create; spec owns its own.
func OperatorRules(m OpenBaoMount) []OpenBaoPolicyRule {
	if m.Type != "pki" {
		return nil
	}
	p := m.Path + "/"
	rw := []string{"create", "read", "update"}
	rules := []OpenBaoPolicyRule{
		// Stage an intermediate whose key never leaves OpenBao, then import the
		// root-signed certificate. `generate/exported` is deliberately absent.
		{Path: p + "intermediate/generate/internal", Capabilities: []string{"create", "update"}},
		{Path: p + "intermediate/set-signed", Capabilities: []string{"create", "update"}},
		// Inspect and re-point issuers/keys (default issuer, usage, names) — no
		// delete, and `+` stops at one segment so issuer/<ref>/sign-* stay denied.
		{Path: p + "issuers", Capabilities: []string{"list"}},
		{Path: p + "issuer/+", Capabilities: []string{"read", "update"}},
		{Path: p + "keys", Capabilities: []string{"list"}},
		{Path: p + "key/+", Capabilities: []string{"read", "update"}},
		{Path: p + "roles", Capabilities: []string{"list"}},
		{Path: p + "roles/+", Capabilities: []string{"create", "read", "update", "delete"}},
		{Path: p + "certs", Capabilities: []string{"list"}},
		{Path: p + "cert/+", Capabilities: []string{"read"}},
		{Path: p + "revoke", Capabilities: []string{"create", "update"}},
		{Path: p + "tidy", Capabilities: []string{"create", "update"}},
		{Path: p + "tidy-status", Capabilities: []string{"read"}},
		{Path: p + "crl/rotate", Capabilities: []string{"read"}},
	}
	// The config endpoints spec may write, plus config/keys (default key). Not
	// config/ca: that imports a key from outside.
	for _, c := range []string{"crl", "auto-tidy", "urls", "cluster", "issuers", "keys"} {
		rules = append(rules, OpenBaoPolicyRule{Path: p + "config/" + c, Capabilities: rw})
	}
	for _, path := range SpecOwnedPaths(m) {
		replaced := false
		for i := range rules {
			if rules[i].Path == path { // one stanza per path: narrow it in place
				rules[i].Capabilities, replaced = []string{"read"}, true
			}
		}
		if !replaced {
			rules = append(rules, OpenBaoPolicyRule{Path: path, Capabilities: []string{"read"}})
		}
	}
	return rules
}

// SpecOwnedPaths are the full API paths the mount's config[] writes — read-only
// to operators (OperatorRules), checked as such by the live gate.
func SpecOwnedPaths(m OpenBaoMount) []string {
	var out []string
	for _, w := range m.Config {
		out = append(out, m.Path+"/"+w.Path)
	}
	return out
}

// ProbeSegment stands in for a `+` or trailing `*` wherever a concrete path is
// needed to ask about a rule: OpenBao answers sys/capabilities-self for concrete
// paths, and a wildcard rule answers for any path it matches.
const ProbeSegment = "_llz_probe"

// caCustodyPaths are the pki endpoints that return or import key material, sign
// anything a role does not constrain, or destroy the CA — concrete paths
// (ProbeSegment for a variable segment), including the multi-issuer aliases
// (issuers/generate/…, issuer/<ref>/sign-…) that do the same under another name.
// Neither an operator nor a workload policy may write to any of them.
func caCustodyPaths(mount string) []string {
	m, x := mount+"/", ProbeSegment
	return []string{
		m + "root",
		m + "root/generate/internal",
		m + "root/generate/exported",
		m + "root/sign-intermediate",
		m + "root/sign-self-issued",
		m + "intermediate/generate/exported",
		m + "issuers/generate/root/internal",
		m + "issuers/generate/root/exported",
		m + "issuers/generate/intermediate/exported",
		m + "sign-verbatim",
		m + "sign-verbatim/" + x,
		m + "issuer/" + x + "/sign-verbatim",
		m + "issuer/" + x + "/sign-intermediate",
		m + "issuer/" + x + "/sign-self-issued",
		m + "issuer/" + x + "/sign-revocation-list",
		m + "config/ca",
		m + "keys/import",
		m + "issuers/import/bundle",
	}
}

// OperatorDeniedPaths are concrete paths an operator token must be DENIED: the
// mount's own administration, every CA-custody endpoint, and issuance (an
// operator stages the CA; workloads issue from it). The live gate asks OpenBao
// about each; a unit test holds OperatorRules to matching none of them.
func OperatorDeniedPaths(mnt OpenBaoMount) []string {
	if mnt.Type != "pki" {
		return nil
	}
	m, x := mnt.Path+"/", ProbeSegment
	out := []string{"sys/mounts/" + mnt.Path}
	out = append(out, caCustodyPaths(mnt.Path)...)
	return append(out,
		m+"issue/"+x,
		m+"sign/"+x,
		m+"issuer/"+x+"/issue/"+x,
		m+"issuer/"+x+"/sign/"+x,
	)
}

// PolicyPathMatches reports whether an ACL rule path matches a concrete path,
// with OpenBao's semantics: `+` is exactly one segment, and a trailing `*` is a
// prefix match that may cross `/` (so `pki/certs*` matches `pki/certs/x`).
func PolicyPathMatches(rule, path string) bool {
	prefix, glob := strings.CutSuffix(rule, "*")
	rs, ps := strings.Split(prefix, "/"), strings.Split(path, "/")
	if !glob {
		if len(rs) != len(ps) {
			return false
		}
		for i := range rs {
			if rs[i] != "+" && rs[i] != ps[i] {
				return false
			}
		}
		return true
	}
	if len(ps) < len(rs) {
		return false
	}
	for i := 0; i < len(rs)-1; i++ {
		if rs[i] != "+" && rs[i] != ps[i] {
			return false
		}
	}
	return strings.HasPrefix(ps[len(rs)-1], rs[len(rs)-1])
}

// writeCapabilities are the capabilities that change state. A rule holding only
// read/list/deny on a custody path cannot take custody.
var writeCapabilities = map[string]bool{"create": true, "update": true, "patch": true, "delete": true}

// OperatorMountsFor returns the mounts the named team operates, sorted.
func (e OpenBaoExtensions) OperatorMountsFor(team string) []string {
	var out []string
	for _, m := range e.Mounts {
		for _, o := range m.Operators {
			if o == team {
				out = append(out, m.Path)
			}
		}
	}
	sort.Strings(out)
	return out
}

// ReservedOpenBaoMounts are mount paths OpenBao or LLZ already owns.
var ReservedOpenBaoMounts = map[string]bool{
	"secret": true, "sys": true, "auth": true, "identity": true, "cubbyhole": true,
}

// ReservedOpenBaoPolicies are the policy names bao-configure writes itself (plus
// OpenBao's built-ins). Drift-checked against the real configure sequence by
// identityconfig's TestReservedOpenBaoNamesCoverConfigureSteps; team
// `<name>-reader` / `<name>-writer` and mount `-operator` names are refused by
// suffix in ValidateOpenBao.
var ReservedOpenBaoPolicies = map[string]bool{
	"root": true, "default": true, OpenBaoManifestPolicy: true,
	"platform-ci": true, "secret-propagator": true, "eso-pusher": true,
	"linode-rotator": true, "harbor-provisioner": true, "reconciler-read": true,
	"broad-pat-rotator": true,
}

// OpenBaoManifestPolicy is the deny-only policy whose comment lines record what
// bao-configure created for spec.openbao, so a removed entry can be revoked
// (identityconfig/openbao_ext_prune.go). Reserved: a spec policy of this name
// would overwrite the record of what may be deleted.
const OpenBaoManifestPolicy = "llz-spec-openbao-manifest"

// ValidExtensionName reports whether n is a well-formed spec.openbao mount,
// policy or role name.
func ValidExtensionName(n string) bool { return extNameRe.MatchString(n) }

// ReservedOpenBaoKubernetesRoles are the kubernetes-auth roles bao-configure
// writes itself. Drift-checked the same way.
var ReservedOpenBaoKubernetesRoles = map[string]bool{
	"eso": true, "eso-pusher": true, "harbor-provisioner": true,
	"reconciler": true, "broad-pat-rotator": true,
}

var (
	extNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	// A rule path: kebab/underscore segments, optional trailing `*`, `+` segments.
	rulePathRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*(/([a-z0-9_-]+|\+))*(/\*|\*)?$`)
	k8sNameRe   = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)
	allowedCaps = map[string]bool{"create": true, "read": true, "update": true, "patch": true, "delete": true, "list": true, "deny": true}
	// pki only. transit shipped in the first cut with no consumer and no live
	// gate: nothing exercised an enable, a keys/<k> write, or a re-run against an
	// existing key (where a changed `type` is silently kept), and confirming the
	// key type would need `bao read` through the admin handle, which would breach
	// its kv fence. Add an engine here together with a lane that proves it.
	allowedTypes = map[string]bool{"pki": true}
)

// mountConfigAllow is the per-engine allowlist of config-write paths. Anything
// that generates or returns key material (pki root/ & intermediate/ generation,
// issue/, sign/) is absent by design: its response
// would be printed into the bootstrap log, and it is an operator action anyway.
var mountConfigAllow = map[string][]*regexp.Regexp{
	"pki": {
		regexp.MustCompile(`^roles/[a-z0-9][a-z0-9_-]*$`),
		// No config/acme: ACME endpoints are unauthenticated, so enabling them from
		// spec opens an issuance path that bypasses every policy declared here.
		regexp.MustCompile(`^config/(crl|auto-tidy|urls|cluster|issuers)$`),
	},
}

// ValidateOpenBao checks spec.openbao against the declared teams. Pure.
func ValidateOpenBao(e OpenBaoExtensions, teams []Team) []error {
	var errs []error
	teamByName := map[string]Team{}
	for _, t := range teams {
		teamByName[t.Name] = t
	}
	mounts := map[string]OpenBaoMount{}
	for i, m := range e.Mounts {
		at := fmt.Sprintf("openbao.mounts[%d]", i)
		switch {
		case !extNameRe.MatchString(m.Path):
			errs = append(errs, fmt.Errorf("%s.path %q must be lowercase kebab with no slashes", at, m.Path))
		case ReservedOpenBaoMounts[m.Path] || m.Path == "kubernetes" || m.Path == "jwt" || m.Path == "keycloak":
			errs = append(errs, fmt.Errorf("%s.path %q is reserved (OpenBao or LLZ owns it)", at, m.Path))
		case mounts[m.Path].Path != "":
			errs = append(errs, fmt.Errorf("%s.path %q is declared twice", at, m.Path))
		}
		if !allowedTypes[m.Type] {
			errs = append(errs, fmt.Errorf("%s.type %q unsupported (want pki)", at, m.Type))
		}
		if m.MaxLeaseTTL != "" {
			if _, err := time.ParseDuration(m.MaxLeaseTTL); err != nil {
				errs = append(errs, fmt.Errorf("%s.maxLeaseTTL %q is not a duration (e.g. 26280h)", at, m.MaxLeaseTTL))
			}
		}
		for j, w := range m.Config {
			if !configPathAllowed(m.Type, w.Path) {
				errs = append(errs, fmt.Errorf("%s.config[%d].path %q is not an allowed %s configuration endpoint (key-generating and issuing endpoints are operator actions, not spec)", at, j, w.Path, m.Type))
			}
		}
		for j, o := range m.Operators {
			if _, ok := teamByName[o]; !ok {
				errs = append(errs, fmt.Errorf("%s.operators[%d] %q is not a spec.teams name", at, j, o))
			}
		}
		if m.Path != "" {
			mounts[m.Path] = m
		}
	}

	policies := map[string]bool{}
	for i, p := range e.Policies {
		at := fmt.Sprintf("openbao.policies[%d]", i)
		switch {
		case !extNameRe.MatchString(p.Name):
			errs = append(errs, fmt.Errorf("%s.name %q must be lowercase kebab", at, p.Name))
		case ReservedOpenBaoPolicies[p.Name] || strings.HasSuffix(p.Name, "-reader") ||
			strings.HasSuffix(p.Name, "-writer") || strings.HasSuffix(p.Name, "-operator"):
			errs = append(errs, fmt.Errorf("%s.name %q is reserved (bao-configure writes platform, team -reader/-writer and mount -operator policies itself)", at, p.Name))
		case policies[p.Name]:
			errs = append(errs, fmt.Errorf("%s.name %q is declared twice", at, p.Name))
		}
		policies[p.Name] = true
		if len(p.Rules) == 0 {
			errs = append(errs, fmt.Errorf("%s has no rules", at))
		}
		for j, r := range p.Rules {
			rat := fmt.Sprintf("%s.rules[%d]", at, j)
			if !rulePathRe.MatchString(r.Path) || strings.Contains(r.Path, "..") {
				errs = append(errs, fmt.Errorf("%s.path %q is not a canonical policy path", rat, r.Path))
			} else if !ruleInScope(r.Path, mounts, teams) {
				errs = append(errs, fmt.Errorf("%s.path %q is outside every declared mount and team subtree (secret/data|metadata/<subtree>/…) — extensions may not grant platform, sys/ or auth/ paths", rat, r.Path))
			} else if !workloadWriteAllowed(r, mounts) {
				errs = append(errs, fmt.Errorf("%s.path %q writes to the mount outside issuance — a workload may only write issue/<role>, sign/<role> or issuer/<ref>/{issue,sign}/<role> (reads and lists are unrestricted); everything else is CA administration, which belongs to operators", rat, r.Path))
			}
			if len(r.Capabilities) == 0 {
				errs = append(errs, fmt.Errorf("%s has no capabilities", rat))
			}
			for _, c := range r.Capabilities {
				if !allowedCaps[c] {
					errs = append(errs, fmt.Errorf("%s capability %q not allowed (sudo/root are refused)", rat, c))
				}
				if c == "deny" && len(r.Capabilities) > 1 {
					// deny overrides everything on the path, so the others are
					// dead text that reads like a grant.
					errs = append(errs, fmt.Errorf("%s mixes deny with other capabilities — deny overrides them; use [deny] alone", rat))
				}
			}
		}
	}

	roles := map[string]bool{}
	for i, r := range e.KubernetesRoles {
		at := fmt.Sprintf("openbao.kubernetesRoles[%d]", i)
		switch {
		case !extNameRe.MatchString(r.Name):
			errs = append(errs, fmt.Errorf("%s.name %q must be lowercase kebab", at, r.Name))
		case ReservedOpenBaoKubernetesRoles[r.Name]:
			errs = append(errs, fmt.Errorf("%s.name %q is reserved (bao-configure writes it)", at, r.Name))
		case roles[r.Name]:
			errs = append(errs, fmt.Errorf("%s.name %q is declared twice", at, r.Name))
		}
		roles[r.Name] = true
		if len(r.BoundServiceAccountNames) == 0 || len(r.BoundServiceAccountNamespaces) == 0 {
			errs = append(errs, fmt.Errorf("%s must bind at least one service account name and namespace", at))
		}
		for _, n := range append(append([]string{}, r.BoundServiceAccountNames...), r.BoundServiceAccountNamespaces...) {
			if n == "*" || !k8sNameRe.MatchString(n) {
				errs = append(errs, fmt.Errorf("%s binds %q — wildcards and non-RFC1123 names are refused", at, n))
			}
		}
		if len(r.Policies) == 0 {
			errs = append(errs, fmt.Errorf("%s.policies is empty", at))
		}
		for _, p := range r.Policies {
			if !policies[p] {
				errs = append(errs, fmt.Errorf("%s.policies names %q, which is not a spec.openbao.policies entry (platform and team policies cannot be attached here)", at, p))
			}
		}
		if d, err := time.ParseDuration(r.EffectiveTTL()); err != nil || d <= 0 || d > time.Hour {
			errs = append(errs, fmt.Errorf("%s.ttl %q must be a duration in (0, 1h]", at, r.TTL))
		}
	}
	return errs
}

// workloadIssuanceRe is everything a workload may WRITE under a pki mount:
// issuance through a role, optionally pinned to an issuer. One segment each,
// `+` allowed, no trailing glob.
var workloadIssuanceRe = regexp.MustCompile(`^(issuer/([a-z0-9][a-z0-9_-]*|\+)/)?(issue|sign)/([a-z0-9][a-z0-9_-]*|\+)$`)

// workloadWriteAllowed: a rule under a declared mount that holds any write
// capability must be an issuance path (workloadIssuanceRe). Team-subtree rules
// (KV) and read/list/deny-only rules are not constrained here. Pure.
//
// SCAR, TWICE. The first cut checked only that a rule sat under a declared
// mount, so `pki-x/*` [create, update] — root/generate/exported, sign-verbatim,
// DELETE pki-x/root — validated onto a kubernetes role bound to any service
// account. The fix after that was a DENYLIST of custody endpoints, and it leaked
// on the next probe: `roles/evil` [create] plus `issue/evil` is sign-verbatim by
// another name, and issuer/+, intermediate/set-signed, config/issuers and
// issuers/import/cert all still validated. A workload needs to issue; every
// other write on a CA is administration, so this is an allowlist.
func workloadWriteAllowed(r OpenBaoPolicyRule, mounts map[string]OpenBaoMount) bool {
	writes := false
	for _, c := range r.Capabilities {
		writes = writes || writeCapabilities[c]
	}
	first, rest, _ := strings.Cut(r.Path, "/")
	if _, ok := mounts[first]; !writes || !ok {
		return true
	}
	return workloadIssuanceRe.MatchString(rest)
}

func configPathAllowed(engine, p string) bool {
	if strings.Contains(p, "..") {
		return false
	}
	for _, re := range mountConfigAllow[engine] {
		if re.MatchString(p) {
			return true
		}
	}
	return false
}

// ruleInScope: a rule must sit under a declared mount, or under a declared team's
// KV v2 data/metadata subtree.
func ruleInScope(p string, mounts map[string]OpenBaoMount, teams []Team) bool {
	first := p
	if i := strings.IndexByte(p, '/'); i >= 0 {
		first = p[:i]
	}
	if _, ok := mounts[first]; ok && first != p {
		return true
	}
	for _, t := range teams {
		sub := strings.TrimPrefix(t.OpenbaoSubtree, "secret/")
		for _, kind := range []string{"secret/data/", "secret/metadata/"} {
			if strings.HasPrefix(p, kind+sub+"/") {
				return true
			}
		}
	}
	return false
}
