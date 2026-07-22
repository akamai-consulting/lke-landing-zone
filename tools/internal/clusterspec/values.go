package clusterspec

import (
	"bytes"
	"fmt"
	"strconv"

	yaml "gopkg.in/yaml.v3"
)

// values.go renders the apl-core backend: a deployment's component toggles flip
// apps.<key>.enabled in the committed apl-values/<env>/values.yaml, and the
// spec-owned identity + platform settings (cluster.name/domainSuffix,
// dns.domainFilters, otomi.has*), the object-store wiring (Loki/Harbor bucket
// names + S3 endpoint/region), the values-repo coordinates (otomi.git
// repoUrl/username/branch), plus per-component sizing are written in. All of
// these RESOLVE their ${...} placeholders from the spec before Terraform runs —
// so landingzone.yaml is the single source and the cluster-bootstrap
// templatefile() is left with only the genuine runtime secrets + the live
// coredns IP to substitute (loki_admin_password, apl_values_repo_password,
// linode_dns_token, coredns_cluster_ip). apl-core is a Helm umbrella whose
// bundled apps (prometheus, loki, harbor, kyverno, …) are switched inside ONE
// values.yaml — so unlike the manifest backend, this is not a resource selection
// but a targeted edit of existing keys.
//
// A landingzone.yaml spec is REQUIRED: the object-store + identity + repo keys
// are no longer filled by Terraform, so an instance that never runs `llz render`
// would ship values.yaml with literal "${loki_bucket_chunks}" strings. There is
// no non-spec path.
//
// values.yaml is hand-authored with load-bearing comments and ${...} placeholders;
// we parse + re-emit with yaml.v3's Node API, which preserves comments AND each
// scalar's quoting style (so the secret "${...}" placeholders left for Terraform
// stay quoted). Only the keys the spec owns are touched; everything else
// round-trips unchanged in meaning.

// ValuesIdentity carries the spec-derived settings RenderValues writes directly
// into a deployment's values.yaml, resolving every non-secret ${...} placeholder
// from the spec before Terraform's templatefile() runs. Build it with
// (*LandingZone).ValuesIdentity.
type ValuesIdentity struct {
	ClusterName  string // cluster.name (was ${cluster_name})
	DomainSuffix string // cluster.domainSuffix + dns.domainFilters[0] (was ${cluster_domain})
	ExternalDNS  bool   // otomi.hasExternalDNS
	ExternalIDP  bool   // otomi.hasExternalIDP

	// Object-store wiring — the Loki/Harbor bucket names + S3 endpoint/region.
	// Derived from the env name (== deployment) + spec.cluster.objectStorage.cluster,
	// mirroring the llz-object-storage module's naming; see objectStoreWiring.
	LokiBucketChunks string // apps.loki._rawValues.loki.storage.bucketNames.chunks (was ${loki_bucket_chunks})
	LokiBucketRuler  string // …bucketNames.ruler  (was ${loki_bucket_ruler})
	LokiBucketAdmin  string // …bucketNames.admin  (was ${loki_bucket_admin})
	LokiS3Endpoint   string // …s3.endpoint — bare host (was ${loki_s3_endpoint})
	LokiS3Region     string // …s3.region — OBJ cluster id (was ${loki_s3_region})
	HarborBucket     string // apps.harbor._rawValues.persistence.imageChartStorage.s3.bucket (was ${harbor_bucket})
	HarborS3Endpoint string // …s3.regionendpoint — full https URL (was ${harbor_s3_endpoint})
	HarborS3Region   string // …s3.region — OBJ cluster id (was ${harbor_s3_region})

	// Values-repo coordinates (otomi.git). URL is required; username/branch fall
	// back to the same defaults the cluster-bootstrap tfvars carried.
	RepoURL      string // otomi.git.repoUrl  (was ${apl_values_repo_url})
	RepoUsername string // otomi.git.username (was ${apl_values_repo_username})
	RepoBranch   string // otomi.git.branch   (was ${apl_values_repo_ref})

	// Alertmanager receiver wiring (spec.alerting, instance-wide). Receivers
	// replaces the base `alerts.receivers` list when non-empty; the channels
	// overwrite the base defaults only when set. The Slack webhook URL is NOT
	// values material — see the Alerting type.
	AlertReceivers        []string // alerts.receivers
	AlertSlackChannel     string   // alerts.slack.channel
	AlertSlackChannelCrit string   // alerts.slack.channelCrit

	// Teams (spec.teams, instance-wide) → a teamConfig.<name> entry in the
	// apl-values overlay. apl-core provisions the native team (namespace + the
	// Keycloak `team-<name>` group/role the OpenBao keycloak role binds on).
	Teams []Team
}

