package main

// ci_plaintext_guard.go implements `llz ci plaintext-guard` — the static gate on
// UNENCRYPTED in-cluster communication.
//
// The problem it solves is drift, not any single hop. An audit of in-cluster
// traffic found credentials crossing the pod network in cleartext (the Harbor
// admin password in a Basic-auth header; Keycloak's JWKS, i.e. the signing keys
// OpenBao validates team logins with) alongside a set of scrapes and probes
// whose plaintext is a considered trade. Nothing distinguished the two, and
// nothing stopped the set growing: a new `scheme: http` on a ServiceMonitor, or
// a new `InsecureSkipVerify: true`, passed every gate in the repo silently.
//
// So: every plaintext hop in the tree must be REGISTERED here with a reason and
// an owner. Adding one becomes a reviewed decision instead of an invisible one,
// and the registry doubles as the auditor-facing list of accepted residuals —
// the thing docs/adr/0010-in-cluster-mtls.md describes in prose.
//
// UNUSED ENTRIES FAIL. A registry that keeps entries for hops that no longer
// exist is how an allowlist rots into a rubber stamp: the next reader cannot
// tell which lines are load-bearing. When a hop is secured, its line goes.
//
// SCANNER SCOPE. It detects four written shapes — `scheme: http` on a scrape
// endpoint, `insecureSkipVerify: true`, an http:// URL naming an in-cluster
// Service, `InsecureSkipVerify: true` in Go — plus mesh policy that accepts
// cleartext, cleartext wire protocols other than HTTP, and one shape nobody
// wrote: a scrape endpoint with NO `scheme:` key, which prometheus-operator
// defaults to http (see scanDefaultedScrapeScheme). That last one matters
// because every other pattern here reads a decision somebody typed, and the
// cheapest way to add a plaintext scrape is to type nothing.
//
// It does NOT see plaintext container ports or probe endpoints — a listener that
// simply serves HTTP on a port is invisible to it. Those are real hops
// (kubelet→/healthz is one) and they belong in the ADR, but a scanner that tried
// to infer them from a port number would be guessing.
//
// WHAT THIS DOES NOT DO. There is no ValidatingAdmissionPolicy twin, unlike
// ci_wave_health_guard.go. A VAP rejecting plaintext ServiceMonitors would apply
// to apl-core's resources too — cert-manager's, Loki's, everything the platform
// installs — and would fail them closed at admission. This guard is scoped to
// what THIS repo ships, which is what this repo can actually fix.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/guardkit"
)

// plaintextRule is why one hop is allowed to stay unencrypted.
type plaintextRule struct {
	// reason must say what crosses the wire, not merely that it is allowed —
	// "otelcol_* internal counters" is reviewable; "accepted" is not.
	reason string
	// owner names who could actually close it: "llz" (us — so it is a TODO with
	// our name on it), "apl-core", "upstream-<project>", or "inherent" for hops
	// no party can close.
	owner string
}

