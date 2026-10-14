package clusterspec

// objlabels.go — the per-instance prefix on every Object Storage bucket and key
// label.
//
// WHY IT IS PER-INSTANCE. Linode Object Storage bucket labels share ONE namespace
// per region, across accounts. The prefix used to be the module default,
// `platform`, hardcoded here and never plumbed through the object-storage root —
// so every LLZ instance in the world tried to create literally
// `platform-loki-chunks-<env>` and `platform-harbor-registry-<env>`. The first
// adopter to use a given deployment name in a region took those names globally;
// the next one's `apply-object-storage` died on
// `[400] The bucket 'platform-loki-chunks-lab' already exists`, with no
// remediation short of renaming the deployment — the root declared no
// `label_prefix` variable, so setting it in tfvars did nothing. Two teams in ONE
// GitHub org both running `llz env add lab` was enough.
//
// The module always had the knob (and its own comment said to "override it per
// sibling deployment so labels don't collide"); nothing passed it. Now the spec
// owns the value, `llz render` emits it into object-storage/<env>.tfvars, and
// every consumer derives from the same place.
//
// KEY labels carry it too. Those are per-account rather than global, so they
// cannot collide across adopters — but `llz reap` and the rotation table match
// keys by exact label, so two instances in one Linode account would reap and
// rotate each other's keys. One prefix governs both.

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// objLabelMaxLen is Linode/S3's bucket-label ceiling. Every label this prefix
// participates in must fit inside it.
const objLabelMaxLen = 63

// objLongestSuffix is the longest `-<name>-` infix the object-storage module
// appends between the prefix and the deployment name (`-harbor-registry-`).
// Length validation uses the longest one so the check is independent of which
// bucket happens to be examined.
const objLongestSuffix = "-harbor-registry-"

// objLabelRe is the S3-compatible bucket-label grammar Linode enforces:
// lowercase alphanumerics and hyphens, no leading or trailing hyphen.
var objLabelRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// ObjLabelPrefix is the effective prefix for this instance's bucket and key
// labels: the explicit spec.instance.objLabelPrefix when set, else the instance
// name sanitized into the label grammar.
//
// Derivation rather than a hard requirement, because metadata.name comes from the
// GitHub repo's short name (`llz new` seeds it from instance_repo) and may legally
// contain uppercase, dots or underscores — none of which are legal in a bucket
// label. `llz env add` writes the derived value into landingzone.yaml so the
// effective prefix is visible in the spec rather than implied by a function.
func (lz *LandingZone) ObjLabelPrefix() string {
	if p := strings.TrimSpace(lz.Spec.Instance.ObjLabelPrefix); p != "" {
		return p
	}
	return SanitizeObjLabelPrefix(lz.Metadata.Name)
}