// objectStoreWiring returns the Loki/Harbor bucket names + S3 endpoint/region for
// a deployment, derived from the env name (== the object-storage region_suffix ==
// deployment) and the Linode OBJ cluster id. It MIRRORS the llz-object-storage
// module's "<label_prefix>-<bucket>-<region_suffix>" naming (label_prefix defaults
// to "platform") and its endpoint shape — the same derivation cluster-bootstrap's
// TF locals used before this moved into the render. If the module's label_prefix
// default changes, change objLabelPrefix here in lockstep.
func objectStoreWiring(env, objCluster string) (chunks, ruler, admin, lokiEndpoint, region, harborBucket, harborEndpoint string) {
	const objLabelPrefix = "platform"
	if objCluster == "" {
		return "", "", "", "", "", "", ""
	}
	host := objCluster + ".linodeobjects.com" // Loki wants the bare host…
	return objLabelPrefix + "-loki-chunks-" + env,
		objLabelPrefix + "-loki-ruler-" + env,
		objLabelPrefix + "-loki-admin-" + env,
		host,
		objCluster,
		objLabelPrefix + "-harbor-registry-" + env,
		"https://" + host // …Harbor wants the full URL for regionendpoint
}

// ValuesIdentity resolves the values.yaml identity + platform + object-store +
// repo settings for env from the assembled spec (env identity is already merged
// with spec.defaults; the platform flags are instance-wide).
func (lz *LandingZone) ValuesIdentity(env string) ValuesIdentity {
	e, _ := lz.Env(env)
	b := e.Cluster.Bootstrap
	chunks, ruler, admin, lokiEndpoint, region, harborBucket, harborEndpoint :=
		objectStoreWiring(env, e.Cluster.ObjectStorage.Cluster)
	// otomi.git.username defaults to x-access-token (GitHub ignores the HTTPS
	// basic-auth username with a fine-grained PAT).
	username := b.AplValues.Username
	if username == "" {
		username = "x-access-token"
	}
	// otomi.git.branch defaults to a per-env, apl-core-OWNED branch (apl-<env>) —
	// deliberately NOT main. apl-operator continuously commits its rendered values tree
	// + platform SealedSecrets to this branch on every reconcile (an additive,
	// pull-rebase-then-push commit — NOT a force-push; see the ADR's settled
	// push-semantics note), so it must be isolated from main, which holds
	// the human-authored IaC + apl-values source and must stay PR-only /
	// branch-protectable. Each env gets its OWN branch (apl-lab, apl-primary, …) so
	// parallel envs never share apl-core state on one branch. apl-operator self-creates
	// the branch on first commit (checkout -B + push -u origin), so it need not
	// pre-exist. Override via spec.cluster.bootstrap.aplValues.revision. The default
	// lives on Bootstrap.AplValuesBranch so the wedge guard in Validate compares the
	// same value this renders. See docs/designs/apl-core-values-branch-isolation.md.
	branch := b.AplValuesBranch(env)
	// otomi.git.repoUrl defaults to the instance repo itself — the same literal
	// the copier-rendered tfvars example carried
	// ("https://github.com/<@ instance_repo @>.git"). Without this an env whose
	// spec omits aplValues.repoURL keeps the ${apl_values_repo_url} placeholder
	// in its committed values.yaml, and cluster-bootstrap's secrets-only
	// templatefile() hard-fails on the unknown variable (the release-e2e
	// regression this default fixes). Left empty only when spec.instance.repo is
	// also unset — which Validate rejects.
	repoURL := b.AplValues.RepoURL
	if repoURL == "" && lz.Spec.Instance.Repo != "" {
		repoURL = "https://github.com/" + lz.Spec.Instance.Repo + ".git"
	}
	return ValuesIdentity{
		ClusterName:  b.Name,
		DomainSuffix: b.DomainSuffix,
		ExternalDNS:  lz.Spec.Defaults.Platform.HasExternalDNS(),
		ExternalIDP:  lz.Spec.Defaults.Platform.HasExternalIDP(),

		LokiBucketChunks: chunks,
		LokiBucketRuler:  ruler,
		LokiBucketAdmin:  admin,
		LokiS3Endpoint:   lokiEndpoint,
		LokiS3Region:     region,
		HarborBucket:     harborBucket,
		HarborS3Endpoint: harborEndpoint,
		HarborS3Region:   region,

		RepoURL:      repoURL,
		RepoUsername: username,
		RepoBranch:   branch,

		AlertReceivers:        lz.Spec.Alerting.Receivers,
		AlertSlackChannel:     lz.Spec.Alerting.Slack.Channel,
		AlertSlackChannelCrit: lz.Spec.Alerting.Slack.ChannelCrit,

		Teams: lz.Spec.Teams,
	}
}