// plaintextAllowed registers every accepted unencrypted hop, keyed by
// "<file-relative-path>:<locator>" where locator identifies the finding within
// the file (a ServiceMonitor endpoint port, a Go symbol, a values key).
//
// Keys are path-based on purpose. A kind/name key would collide across the
// chart-templated resources, and the path is what a reviewer needs to go read.
var plaintextAllowed = map[string]plaintextRule{
	// ── ours, surviving the mTLS cutover (PR #360, ADR 0010) ─────────────
	//
	// #360 has MERGED, and the forcing function this block was built around did its
	// job: every entry that named a hop the cutover secured went stale and failed
	// the guard on the rebase, and was deleted (the reconciler + OpenBao scrapes,
	// now mTLS; the Keycloak JWKS fetch, now https) or re-keyed (HTTPClientInsecure
	// → HTTPClientLoopback). What remains below is what the cutover did NOT close,
	// each kept for a stated reason rather than for lack of attention.
	"kubernetes-charts/llz-openbao-platform/values.yaml:http://loki-gateway.monitoring.svc.cluster.local": {
		owner: "llz",
		reason: "OpenBao audit-log shipping from the promtail sidecar. Values are hashed by " +
			"OpenBao, but request paths, operations and auth.display_name ship in clear. RE-KEYED: " +
			"this pointed at llz-observability, where nothing creates a loki-gateway Service, so the " +
			"pipeline was BROKEN as well as plaintext. Repaired to `monitoring`, where apl-core " +
			"actually runs Loki — confirmed by apl-core's own otel-operator shipping to " +
			"http://loki-gateway.monitoring/otlp. The TLS half of ADR 0010's instruction (\"whoever " +
			"repairs the URL must give it TLS at the same time\") is NOT satisfied and cannot be from " +
			"here: the gateway is nginx serving plain HTTP with no TLS material from apl-core, so " +
			"https:// would connect to nothing. This closes when the hop is meshed — i.e. when " +
			"llz-openbao and monitoring are both enrolled in ambient",
	},
	"tools/cmd/llz/ci_harbor_provisioner.go:http://harbor-core.harbor.svc.cluster.local": {
		owner: "llz",
		reason: "the harbor-robot-provisioner's REST base. Carries the Harbor ADMIN PASSWORD in a " +
			"Basic-auth header and receives freshly minted ROBOT SECRETS in the response — the " +
			"sharpest cleartext edge in the cluster. #360 PUT the pod in the Istio mesh, so the " +
			"sidecar upgrades this to mTLS on the wire (harbor-core has no client-cert auth, so " +
			"direct HTTPS would be TLS and not mTLS); the URL stays http:// by design once meshed",
	},
	"platform-apl/components/harbor/harbor-robot-provisioner/cronjob.yaml:http://harbor-core.harbor.svc.cluster.local": {
		owner: "llz",
		reason: "HARBOR_API_URL — the manifest half of the ci_harbor_provisioner.go hop above, " +
			"added by #360 itself. Same payload (Harbor admin password out, robot secrets back) and " +
			"the same justification: this CronJob's pod is deliberately IN the istio mesh, so the " +
			"sidecar upgrades the hop to mTLS. Registered separately because the guard keys on file " +
			"path, and a reviewer reading the manifest should find the reasoning here too",
	},
	"tools/cmd/llz/ci_bootstrap_cluster.go:http://git-server.git-server.svc.cluster.local": {
		owner: "llz",
		reason: "aplGiteaInClusterURL — apl-core's in-cluster Gitea values repo, used to re-seed a " +
			"missing apl-values branch. NOT mesh-upgraded: giteaSourceFromCloneCmd injects the Gitea " +
			"USERNAME AND PASSWORD into this URL (basicAuthGitURL) and the migration Job that clones " +
			"it sets sidecar.istio.io/inject=false on purpose (batch pod), so git sends that " +
			"credential as an HTTP Basic header in clear, and the whole values tree comes back in " +
			"clear. Arrived with the migration fix (854d627c), AFTER #360 drew the mTLS line. Not " +
			"closed here because apl-core deploys git-server with httproute.enabled=false and no TLS " +
			"listener, so closing it means meshing the Job or terminating TLS at git-server — " +
			"neither is a rebase-sized change. Reachable only on a re-bootstrap whose destination " +
			"branch has gone missing, which narrows exposure but does not remove it",
	},

	// ── mesh policy: harbor's PeerAuthentication ─────────────────────────────
	//
	// harbor-peerauthentication.yaml ships three documents, ALL currently
	// `mode: PERMISSIVE`, plus the portLevelMtls exemptions that become meaningful
	// when they are flipped to STRICT. Every one of them is an accepted plaintext
	// hop in exactly the sense this registry exists to record, and they were
	// invisible until the guard learned to read mesh policy.
	//
	// CORRECTION: an earlier revision of this block asserted harbor WAS STRICT. It
	// was not, and never has been on any cluster. The policy carried portLevelMtls
	// on a selector-less document, which Istio's CRD rejects by CEL rule
	// ("portLevelMtls requires selector"), so the apiserver refused every apply and
	// the resource existed nowhere — the namespace ran the mesh default, PERMISSIVE,
	// throughout. Splitting it into a namespace-wide document plus workload-scoped
	// ones is what made it appliable; it is deliberately appliable-and-PERMISSIVE
	// first, because ADR 0010 step 3's measurement of who would break under STRICT
	// has never been able to run.
	//
	// THIS IS THE ENTRY THAT GUARDS THE GUARD, so read it before deleting it. The
	// two harbor http:// entries above are accepted because the provisioner's
	// sidecar upgrades that hop to mTLS. That still holds under PERMISSIVE — a
	// meshed client negotiates mTLS with a meshed server either way — so those two
	// entries remain true. What PERMISSIVE withholds is the guarantee that nothing
	// ELSE can speak cleartext to harbor-core, which is the difference between "our
	// client happens to use mTLS" and "harbor cannot be spoken to in cleartext".
	// Until the flip, that guarantee is not in place and this entry is what says so
	// out loud rather than letting the file's STRICT-shaped comments imply it.
	//
	// DELETE ALL THREE AT THE FLIP. They go STRICT together, these findings
	// disappear with them, and a stale registry entry is itself a guard failure —
	// which is what makes the cutover visible here instead of silent. They are
	// three entries and not one because each document governs a different set of
	// pods: registering them jointly would let one be flipped and another left
	// behind with nothing to say so.
	"platform-apl/components/harbor/harbor-peerauthentication.yaml:mtls-mode-harbor-strict-mtls": {
		owner: "llz",
		reason: "document 1, namespace-wide: every harbor pod no workload-scoped policy matches — in " +
			"practice harbor-robot-provisioner, which nothing dials INTO, so this document is the " +
			"lowest-risk third of the cutover. PERMISSIVE because ADR 0010 step 3's measurement of who " +
			"would break under STRICT has never been able to run: the pre-split policy was rejected by " +
			"the apiserver on every apply, so no cluster has ever had a PeerAuthentication here in " +
			"either mode. CLOSES with the other two when " +
			"istio_requests_total{connection_security_policy=\"none\"} reads zero for this namespace " +
			"across a full converge",
	},
	"platform-apl/components/harbor/harbor-peerauthentication.yaml:mtls-mode-harbor-otomi-db-mtls": {
		owner: "llz",
		reason: "document 2, the CNPG database pods — the riskiest third, and the reason the whole " +
			"rollout is staged. :8000 and :9187 are exempted below and stay exempt after the flip; :5432 " +
			"is NOT, and llz-cluster-foundation's networkPolicies.cnpg opens 5432 from the unmeshed " +
			"cnpg-system for \"operator connectivity checks\". If that client is real, flipping this " +
			"document STRICT wedges the Harbor DB (Instance Status Extraction Error -> ClusterIsNotReady " +
			"-> convergence stalls). Nobody has ever observed it either way because this resource has " +
			"never applied. CLOSES by reading the none-security metric for 5432 traffic from cnpg-system: " +
			"exempt the port first if it is there, then flip",
	},
	"platform-apl/components/harbor/harbor-peerauthentication.yaml:mtls-mode-harbor-app-mtls": {
		owner: "llz",
		reason: "document 3, every app=harbor pod (core, registry, jobservice, portal, redis, trivy). " +
			"Governs the hop harbor-robot-provisioner makes to harbor-core carrying the Harbor ADMIN " +
			"PASSWORD. That hop is STILL mTLS on the wire under PERMISSIVE — a meshed client negotiates " +
			"mTLS with a meshed server regardless — so the two harbor http:// entries above remain true; " +
			"what PERMISSIVE withholds is the prohibition on anyone ELSE speaking cleartext to these " +
			"pods, which is the difference between \"our client happens to use mTLS\" and \"harbor cannot " +
			"be spoken to in cleartext\". Also the document that decides llz-cert-automation's " +
			"haproxy-rebuild egress allow, which ci_mesh_egress_guard.go calls BELIEVED DEAD on the " +
			"strength of a STRICT that was never in force. CLOSES with the other two",
	},
	"platform-apl/components/harbor/harbor-peerauthentication.yaml:mtls-8000": {
		owner: "apl-core",
		reason: "CNPG operator (cnpg-system, NOT a meshed namespace) polling harbor-otomi-db's " +
			"instance-status endpoint. What crosses is Postgres cluster/instance state — primary " +
			"identity, replication and WAL position, readiness — not credentials. Cannot be closed " +
			"from this repo: it needs cnpg-system meshed, which is apl-core's call. Removing the " +
			"exemption instead of closing it wedges the Harbor DB (\"Instance Status Extraction " +
			"Error\" -> ClusterIsNotReady -> convergence stalls), so it must stay until the operator " +
			"side is meshed",
	},
	"platform-apl/components/harbor/harbor-peerauthentication.yaml:mtls-9187": {
		owner: "apl-core",
		reason: "Prometheus scraping harbor-otomi-db's CNPG metrics port. Payload is Postgres " +
			"exporter gauges — connection counts, replication lag, WAL position; no credentials and " +
			"no row data. MISSED by the first revision of this policy, which exempted :8000 and :8001 " +
			"but not this one: PodMonitor harbor/harbor-otomi-db selects these pods and scrapes port " +
			"`metrics` with NO `scheme:`, which Prometheus defaults to HTTP, and the `monitoring` " +
			"namespace carries no istio.io/rev label — so the hop is unmeshed cleartext. Had STRICT " +
			"ever applied with the old exemption set, it would have blackholed CNPG metrics. Closing " +
			"it needs a meshed Prometheus, which is apl-core's to make",
	},
	"platform-apl/components/harbor/harbor-peerauthentication.yaml:mtls-8001": {
		owner: "apl-core",
		reason: "apl-core's Prometheus (monitoring, unmeshed) scraping Harbor's :8001 metrics — the " +
			"exporter plus core/registry/jobservice. Payload is Harbor's own counters; no key " +
			"material. This is the SAME hop in kind as the registered cert-manager and " +
			"otel-collector scrapes, and it escaped notice only because it is spelled as a mesh " +
			"exemption instead of `scheme: http` — the coverage gap that motivated reading mesh " +
			"policy here. Closing it needs a meshed Prometheus or an mTLS scrape config, both " +
			"apl-core's to make",
	},

	// ── apl-core owned ───────────────────────────────────────────────────────
	"platform-apl/components/observability/cert-manager-servicemonitor.yaml:tcp-prometheus-servicemonitor": {
		owner: "apl-core",
		reason: "cert-manager is installed and configured by apl-core; its metrics listener serves " +
			"HTTP and LLZ ships no values for it. Payload is cert-manager counters plus certificate " +
			"expiry timestamps — no key material. RESOLVED (read against linode/apl-core @ bbecae1): " +
			"there is NO _rawValues escape hatch for cert-manager. Its helmfile release " +
			"(helmfile.d/helmfile-01.init.yaml.gotmpl:45-51) is a bare `<<: *default` and " +
			"values/cert-manager/cert-manager.gotmpl is the only values source; only prometheus, " +
			"alertmanager, grafana and tekton take _rawValues. So this CANNOT be closed from the " +
			"values repo, contrary to the earlier note here. Worse under sidecars: that same file " +
			"sets `podAnnotations.sidecar.istio.io/inject: \"false\"`, so even a meshed Prometheus " +
			"could not mTLS to it. It closes only under Istio AMBIENT, where that annotation is " +
			"inert and the pods enrol via the namespace — see docs/adr/0010-in-cluster-mtls.md",
	},

	// ── upstream limited ─────────────────────────────────────────────────────
	"platform-apl/components/observability/otel-collector-servicemonitor.yaml:monitoring": {
		owner: "upstream-opentelemetry-collector",
		reason: "the :8888 Service is created by the otel-operator from the CR, and its Prometheus " +
			"pull endpoint takes TLS only through the collector's thin service.telemetry support. " +
			"Payload is otelcol_* internals; breaking OTelCollectorMetricsTargetDown to encrypt a " +
			"queue gauge is a bad trade",
	},

	// ── payload on a message bus, not a transport ────────────────────────────
	"kubernetes-charts/llz-cert-automation/templates/eventsource.yaml:eventsource-secrets": {
		owner: "llz",
		reason: "the haproxy-tls rotation watch. This EventSource publishes the WHOLE watched " +
			"object onto the JetStream EventBus, so the cert-manager Secret's `data` — the HAProxy " +
			"TLS PRIVATE KEY — is serialized onto the bus, persisted by JetStream (replicas: 3), and " +
			"delivered to the Sensor. It is the only hop in this tree carrying private key material, " +
			"and it predates ADR 0010 without ever being considered by it. NOT believed to be " +
			"cleartext on the wire: Argo Events enables TLS by default for JetStream client and " +
			"route traffic using self-signed material it generates. That is an upstream default this " +
			"repo does not set, pin or verify, and the namespace is `istio-injection: disabled` " +
			"(namespace.yaml:23) so the mesh is never in this path either. " +
			"REMEDIATION, deliberately NOT applied blind: the Sensor consumes only " +
			"`.Input.body.resource.metadata.resourceVersion`, so the trigger never needs the Secret's " +
			"contents — the rebuild Workflow re-reads the cert from the apiserver under RBAC. " +
			"Watching the cert-manager `Certificate` instead of its Secret removes the key from the " +
			"bus with no loss of function. It needs ONE fact this repo does not have: the " +
			"Certificate's name (the Secret is created outside this tree, and Certificate name and " +
			"secretName are conventionally but not necessarily equal). Guessing it wrong makes the " +
			"trigger silently never fire, and the failure surfaces ~80 days later as an expired edge " +
			"certificate — strictly worse than the hop itself. Read " +
			"`kubectl -n <cert-manager-ns> get secret haproxy-tls -o " +
			"jsonpath='{.metadata.annotations.cert-manager\\.io/certificate-name}'` and switch it",
	},

	// ── ours, out of this guard's scope by construction ──────────────────────
	"tools/internal/openbao/openbao.go:HTTPClientLoopback": {
		owner: "inherent",
		reason: "re-keyed from HTTPClientInsecure, which #360 renamed. The unverified transport now " +
			"reaches ONLY loopback: the `kubectl port-forward` tunnel to 127.0.0.1 that `llz openbao " +
			"get/set/login` opens, and in-pod callers of OpenBao's own 127.0.0.1:8210 listener. " +
			"Nothing crosses a network, so there is no peer to authenticate and no traffic to " +
			"intercept. Pod-network callers can no longer reuse it even by mistake — the listener " +
			"requires and verifies a client cert, so an unverified transport fails the handshake",
	},
	"tools/cmd/llz/ci_wait.go:apiProbeClient": {
		owner: "inherent",
		reason: "probes the LKE-managed control-plane endpoint from OUTSIDE the cluster during " +
			"provisioning, before a cluster exists to have a PKI. Reads a status line; carries no " +
			"credential and its result authorizes nothing",
	},
}