// SanitizeObjLabelPrefix coerces an arbitrary instance name into the bucket-label
// grammar: lowercased, every illegal run collapsed to a single hyphen, ends
// trimmed, and bounded by ObjLabelPrefixMaxLen (which is a sanity bound, not a
// per-deployment budget — see there).
//
// Returns "" for input that sanitizes away to nothing; callers validate rather
// than substituting a default, because silently falling back to a shared prefix
// is the collision this file exists to prevent.
func SanitizeObjLabelPrefix(name string) string {
	var b strings.Builder
	prevHyphen := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevHyphen = false
		default:
			if !prevHyphen && b.Len() > 0 {
				b.WriteByte('-')
				prevHyphen = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if max := ObjLabelPrefixMaxLen(); len(out) > max {
		out = strings.Trim(out[:max], "-")
	}
	return out
}

// ObjLabelPrefixMaxLen bounds the sanitized prefix.
//
// Deliberately NOT "63 minus the longest suffix minus a maximum-length deployment
// name": that arithmetic yields 15 characters, which truncates ordinary instance
// names like `my-instance-repo` and would be a surprising silent rename of a
// cloud resource. Deployment names are capped at 31 by validate.EnvName but are
// realistically short, and validation runs with the SPEC in hand — so the length
// rule that matters is applied to the labels an instance actually produces
// (validateObjLabelLengths), where the error can name the offending deployment.
// This bound only stops an absurd prefix from reaching that check.
func ObjLabelPrefixMaxLen() int { return 40 }

// validateObjLabelPrefix checks the EFFECTIVE prefix (explicit or derived). It is
// reported against the field the operator can act on: an explicit value is their
// text, a derived one sends them to metadata.name or to setting the field.
func validateObjLabelPrefix(lz *LandingZone) []error {
	explicit := strings.TrimSpace(lz.Spec.Instance.ObjLabelPrefix)
	got := lz.ObjLabelPrefix()
	where := "spec.instance.objLabelPrefix"
	how := fmt.Sprintf("set %s explicitly", where)
	if explicit == "" {
		where = "metadata.name"
		how = fmt.Sprintf("rename the instance or set %s explicitly", "spec.instance.objLabelPrefix")
	}

	if got == "" {
		return []error{fmt.Errorf("%s yields an empty Object Storage label prefix — bucket labels would collide with every other instance; %s "+
			"(lowercase letters, digits and hyphens)", where, how)}
	}
	var errs []error
	if !objLabelRe.MatchString(got) {
		errs = append(errs, fmt.Errorf("%s: Object Storage label prefix %q must be lowercase alphanumerics and hyphens, "+
			"not starting or ending with a hyphen (Linode bucket labels are S3-shaped); %s", where, got, how))
	}
	if max := ObjLabelPrefixMaxLen(); len(got) > max {
		errs = append(errs, fmt.Errorf("%s: Object Storage label prefix %q is %d characters; the maximum is %d; %s",
			where, got, len(got), max, how))
	}
	// The rule that actually binds: every label this instance will create has to
	// fit Linode's limit. Checked per DECLARED deployment rather than against a
	// hypothetical maximum-length one, so the message names the real pair and a
	// short-named instance is not rejected for a deployment it will never have.
	errs = append(errs, validateObjLabelLengths(lz, got, where, how)...)
	return errs
}

// validateObjLabelLengths reports every prefix+deployment pair whose bucket label
// would exceed Linode's limit.
func validateObjLabelLengths(lz *LandingZone, prefix, where, how string) []error {
	if prefix == "" {
		return nil
	}
	var errs []error
	for _, env := range sortedEnvNames(lz) {
		// NOT length-checking the database label here. Its ceiling is genuinely
		// unknown — terraform-modules/llz-databases/main.tf records "the composed
		// label's maximum length" as UNVERIFIED and deliberately declines to guard it
		// with a guessed bound, because a wrong limit rejects valid labels. Reusing
		// the bucket's 63 would be exactly that guess: if the real ceiling is lower we
		// under-detect, if higher we block a legal spec at render. The widened prefix
		// does make long DB labels newly reachable — worth verifying the real API
		// limit and pinning it in the module, which is where the other DB label facts
		// live.
		label := prefix + objLongestSuffix + env
		if len(label) > objLabelMaxLen {
			errs = append(errs, fmt.Errorf("%s: bucket label %q for deployment %q is %d characters, over Linode's %d-character limit — "+
				"shorten the deployment name or %s", where, label, env, len(label), objLabelMaxLen, how))
		}
	}
	return errs
}

// sortedEnvNames keeps validation output deterministic (map iteration order).
func sortedEnvNames(lz *LandingZone) []string {
	names := make([]string, 0, len(lz.Spec.Environments))
	for n := range lz.Spec.Environments {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ObjLokiChunksBucket and ObjHarborRegistryBucket are THE derivations of the two
// bucket names a deployment writes to, shared with RenderObjOverlayEnv so a
// checker cannot drift from the buckets the object-storage module actually
// creates.
//
// They exist because the alternative was three invented environment variables
// (OBJ_ENDPOINT_HOST, LOKI_CHUNKS_BUCKET, HARBOR_REGISTRY_BUCKET) exported by
// nothing — a gate configured from values no workflow sets does not fail closed,
// it fails at the first flag check with an error about a missing argument.
//
// prefix is threaded rather than read from a package const: the const WAS the
// bug, because it silently agreed with a module default the root never passed.
func ObjLokiChunksBucket(prefix, env string) string {
	return objLabel(prefix, "loki-chunks", env)
}

func ObjHarborRegistryBucket(prefix, env string) string {
	return objLabel(prefix, "harbor-registry", env)
}

// ObjKeyLabels are the Object Storage KEY labels an instance mints per
// deployment. Kept here beside the bucket derivations so `llz reap` and the
// rotation table cannot drift from each other or from the prefix.
func ObjKeyLabels(prefix, env string) []string {
	// nil, not three empty strings: reapEnvObjKeys builds a match set from this,
	// and a "" entry would match any unlabelled key. objLabel already refuses to
	// guess; propagate that rather than wrapping it in a slice.
	if prefix == "" || env == "" {
		return nil
	}
	// THREE LABELS, THOUGH ONLY `obj` IS STILL MINTED. The per-app loki and
	// harbor-registry keys were retired — their ExternalSecrets had been deleted,
	// so nothing consumed them — but every cluster bootstrapped before that still
	// has both keys in the Linode account, and teardown is the only thing that
	// removes them. Dropping them here would leak two keys per destroyed
	// deployment against an account that caps at 100 (76 once piled up, which is
	// the incident this reaper exists for).
	//
	// So this list is the REAP set, which is a superset of the mint set by design.
	// TestEnvObjKeyLabelsMatchRotationTable holds that relation: everything minted
	// must be reaped, and anything reaped-but-not-minted must be named as retired.
	return []string{
		objLabel(prefix, "loki", env),
		objLabel(prefix, "harbor-registry", env),
		objLabel(prefix, "obj", env),
	}
}

// objLabel is the one place a label is assembled. Empty in, empty out — callers
// that pass an unresolved prefix or env get a value that fails loudly downstream
// rather than a plausible-looking label pointing at another instance's bucket.
func objLabel(prefix, name, env string) string {
	if prefix == "" || env == "" {
		return ""
	}
	return prefix + "-" + name + "-" + env
}

// ── values-repo identity ─────────────────────────────────────────────────────

// validateAplValuesRepo rejects an aplValues.repoURL that names anything other
// than this instance's own repo.
//
// Everything downstream of ValuesIdentity.RepoURL targets the INSTANCE repo and
// only works there: platform-bootstrap's repoURL is hardcoded to it, the
// AppProject sourceRepos allowlist holds only it plus the template (so a carved
// App pointing elsewhere is REJECTED by Argo rather than misrouted), the ArgoCD
// repository credential is minted for it alone, and configureManagedApl
// force-pushes the apl-<env> branch to it. The same value also becomes GH_REPO
// for the harbor robot provisioner and the broad-PAT rotator, both of which
// publish secrets the instance repo's own workflows then read.
//
// So a divergent value has no working configuration behind it — it silently
// retargets five consumers at a repo that cannot serve them. Reject rather than
// ignore: silently dropping a field an adopter deliberately set is the next bug.
// Setting it to the instance repo's own URL is fine and renders identically to
// leaving it unset.
func validateAplValuesRepo(lz *LandingZone) []error {
	if lz.Spec.Instance.Repo == "" {
		return nil // Validate already reports the missing instance repo
	}
	want := lz.Spec.Instance.Repo
	var errs []error
	for _, name := range sortedEnvNames(lz) {
		got := strings.TrimSpace(lz.Spec.Environments[name].Cluster.Bootstrap.AplValues.RepoURL)
		if got == "" || sameRepoURL(got, want) {
			continue
		}
		errs = append(errs, fmt.Errorf("environments.%s.cluster.bootstrap.aplValues.repoURL (%q) names %q, but this instance is %q — "+
			"the carved Argo Applications, the AppProject sourceRepos allowlist, the ArgoCD repo credential and the apl-%s branch "+
			"apl-operator writes are all scoped to spec.instance.repo, so another repo is rejected by Argo rather than served. "+
			"Point it at this instance (any host) or leave it unset", name, got, repoPath(got), want, name))
	}
	return errs
}

// sameRepoURL reports whether two git remote URLs name the same REPO, ignoring
// the spellings that do not change which repo it is: a trailing slash, the
// optional ".git", case, and — deliberately — the HOST.
//
// Host-blind because the host is the forge, not the identity. `spec.instance.repo`
// is an <owner>/<name> with no host, ValuesIdentity hardcodes github.com when it
// synthesizes a URL, and a GitHub Enterprise instance's repoURL is
// https://<appliance>/<owner>/<name>.git. Comparing full URLs made this validator
// reject every non-github.com instance outright — a permanent wall in front of
// the `forge: github-enterprise` path the repo already supports.
func sameRepoURL(a, b string) bool { return repoPath(a) == repoPath(b) }

// repoPath reduces a remote URL to "<owner>/<name>", lowercased. Handles the
// https and scp-like ssh spellings; anything it cannot parse is returned trimmed
// so two identical unparseable strings still compare equal.
func repoPath(u string) string {
	u = strings.TrimSuffix(strings.TrimSpace(strings.ToLower(u)), "/")
	u = strings.TrimSuffix(u, ".git")
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
	}
	if i := strings.Index(u, "@"); i >= 0 { // ssh user, or scp-like git@host:owner/name
		u = u[i+1:]
	}
	u = strings.ReplaceAll(u, ":", "/") // scp-like separator
	parts := strings.Split(strings.Trim(u, "/"), "/")
	if len(parts) >= 2 {
		return parts[len(parts)-2] + "/" + parts[len(parts)-1]
	}
	return u
}

// LabelPrefixFor loads the instance spec and returns its label prefix, for the CI
// verbs that take a deployment name rather than an already-loaded spec.
//
// IT CAME DOWN FROM internal/extensions/objenc, and it is the clearest single case
// for the rule that nothing in extensions/ should be imported by extensions/.
// `credrotate` imported the whole object-encryption capability for this one
// function and used nothing else from it; `teardown` and `assertobjstore` did the
// same alongside a few protocol helpers. None of them wants object encryption.
// They want to know which prefix namespaces this instance's buckets — which is a
// question about the SPEC, and this is the package that answers those.
//
// THE PREFIX namespaces every bucket and key label an instance creates. It USED to
// be the constant `platform` in a handful of places, which is precisely why every
// instance collided on the same global bucket names — so the replacement must not
// have a "just use platform" fallback hiding in it. Resolution is from the spec or
// nothing: these commands all run inside the instance checkout, and a wrong prefix
// here does not fail, it points rotation and teardown at ANOTHER instance's
// buckets and keys.
func LabelPrefixFor(what string) (string, error) {
	lz, err := LoadInstance(".")
	if err != nil {
		return "", errObjPrefixUnresolved(what, err)
	}
	p := lz.ObjLabelPrefix()
	if p == "" {
		return "", errObjPrefixUnresolved(what, nil)
	}
	return p, nil
}

func errObjPrefixUnresolved(what string, cause error) error {
	if cause != nil {
		//lint:ignore ST1005 multi-line operator diagnostic: the trailing period closes an embedded remediation line, not a sentence fragment
		return fmt.Errorf("%s needs the instance's Object Storage label prefix, which comes from the LandingZone spec: %w\n"+
			"  Run it from the instance root (the spec is landingzone.yaml).", what, cause)
	}
	//lint:ignore ST1005 multi-line operator diagnostic: the trailing period closes an embedded remediation line, not a sentence fragment
	return fmt.Errorf("%s needs the instance's Object Storage label prefix, but the spec yields an empty one.\n"+
		"  Set spec.instance.objLabelPrefix in landingzone.yaml.", what)
}