// RenderValues returns base (an apl-core values.yaml) with apps.<key>.enabled set
// from the component toggles (each component's AplCoreApps are enabled iff the
// component is) and the spec-owned identity + platform keys set from id. Apps not
// present in base, or with no enabled key, are left alone; a spec-owned key absent
// from base is skipped (never invented).
func RenderValues(base []byte, components map[string]ComponentToggle, id ValuesIdentity) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(base, &doc); err != nil {
		return nil, fmt.Errorf("parse values.yaml: %w", err)
	}
	if len(doc.Content) == 0 {
		return nil, fmt.Errorf("values.yaml is empty")
	}
	root := doc.Content[0]
	apps := mapValue(root, "apps")
	if apps == nil {
		return nil, fmt.Errorf("values.yaml has no apps: section")
	}

	// Desired enabled state per apl-core app key (later components win, but the
	// registry is disjoint on AplCoreApps so order is immaterial).
	want := map[string]bool{}
	for _, c := range Components {
		for _, app := range c.AplCoreApps {
			want[app] = ComponentEnabled(components, c.Name)
		}
	}
	for app, enabled := range want {
		appNode := mapValue(apps, app)
		if appNode == nil {
			continue // app not in this values.yaml — leave the template as-is
		}
		en := mapValue(appNode, "enabled")
		if en == nil {
			continue // no explicit enabled key (e.g. external-dns) — don't invent one
		}
		en.Tag = "!!bool"
		en.Value = boolString(enabled)
	}

	// Spec-owned identity + platform. Each is set only when the key already exists
	// (so a stripped-down values.yaml isn't grown new keys) and, for the string
	// identity, only when the spec provides a value.
	if cluster := mapValue(root, "cluster"); cluster != nil {
		setStr(mapValue(cluster, "name"), id.ClusterName)
		setStr(mapValue(cluster, "domainSuffix"), id.DomainSuffix)
	}
	if dns := mapValue(root, "dns"); dns != nil {
		if filters := mapValue(dns, "domainFilters"); filters != nil &&
			filters.Kind == yaml.SequenceNode && len(filters.Content) > 0 {
			setStr(filters.Content[0], id.DomainSuffix)
		}
	}
	if otomi := mapValue(root, "otomi"); otomi != nil {
		setBool(mapValue(otomi, "hasExternalDNS"), id.ExternalDNS)
		setBool(mapValue(otomi, "hasExternalIDP"), id.ExternalIDP)
		if git := mapValue(otomi, "git"); git != nil {
			setStr(mapValue(git, "repoUrl"), id.RepoURL)
			setStr(mapValue(git, "username"), id.RepoUsername)
			setStr(mapValue(git, "branch"), id.RepoBranch)
		}
	}

	// Object-store wiring — the Loki/Harbor bucket names + S3 endpoint/region,
	// derived from the spec (was cluster-bootstrap's TF locals + templatefile()).
	// Each is set only when its node already exists in the base, so a slimmed-down
	// values.yaml is never grown new structure.
	setStr(dig(apps, "loki", "_rawValues", "loki", "storage", "bucketNames", "chunks"), id.LokiBucketChunks)
	setStr(dig(apps, "loki", "_rawValues", "loki", "storage", "bucketNames", "ruler"), id.LokiBucketRuler)
	setStr(dig(apps, "loki", "_rawValues", "loki", "storage", "bucketNames", "admin"), id.LokiBucketAdmin)
	setStr(dig(apps, "loki", "_rawValues", "loki", "storage", "s3", "endpoint"), id.LokiS3Endpoint)
	setStr(dig(apps, "loki", "_rawValues", "loki", "storage", "s3", "region"), id.LokiS3Region)
	setStr(dig(apps, "harbor", "_rawValues", "persistence", "imageChartStorage", "s3", "bucket"), id.HarborBucket)
	setStr(dig(apps, "harbor", "_rawValues", "persistence", "imageChartStorage", "s3", "regionendpoint"), id.HarborS3Endpoint)
	setStr(dig(apps, "harbor", "_rawValues", "persistence", "imageChartStorage", "s3", "region"), id.HarborS3Region)

	// Per-component sizing (config in the spec, mechanism in the base). Each knob
	// overwrites an existing scalar in the base; unset knobs leave the base default.
	if o, ok := components["observability"]; ok {
		setStr(dig(apps, "prometheus", "retention"), o.Retention)
		setStr(dig(apps, "prometheus", "storageSize"), o.Storage)
		if o.Replicas != nil {
			setInt(dig(apps, "prometheus", "replicas"), *o.Replicas)
		}
	}
	if h, ok := components["harbor"]; ok {
		setStr(dig(apps, "harbor", "_rawValues", "persistence", "persistentVolumeClaim", "registry", "size"), h.RegistryStorage)
	}

	// Alertmanager receiver wiring (spec.alerting). Same never-invent rule: the
	// receivers list and slack channels are set only when the base carries the
	// alerts: block. An empty spec list keeps the base default (receivers:
	// [none] — Alertmanager runs with a null route until an operator opts in).
	if alerts := mapValue(root, "alerts"); alerts != nil {
		setStrSeq(mapValue(alerts, "receivers"), id.AlertReceivers)
		setStr(dig(alerts, "slack", "channel"), id.AlertSlackChannel)
		setStr(dig(alerts, "slack", "channelCrit"), id.AlertSlackChannelCrit)
	}

	// spec.teams → teamConfig.<name> (the ONE place this render invents structure,
	// vs. the never-invent scalar patches above — teams are a spec-owned structural
	// input apl-core turns into the native team + Keycloak team-<name> group/role).
	// No teams (an instance that never declared spec.teams) → the block is never
	// created, so a team-less instance's values.yaml is unchanged.
	applyTeamConfig(root, id.Teams)

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2) // match the hand-authored values.yaml
	if err := enc.Encode(&doc); err != nil {
		return nil, fmt.Errorf("encode values.yaml: %w", err)
	}
	_ = enc.Close()
	return buf.Bytes(), nil
}