// ── scanning ────────────────────────────────────────────────────────────────

type plaintextFinding struct {
	key, file, what string
	line            int
}

// Patterns are deliberately TOLERANT of YAML's spelling freedom. An anchored,
// case-sensitive, unquoted-only match is trivially evaded — `scheme: "http"`,
// `scheme: HTTP`, or a flow-style `{port: m, scheme: http}` all mean the same
// thing to Kubernetes and all slipped past the first revision of this guard.
// A gate that a reviewer can bypass by adding quotes is not a gate.
//
// `https` is excluded by the trailing class rather than by a negative lookahead
// (Go's RE2 has none): after `http` the next character must be whitespace, a
// comma, a closing brace, a quote, or end-of-line — `s` is none of those.
var (
	reSchemeHTTP   = regexp.MustCompile(`(?i)scheme:\s*["']?http["']?(?:[\s,}]|$)`)
	reInsecureYAML = regexp.MustCompile(`(?i)insecureSkipVerify:\s*["']?true["']?(?:[\s,}]|$)`)
	reInsecureGo   = regexp.MustCompile(`InsecureSkipVerify:\s*true`)
	reSvcHTTP      = regexp.MustCompile(`http://[a-z0-9.\-]+\.svc(\.cluster\.local)?`)
	rePortName     = regexp.MustCompile(`port:\s*["']?([A-Za-z0-9_.\-]+)`)

	// ── mesh-level plaintext ─────────────────────────────────────────────────
	//
	// An Istio PeerAuthentication `mode: PERMISSIVE` ACCEPTS cleartext alongside
	// mTLS, and a DestinationRule `tls.mode: DISABLE` turns encryption off outright.
	// Both declare an accepted unencrypted hop just as surely as `scheme: http`
	// does — they are simply spelled as mesh policy rather than as a URL or a
	// scrape scheme, which is why the first revision of this guard could not see
	// the two port exemptions in the harbor namespace.
	//
	// The VALUE carries the specificity, not the key: bare `mode:` is everywhere in
	// Kubernetes YAML (volume file modes, volumeMode), so matching it would be
	// noise. `UNSET` is deliberately NOT flagged — it means "inherit", so the hop it
	// describes belongs to whatever sets the mesh-wide default, not to this file.
	//
	// `mode:` is anchored to a line start / whitespace / brace so it cannot match
	// the TAIL of a longer key. Without that, `sslmode: disable` — a legitimate
	// (if unwise) Postgres setting — was reported as "mesh mTLS not enforced",
	// which is both a wrong diagnosis and a wrong owner to send it to.
	rePermissiveMTLS = regexp.MustCompile(`(?im)(?:^|[\s,{])mode:\s*["']?(PERMISSIVE|DISABLE)["']?(?:[\s,}]|$)`)
	// portLevelMtls keys each exemption as a QUOTED MAP KEY (`"8000":`), never as
	// `port: 8000`, so rePortName cannot see it. Without this the exemptions would
	// key on whatever unrelated `port:` line came last in the document.
	rePortMapKey = regexp.MustCompile(`^\s*["'](\d+)["']:\s*$`)
	// A document-level `mtls.mode:` carries no port, so every such finding in one
	// file used to collapse onto the single key `mtls-mode`. That is fine for a
	// file with one policy and wrong for harbor-peerauthentication.yaml, which
	// ships three — one registry entry would have vouched for all three, and
	// TestScanPlaintextKeysAreUniquePerFile exists to refuse exactly that. The
	// object's own name is what separates them, so track it: first bare `name:` per
	// document, which in every manifest here is metadata.name. Anchored with no
	// leading `-` so a `- name:` list entry (containers, ports) cannot claim it.
	reDocName = regexp.MustCompile(`^\s*name:\s*["']?([A-Za-z0-9_.\-]+)["']?\s*$`)

	// ── event-bus payloads ───────────────────────────────────────────────────
	//
	// An Argo Events `resource` EventSource publishes the WHOLE watched object
	// onto its EventBus. There is no field projection: upstream's guidance is to
	// use "the Data Filters available in the sensor", which act AFTER publication,
	// so everything the watch returns has already crossed the bus by then.
	//
	// When the watched resource is `secrets`, that means the Secret's `data` goes
	// on the bus — and for a cert-manager TLS Secret that is the PRIVATE KEY. It is
	// then persisted by JetStream (replicated), held in the EventSource and Sensor
	// pod memory, and readable by anything subscribed.
	//
	// None of the shapes above can see this. It is not a URL, not a scrape scheme,
	// not a mesh mode — it is a payload decision — which is exactly why it survived
	// the entire mTLS audit unnoticed. The transport may well be encrypted (Argo
	// Events turns TLS on by default for JetStream, with self-signed material it
	// generates itself), but that is an upstream default this repo neither sets nor
	// verifies, and it does not make a private key on a message bus a decision
	// anyone here made on purpose. That is what this registry is for.
	//
	// Anchored to the whole line so `resource: secrets` is matched as the
	// EventSource's resource selector and not as prose in a comment or a longer
	// key such as `subresource: secrets-foo`.
	reEventSourceSecrets = regexp.MustCompile(`(?i)^\s*resource:\s*["']?secrets["']?\s*$`)

	// ── cleartext wire protocols other than HTTP ────────────────────────────
	//
	// The guard began as an HTTP gate, which quietly implied that HTTP is where
	// cleartext lives. It is not: this platform runs CNPG Postgres (harbor-otomi-db,
	// keycloak-db) and Redis, and a plaintext DSN or a `sslmode=disable` carries
	// credentials and row data in the clear with no `http://` anywhere in sight.
	//
	// The TLS-bearing spellings are excluded by construction rather than by a
	// negative lookahead (RE2 has none): after `redis` the next character must be
	// `:`, so `rediss://` cannot match, and likewise for amqps/ldaps/mongodb+srv.
	reCleartextProto = regexp.MustCompile(`(?i)\b(redis|postgres|postgresql|mysql|mongodb|amqp|ldap|memcached)://`)
	// A Postgres DSN that disables TLS outright. Spelled as a URI parameter
	// (sslmode=disable) or as a YAML/config key (sslmode: disable).
	reSSLModeDisable = regexp.MustCompile(`(?i)sslmode\s*[=:]\s*["']?disable`)

	// ── insecure-TLS spellings the first revision did not match ─────────────
	//
	// reInsecureGo matches only the composite-literal form `InsecureSkipVerify:
	// true`. These are the same decision written differently, and a gate that a
	// reviewer clears by assigning instead of initialising is not a gate.
	reInsecureGoAssign = regexp.MustCompile(`InsecureSkipVerify\s*=\s*true`)
	// client-go's rest.Config spells it `Insecure`, not `InsecureSkipVerify`, so
	// every kubeconfig-building path was invisible to the original pattern.
	reInsecureGoRest = regexp.MustCompile(`\bInsecure:\s*true`)
	// Command-line opt-outs, wherever they appear — including inside a YAML
	// container command/args block, which the walker already reads as text.
	reInsecureCLI = regexp.MustCompile(`--insecure-skip-tls-verify|--no-check-certificate|--plain-http|\bcurl\b[^\n]*\s-k(\s|$)`)
	// YAML/Helm keys that mean "do not verify TLS". Two families: keys that are
	// unsafe when TRUE (insecure, skipVerify) and keys that are unsafe when FALSE
	// (sslVerify, tlsVerify, verify_ssl).
	reInsecureYAMLTrue  = regexp.MustCompile(`(?i)\b(insecure|skipVerify|tlsSkipVerify|insecureSkipTLSVerify):\s*["']?true(?:[\s,}]|$)`)
	reInsecureYAMLFalse = regexp.MustCompile(`(?i)\b(sslVerify|tlsVerify|verify_ssl):\s*["']?false(?:[\s,}]|$)`)

	// ── short-form in-cluster URLs ───────────────────────────────────────────
	//
	// reSvcHTTP only sees the FULLY-QUALIFIED spelling. Kubernetes DNS also
	// resolves `svc.namespace` and a bare `svc` within the same namespace, and those
	// are the idiomatic forms — so the gate was bypassable by writing the shorter
	// name, which is the spelling a reviewer is most likely to reach for.
	//
	// The host is captured WITHOUT userinfo or port so the registry key stays stable
	// when a port is added or a password rotates. Templated hosts
	// (`http://{{ .Values.x }}`) do not match at all: `{` is outside the class. That
	// is a real blind spot, left open on purpose — the alternative is matching
	// `http://` unconditionally, which floods the gate with external URLs.
	reAnyHTTPHost = regexp.MustCompile(`http://(?:[^\s"'/@]+@)?([A-Za-z0-9.\-]+)`)
)

// publicLastLabels are final DNS labels that mark a host as PUBLIC, so a dotted
// name ending in one is not an in-cluster Service. This is a deny-list rather
// than an allow-list of cluster names because namespaces are unbounded: any new
// namespace must be caught WITHOUT touching this file, while the set of public
// suffixes this repo actually references is small and changes rarely.
//
// `local` is deliberately ABSENT: the `.svc.cluster.local` form is reSvcHTTP's,
// and leaving `local` out means an odd `foo.cluster.local` still gets flagged
// rather than silently passing as public.
var publicLastLabels = map[string]bool{
	"com": true, "org": true, "net": true, "io": true, "dev": true, "app": true,
	"cloud": true, "ai": true, "co": true, "uk": true, "edu": true, "gov": true,
	"me": true, "sh": true, "run": true, "info": true, "biz": true,
}

// inClusterHTTPHost decides whether a bare http:// host is a cluster-local
// Service. It errs toward NOT flagging: a false positive on an external URL
// trains reviewers to bypass the gate, which costs more than the hop it would
// have caught.
func inClusterHTTPHost(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if h == "" || h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return false
	}
	labels := strings.Split(h, ".")
	last := labels[len(labels)-1]
	// An IP literal is not a Service name. Testing only the LAST label is enough:
	// no DNS name may end in an all-numeric label, so this cannot reject a Service.
	if last != "" && strings.Trim(last, "0123456789") == "" {
		return false
	}
	// A single label resolves only inside the cluster (localhost is out above).
	if len(labels) == 1 {
		return true
	}
	return !publicLastLabels[last]
}