// setStr overwrites a scalar node with a plain string literal. No-op when the node
// is absent (key not in base) or the value is empty (nothing to render).
func setStr(n *yaml.Node, val string) {
	if n == nil || val == "" {
		return
	}
	n.Kind = yaml.ScalarNode
	n.Tag = "!!str"
	n.Style = 0 // plain — drop any ${...}-placeholder quoting
	n.Value = val
}

// setStrSeq replaces a sequence node's items with plain string literals. No-op
// when the node is absent (key not in base) or vals is empty (keep the base
// default) — the sequence twin of setStr.
func setStrSeq(n *yaml.Node, vals []string) {
	if n == nil || len(vals) == 0 {
		return
	}
	n.Kind = yaml.SequenceNode
	n.Tag = "!!seq"
	n.Style = 0
	n.Content = n.Content[:0]
	for _, v := range vals {
		n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v})
	}
}

// setBool overwrites a scalar node with a bool literal. No-op when absent.
func setBool(n *yaml.Node, val bool) {
	if n == nil {
		return
	}
	n.Kind = yaml.ScalarNode
	n.Tag = "!!bool"
	n.Style = 0
	n.Value = boolString(val)
}

// setInt overwrites a scalar node with an int literal. No-op when absent.
func setInt(n *yaml.Node, val int) {
	if n == nil {
		return
	}
	n.Kind = yaml.ScalarNode
	n.Tag = "!!int"
	n.Style = 0
	n.Value = strconv.Itoa(val)
}

// dig walks a chain of mapping keys, returning the value node or nil if any level
// is missing (so a slimmed-down base never grows new structure).
func dig(n *yaml.Node, keys ...string) *yaml.Node {
	for _, k := range keys {
		n = mapValue(n, k)
		if n == nil {
			return nil
		}
	}
	return n
}

// mapValue returns the value node for key in a mapping node, or nil.
func mapValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// applyTeamConfig ensures a teamConfig.<name>.settings.id entry exists for each
// spec.teams entry, creating the teamConfig block only when at least one team is
// declared — so a team-less values.yaml is byte-identical to the un-rendered
// base. It ADDS missing teams only; an
// existing teamConfig.<name> (a human/apl-enriched entry) is left untouched, so
// re-render is idempotent and non-destructive. apl-core reads teamConfig to
// provision the native team (namespace + Keycloak group/role team-<name>).
func applyTeamConfig(root *yaml.Node, teams []Team) {
	if len(teams) == 0 {
		return
	}
	tc := mapValue(root, "teamConfig")
	if tc != nil && tc.Kind != yaml.MappingNode {
		// A malformed base (teamConfig: null/scalar/seq) — reset the value node to
		// an empty map IN PLACE so we don't append a duplicate teamConfig key.
		tc.Kind, tc.Tag, tc.Value, tc.Content = yaml.MappingNode, "!!map", "", nil
	}
	if tc == nil {
		tc = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		root.Content = append(root.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "teamConfig"}, tc)
	}
	for _, t := range teams {
		if mapValue(tc, t.Name) != nil {
			continue // respect an already-authored entry
		}
		// { settings: { id: <name> } } — the minimal valid apl-core team.
		idScalar := func(v string) *yaml.Node {
			return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
		}
		settings := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map",
			Content: []*yaml.Node{idScalar("id"), idScalar(t.Name)}}
		team := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map",
			Content: []*yaml.Node{idScalar("settings"), settings}}
		tc.Content = append(tc.Content, idScalar(t.Name), team)
	}
}

func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