// shortInClusterURL finds the first short-form in-cluster http:// URL on a line,
// skipping the fully-qualified spelling so reSvcHTTP keeps ownership of it AND of
// its existing registry keys — re-keying those would strand every entry.
func shortInClusterURL(code string) (string, bool) {
	for _, m := range reAnyHTTPHost.FindAllStringSubmatch(code, -1) {
		host := m[1]
		if strings.Contains(strings.ToLower(host), ".svc") {
			continue
		}
		if inClusterHTTPHost(host) {
			return "http://" + strings.ToLower(host), true
		}
	}
	return "", false
}

func ciPlaintextGuardCmd() *cobra.Command {
	var root string
	cmd := &cobra.Command{
		Use:   "plaintext-guard",
		Short: "fail when an unencrypted in-cluster hop is not registered as an accepted residual",
		Long: "Static gate on cleartext in-cluster communication (docs/adr/0010-in-cluster-mtls.md).\n" +
			"Scans platform-apl/ and kubernetes-charts/ for `scheme: http` scrapes,\n" +
			"`insecureSkipVerify: true`, http:// URLs to in-cluster Services (fully\n" +
			"qualified OR the short svc.namespace / svc forms), and Istio mesh policy that\n" +
			"accepts cleartext (PeerAuthentication mode: PERMISSIVE, DestinationRule\n" +
			"tls.mode: DISABLE), plus tools/ for InsecureSkipVerify. Every hit must be\n" +
			"registered in plaintextAllowed with a reason and an owner; unregistered hits\n" +
			"fail, and so do registry entries whose hop no longer exists.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIPlaintextGuard(root) },
	}
	cmd.Flags().StringVar(&root, "root", ".", "repo root (template or instance layout)")
	return cmd
}

func runCIPlaintextGuard(root string) error {
	dirs := plaintextScanDirs(root)
	findings, examined, err := collectPlaintextFindings(root, dirs)
	if err != nil {
		return err
	}
	// Same rationale as the sibling guards: a guard that walked nothing prints
	// the same green as one that walked everything.
	if err := guardkit.RequireCorpus("plaintext-guard", examined, dirs); err != nil {
		return err
	}

	seen := map[string]bool{}
	failed := false
	for _, f := range findings {
		rule, ok := plaintextAllowed[f.key]
		seen[f.key] = true
		if ok {
			fmt.Printf("  ok: %s:%d %s — [%s] %s\n", f.file, f.line, f.what, rule.owner, rule.reason)
			continue
		}
		failed = true
		fmt.Printf("::error file=%s,line=%d::unregistered plaintext hop (%s). Every unencrypted in-cluster hop must be an explicit, reviewed decision: either secure it, or register %q in plaintextAllowed (ci_plaintext_guard.go) with a reason naming WHAT crosses the wire and an owner who could close it. See docs/adr/0010-in-cluster-mtls.md.\n",
			f.file, f.line, f.what, f.key)
	}

	// Stale entries: the hop is gone, so the line is now misinformation.
	var stale []string
	for k := range plaintextAllowed {
		if !seen[k] {
			stale = append(stale, k)
		}
	}
	sort.Strings(stale)
	for _, k := range stale {
		failed = true
		fmt.Printf("::error::plaintextAllowed entry %q matches nothing in the tree. The hop was secured or moved — delete the entry. A registry that keeps dead entries stops being reviewable, because a reader cannot tell which lines are load-bearing.\n", k)
	}

	if failed {
		return fmt.Errorf("plaintext-guard: unregistered plaintext hop(s) and/or stale registry entries")
	}
	fmt.Printf("plaintext-guard: %d plaintext hop(s), all registered with a reason and an owner.\n", len(findings))
	return nil
}

// plaintextScanDirs resolves the scan roots through esRepoPath, NOT raw joins.
// The sibling guards all take --root as either a template checkout or an
// instance, where the same trees sit one level down under instance-template/.
// A raw filepath.Join silently resolved to a non-existent path in the second
// layout, and a missing tree is tolerated by the walk — so the guard would have
// scanned less than it appeared to, which is the precise failure requireCorpus
// exists to catch and would NOT have caught here (platform-apl alone keeps the
// examined count above zero).
func plaintextScanDirs(root string) []string {
	dirs := platformTreeDirs(root)
	dirs = append(dirs, guardkit.RepoPath(root, "kubernetes-charts"), guardkit.RepoPath(root, "tools"))
	return dirs
}

func collectPlaintextFindings(root string, dirs []string) ([]plaintextFinding, int, error) {
	var out []plaintextFinding
	examined := 0
	for _, dir := range dirs {
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil // a missing tree is requireCorpus's problem, not a walk error
			}
			if info.IsDir() {
				// Vendored/generated trees say nothing about what we ship.
				if b := info.Name(); b == "vendor" || b == "rendered" || b == "coverage" || b == ".git" {
					return filepath.SkipDir
				}
				return nil
			}
			// The guard's own source contains the literals it searches for (its
			// finding messages and the registry's example keys), so it would
			// report itself. Linters exempt themselves for the same reason.
			if filepath.Base(path) == "ci_plaintext_guard.go" || filepath.Base(path) == "ci_plaintext_guard_test.go" {
				return nil
			}
			ext := filepath.Ext(path)
			isYAML := ext == ".yaml" || ext == ".yml"
			isGo := ext == ".go" && !strings.HasSuffix(path, "_test.go")
			if !isYAML && !isGo {
				return nil
			}
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			examined++
			rel := relForKey(root, path)
			out = append(out, scanPlaintext(rel, string(b), isGo)...)
			return nil
		})
		if err != nil {
			return nil, 0, err
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].file != out[j].file {
			return out[i].file < out[j].file
		}
		return out[i].line < out[j].line
	})
	return out, examined, nil
}

// scanPlaintext is the pure scanner — file content in, findings out — so the
// match rules are unit-tested without a tree on disk.
func scanPlaintext(rel, content string, isGo bool) []plaintextFinding {
	var out []plaintextFinding
	if !isGo {
		out = append(out, scanDefaultedScrapeScheme(rel, content)...)
	}
	lines := strings.Split(content, "\n")
	lastPort := ""
	lastMTLSPort := ""
	lastDocName := ""
	for i, ln := range lines {
		n := i + 1
		// Strip comments so a line DESCRIBING a plaintext hop is not itself a
		// finding — every registered residual carries an explanatory comment, and
		// those must not become self-fulfilling matches.
		code := stripComment(ln, isGo)
		if strings.TrimSpace(code) == "" {
			continue
		}
		if isGo {
			if u := reCleartextProto.FindString(code); u != "" {
				out = append(out, plaintextFinding{
					key: rel + ":" + strings.ToLower(strings.TrimSuffix(u, "://")), file: rel, line: n,
					what: "cleartext wire protocol (" + strings.ToLower(u) + ")",
				})
			}
			if reSSLModeDisable.MatchString(code) {
				out = append(out, plaintextFinding{
					key: rel + ":sslmode-disable", file: rel, line: n,
					what: "TLS disabled on a Postgres connection (sslmode=disable)",
				})
			}
			if reInsecureGoAssign.MatchString(code) || reInsecureGoRest.MatchString(code) {
				out = append(out, plaintextFinding{
					key: rel + ":" + goSymbolFor(lines, i), file: rel, line: n,
					what: "TLS verification disabled (assignment or client-go rest.Config Insecure)",
				})
			}
			if reInsecureGo.MatchString(code) {
				out = append(out, plaintextFinding{
					key: rel + ":" + goSymbolFor(lines, i), file: rel, line: n,
					what: "InsecureSkipVerify: true",
				})
			}
			// An in-cluster http:// URL is a plaintext hop wherever it is written,
			// and the most consequential ones in this tree are Go constants, not
			// YAML: the Harbor REST base (which carries the admin password in a
			// Basic-auth header) and Keycloak's JWKS URL (which carries the signing
			// keys OpenBao validates team logins with). An earlier revision of this
			// scanner `continue`d here and saw neither — the guard was blind to
			// exactly the two hops that motivated it.
			if u := reSvcHTTP.FindString(code); u != "" {
				out = append(out, plaintextFinding{
					key: rel + ":" + u, file: rel, line: n,
					what: "plaintext URL to an in-cluster Service (" + u + ")",
				})
			} else if su, ok := shortInClusterURL(code); ok {
				out = append(out, plaintextFinding{
					key: rel + ":" + su, file: rel, line: n,
					what: "plaintext URL to an in-cluster Service, short form (" + su + ")",
				})
			}
			continue
		}
		// A document separator RESETS the port context. Without this the port from
		// a previous document leaks into the next one, so a finding is keyed on a
		// port it has nothing to do with — and two hops in one file can collide on
		// the same key, silently masking one behind the other's registry entry.
		if strings.HasPrefix(strings.TrimSpace(code), "---") {
			lastPort = ""
			lastMTLSPort = ""
			lastDocName = ""
			continue
		}
		// Tracked separately from lastPort: a PeerAuthentication carries no `port:`
		// line at all, so sharing one variable would key an exemption on a port from
		// an unrelated document earlier in the file.
		if m := rePortMapKey.FindStringSubmatch(code); m != nil {
			lastMTLSPort = m[1]
		}
		// First `name:` in the document only — metadata.name in every manifest this
		// scans. Later ones (a selector value, a container) must not overwrite it.
		if lastDocName == "" {
			if m := reDocName.FindStringSubmatch(code); m != nil {
				lastDocName = m[1]
			}
		}
		if m := rePortName.FindStringSubmatch(code); m != nil {
			lastPort = strings.Trim(m[1], `"'`)
		}
		switch {
		case reSchemeHTTP.MatchString(code):
			out = append(out, plaintextFinding{
				key: rel + ":" + locator(lastPort, "scheme-http"), file: rel, line: n,
				what: "scrape over plaintext (scheme: http)",
			})
		case reInsecureYAML.MatchString(code):
			out = append(out, plaintextFinding{
				key: rel + ":" + locator(lastPort, "insecure-skip-verify"), file: rel, line: n,
				what: "TLS without server verification (insecureSkipVerify: true)",
			})
		case reSvcHTTP.MatchString(code):
			out = append(out, plaintextFinding{
				key: rel + ":" + reSvcHTTP.FindString(code), file: rel, line: n,
				what: "plaintext URL to an in-cluster Service (" + reSvcHTTP.FindString(code) + ")",
			})
		case reEventSourceSecrets.MatchString(code):
			out = append(out, plaintextFinding{
				key: rel + ":" + locator("", "eventsource-secrets"), file: rel, line: n,
				what: "Secret contents published onto an event bus (resource EventSource watching `secrets` — the whole object, `data` included, crosses the bus)",
			})
		case rePermissiveMTLS.MatchString(code):
			m := rePermissiveMTLS.FindStringSubmatch(code)
			out = append(out, plaintextFinding{
				key: rel + ":" + locator(mtlsLocator(lastMTLSPort), mtlsModeLocator(lastDocName)), file: rel, line: n,
				what: "mesh mTLS not enforced (mode: " + strings.ToUpper(m[1]) + ") — cleartext is accepted on this hop",
			})
		case reCleartextProto.MatchString(code):
			u := reCleartextProto.FindString(code)
			out = append(out, plaintextFinding{
				key: rel + ":" + strings.ToLower(strings.TrimSuffix(u, "://")), file: rel, line: n,
				what: "cleartext wire protocol (" + strings.ToLower(u) + ")",
			})
		case reSSLModeDisable.MatchString(code):
			out = append(out, plaintextFinding{
				key: rel + ":sslmode-disable", file: rel, line: n,
				what: "TLS disabled on a Postgres connection (sslmode=disable)",
			})
		case reInsecureCLI.MatchString(code):
			out = append(out, plaintextFinding{
				key: rel + ":" + locator(lastPort, "insecure-cli-flag"), file: rel, line: n,
				what: "TLS verification disabled on a command line",
			})
		case reInsecureYAMLTrue.MatchString(code) || reInsecureYAMLFalse.MatchString(code):
			out = append(out, plaintextFinding{
				key: rel + ":" + locator(lastPort, "insecure-tls-key"), file: rel, line: n,
				what: "TLS verification disabled by configuration key",
			})
		default:
			if u, ok := shortInClusterURL(code); ok {
				out = append(out, plaintextFinding{
					key: rel + ":" + u, file: rel, line: n,
					what: "plaintext URL to an in-cluster Service, short form (" + u + ")",
				})
			}
		}
	}
	return out
}

// locator picks the within-file part of a registry key: the scrape port when
// one is in scope, otherwise a stable name for the finding kind. It must never
// be empty — an empty locator makes two unrelated findings in one file share a
// key, so registering one would silently vouch for the other.
func locator(port, fallback string) string {
	if port != "" {
		return port
	}
	return fallback
}

// mtlsLocator prefixes the port so an mTLS exemption on port 8000 cannot collide
// with a scrape finding keyed on the same number.
func mtlsLocator(port string) string {
	if port == "" {
		return ""
	}
	return "mtls-" + port
}

// mtlsModeLocator names a DOCUMENT-level mesh mode (no port in scope) by the
// object it belongs to, so several policies in one file get several keys. Falls
// back to the bare kind when no name was seen — a file with one policy keeps the
// key it already had, and a nameless one still cannot collide with a port key.
func mtlsModeLocator(docName string) string {
	if docName == "" {
		return "mtls-mode"
	}
	return "mtls-mode-" + docName
}

// stripComment removes the trailing/leading comment so prose about a hop is not
// mistaken for the hop. Deliberately naive about quoted '#' / '//' inside
// strings: a false NEGATIVE there would need a URL inside a quoted string
// following a comment marker on the same line, which does not occur in this
// tree, and the alternative (matching comments) makes every explanatory note a
// finding.
func stripComment(ln string, isGo bool) string {
	if isGo {
		// Mask "://" before hunting for "//", or the scheme separator in
		// "http://harbor-core.harbor.svc" reads as a comment start and the rest of
		// the line — the URL itself — is discarded. That is exactly how an earlier
		// revision stayed blind to the Harbor and Keycloak hops: the finding was
		// stripped away before any pattern could match it.
		masked := strings.ReplaceAll(ln, "://", ":\x00\x00")
		if i := strings.Index(masked, "//"); i >= 0 {
			return ln[:i]
		}
		return ln
	}
	if i := strings.Index(ln, "#"); i >= 0 {
		return ln[:i]
	}
	return ln
}

// goSymbolFor names the enclosing func/var for a Go finding, so the registry key
// is stable against line moves. Walks backwards to the nearest declaration.
func goSymbolFor(lines []string, idx int) string {
	for i := idx; i >= 0; i-- {
		s := strings.TrimSpace(lines[i])
		for _, p := range []string{"func ", "var ", "const "} {
			if strings.HasPrefix(s, p) {
				rest := strings.TrimPrefix(s, p)
				rest = strings.TrimPrefix(rest, "(")
				cut := strings.IndexAny(rest, " (){}=*[]")
				if cut > 0 {
					return rest[:cut]
				}
				if rest != "" {
					return rest
				}
			}
		}
	}
	return "?"
}

// relForKey renders a repo-relative, slash-separated path for registry keys, so
// a key is identical whether the guard runs from the repo root or from tools/.
func relForKey(root, path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return filepath.ToSlash(path)
	}
	rel, err := filepath.Rel(rootAbs, abs)
	if err != nil {
		return filepath.ToSlash(path)
	}
	// Canonicalise across the two layouts esRepoPath resolves. In an instance
	// checkout the same trees sit under instance-template/, so without this strip
	// every registry key would carry that prefix in one layout and not the other —
	// and EVERY entry would miss, reporting the whole tree as unregistered while
	// simultaneously reporting every entry as stale. Keys name the hop, not the
	// checkout it was read from.
	return strings.TrimPrefix(filepath.ToSlash(rel), "instance-template/")
}

// ── the scheme nobody wrote ──────────────────────────────────────────────────
//
// Every pattern above reads what somebody WROTE. This one reads what they left
// out, and it is the only shape in this file that can.
//
// A prometheus-operator scrape endpoint with no `scheme:` key scrapes over
// **HTTP** — the CRD's documented default ("if empty, Prometheus uses the default
// value `http`"). So `scheme: http` and no scheme at all are the same hop on the
// wire, and reSchemeHTTP sees only the first. A new ServiceMonitor that simply
// omits the line is plaintext that passes this gate, and omitting it is the more
// likely spelling: `scheme: https` is something you add on purpose, and nobody
// adds `scheme: http` deliberately when leaving it out does the same thing.
//
// LATENT. All four monitors in this tree declare a scheme today, and three of
// them declare `https` because #360 moved them. That is what makes this worth
// closing now rather than after: the gate's whole value is that the NEXT one
// cannot be added silently, and a corpus with no live finding is where a drift
// gate is cheapest to add and hardest to argue with.
//
// Deliberately NOT expressed as "flag any endpoint without scheme: https". An
// endpoint can legitimately carry no scheme where relabeling rewrites
// `__scheme__`, and the registry is where that would be recorded — but the
// finding has to exist before it can be recorded, which is the point.
var (
	reMonitorKind    = regexp.MustCompile(`(?m)^\s*kind:\s*["']?(ServiceMonitor|PodMonitor)["']?\s*$`)
	reEndpointsKey   = regexp.MustCompile(`^(\s*)(endpoints|podMetricsEndpoints):\s*$`)
	reListItem       = regexp.MustCompile(`^(\s*)-\s`)
	reSchemeAnyKey   = regexp.MustCompile(`(?i)^\s*-?\s*scheme:`)
	reEndpointPortKV = regexp.MustCompile(`^\s*-?\s*(?:port|targetPort):\s*["']?([A-Za-z0-9_.\-]+)`)
)

// scanDefaultedScrapeScheme reports every ServiceMonitor/PodMonitor endpoint that
// declares no scheme. Document-scoped rather than line-scoped, because "this item
// contains no such key" is a statement about a block and the main loop's
// last-seen-wins model cannot make it.
func scanDefaultedScrapeScheme(rel, content string) []plaintextFinding {
	var out []plaintextFinding
	// Line offset of each document, so findings report a line in the FILE and not
	// in the fragment — a finding a reviewer cannot navigate to is half a finding.
	offset := 0
	for _, doc := range splitYAMLDocs(content) {
		if reMonitorKind.MatchString(doc) {
			out = append(out, defaultedSchemeInDoc(rel, doc, offset)...)
		}
		offset += strings.Count(doc, "\n")
	}
	return out
}

// splitYAMLDocs splits on `---` at the start of a line, keeping the separator
// with the following document so the line accounting stays exact.
func splitYAMLDocs(content string) []string {
	var docs []string
	var cur []string
	for _, ln := range strings.Split(content, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "---") && len(cur) > 0 {
			docs = append(docs, strings.Join(cur, "\n")+"\n")
			cur = nil
		}
		cur = append(cur, ln)
	}
	return append(docs, strings.Join(cur, "\n"))
}

func defaultedSchemeInDoc(rel, doc string, offset int) []plaintextFinding {
	var out []plaintextFinding
	lines := strings.Split(doc, "\n")
	keyIndent := -1
	itemIndent := -1
	itemStart := -1
	itemPort := ""
	hasScheme := false

	// close() emits a finding for the item that just ended, if it declared none.
	closeItem := func() {
		if itemStart < 0 {
			return
		}
		if !hasScheme {
			out = append(out, plaintextFinding{
				key:  rel + ":" + locator(itemPort, "scheme-defaulted"),
				file: rel, line: offset + itemStart + 1,
				what: "scrape over plaintext (no `scheme:` — prometheus-operator defaults to http)",
			})
		}
		itemStart, itemPort, hasScheme = -1, "", false
	}

	for i, raw := range lines {
		code := stripComment(raw, false)
		if strings.TrimSpace(code) == "" {
			continue
		}
		if keyIndent < 0 {
			if m := reEndpointsKey.FindStringSubmatch(code); m != nil {
				keyIndent = len(m[1])
			}
			continue
		}
		ind := len(code) - len(strings.TrimLeft(code, " \t"))
		// Dedent to or past the `endpoints:` key ends the list.
		if ind <= keyIndent {
			closeItem()
			keyIndent, itemIndent = -1, -1
			continue
		}
		if m := reListItem.FindStringSubmatch(code); m != nil && (itemIndent < 0 || len(m[1]) == itemIndent) {
			closeItem()
			itemIndent = len(m[1])
			itemStart = i
		}
		if itemStart < 0 {
			continue
		}
		if reSchemeAnyKey.MatchString(code) {
			hasScheme = true
		}
		if itemPort == "" {
			if m := reEndpointPortKV.FindStringSubmatch(code); m != nil {
				itemPort = m[1]
			}
		}
	}
	closeItem()
	return out
}
