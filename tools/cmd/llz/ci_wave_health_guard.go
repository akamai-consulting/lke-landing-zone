package main

// ci_wave_health_guard.go implements `llz ci wave-health-guard` — the static
// guard extracted from the 2026-07-04 four-wedge bootstrap outage (PR #142).
//
// Argo CD sync waves gate on per-resource HEALTH: a resource at wave N that
// sits Progressing forever (NetworkPolicy with no CNI-written status) or goes
// Degraded (ClusterIssuer whose deferred ACME email Let's Encrypt rejects)
// blocks or fails every wave > N of the platform-bootstrap sync — and OpenBao
// lives at wave 0, so the whole cluster bootstrap wedges. Two of the four
// 2026-07-04 wedges were exactly this class, and each cost a ~50-minute e2e
// run to discover on a real cluster.
//
// The guard makes that class a PR-time failure: every resource kind that
// appears at a NEGATIVE sync wave anywhere in the platform-bootstrap tree
// (apl-values/_shared/manifest/ + apl-values/components/) must be listed in
// waveHealthAllowedKinds — either because Argo assesses no health for it, or
// because _shared/values.yaml carries a resource.customizations.health
// override neutralizing its built-in check. Kinds whose safety DEPENDS on such
// an override are cross-checked against values.yaml, so deleting the override
// re-fails the guard.
//
// Adding a new kind at a negative wave forces the author to decide — and
// document here — why it cannot wedge a fresh-cluster bootstrap.

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// waveHealthKindRule describes why a kind is safe at a negative sync wave.
type waveHealthKindRule struct {
	// overrideKey non-empty → safety depends on the named
	// resource.customizations.health entry in _shared/values.yaml; the guard
	// fails if that key is missing there.
	overrideKey string
	reason      string
}

// waveHealthAllowedKinds maps "group/Kind" (core group = "") to the reason it
// may appear at a negative sync wave in the platform-bootstrap tree.
var waveHealthAllowedKinds = map[string]waveHealthKindRule{
	// Plain config/RBAC — Argo assesses no health; applied == done.
	"/Namespace":                                   {reason: "no Argo health check"},
	"/ServiceAccount":                              {reason: "no Argo health check"},
	"/ConfigMap":                                   {reason: "no Argo health check"},
	"/Secret":                                      {reason: "no Argo health check"},
	"rbac.authorization.k8s.io/Role":               {reason: "no Argo health check"},
	"rbac.authorization.k8s.io/RoleBinding":        {reason: "no Argo health check"},
	"rbac.authorization.k8s.io/ClusterRole":        {reason: "no Argo health check"},
	"rbac.authorization.k8s.io/ClusterRoleBinding": {reason: "no Argo health check"},
	// CronJob: Argo has no CronJob health check (it never runs at sync time).
	"batch/CronJob": {reason: "no Argo health check"},
	// Argo CD's own CRs: AppProject has no health; a child Application's
	// health is assessed only when the operator configures an explicit
	// argoproj.io_Application customization (apl-core does not), so the CR is
	// health-inert at sync time. cluster-foundation has synced at wave -20
	// through every bootstrap on record.
	"argoproj.io/AppProject":  {reason: "no Argo health check"},
	"argoproj.io/Application": {reason: "no app-of-apps health customization configured"},
	// Kyverno policies: Argo's kyverno health check reads the Ready condition,
	// which the admission controller sets promptly once the policy passes
	// webhook validation (the webhook-REJECTION class is caught earlier by the
	// lint dry-run job's Kyverno policy-admission gate).
	"kyverno.io/ClusterPolicy": {reason: "Kyverno sets Ready promptly post-admission"},
	// Health-checked kinds neutralized by _shared/values.yaml overrides — the
	// two wedges of PR #142. The override key is cross-checked below.
	"networking.k8s.io/NetworkPolicy": {
		overrideKey: "resource.customizations.health.networking.k8s.io_NetworkPolicy",
		reason:      "LKE CNI writes no NP status; built-in check waits Progressing forever",
	},
	"cert-manager.io/ClusterIssuer": {
		overrideKey: "resource.customizations.health.cert-manager.io_ClusterIssuer",
		reason:      "deferred ACME email is a supported state; built-in check grades it Degraded",
	},
	// ESO CRs: the lenient overrides reclassify not-Ready as Progressing so a
	// first-boot store/secret cannot FAIL a wave. They still wait — which is
	// correct: they converge once OpenBao is bootstrapped, and none sit at a
	// wave that gates OpenBao itself (wave 0). The override keys are pinned.
	"external-secrets.io/ClusterSecretStore": {
		overrideKey: "resource.customizations.health.external-secrets.io_ClusterSecretStore",
		reason:      "lenient override: not-Ready is Progressing during first boot",
	},
	"external-secrets.io/ExternalSecret": {
		overrideKey: "resource.customizations.health.external-secrets.io_ExternalSecret",
		reason:      "lenient override: not-Ready is Progressing during first boot",
	},
	"external-secrets.io/PushSecret": {
		overrideKey: "resource.customizations.health.external-secrets.io_PushSecret",
		reason:      "lenient override: not-Ready is Progressing during first boot",
	},
}

// waveHealthAllowedNames are per-RESOURCE exceptions ("group/Kind/name") for
// kinds that are NOT kind-level safe. Certificates are the case in point: Argo
// health-checks them (Ready-based), and an ACME Certificate at a negative wave
// would re-create wedge #3 — but these specific certs are issued by in-cluster
// self-signed CA chains with no external dependency, by a cert-manager whose
// webhook `llz ci wait-apl-pipeline` gates Available before the tree ever
// syncs. They have converged promptly through every green bootstrap on record.
// Keep this name-scoped: a NEW Certificate at a negative wave must be vetted
// here, not waved through by kind.
var waveHealthAllowedNames = map[string]waveHealthKindRule{
	"cert-manager.io/Certificate/openbao-ca":                  {reason: "in-cluster self-signed CA-chain cert; no external dependency"},
	"cert-manager.io/Certificate/otel-bootstrap-ca":           {reason: "in-cluster self-signed CA-chain cert; no external dependency"},
	"cert-manager.io/Certificate/platform-otel-collector-tls": {reason: "issued by the in-cluster otel-bootstrap-ca; no external dependency"},
}

// waveHealthDoc is the minimal YAML shape the guard inspects.
type waveHealthDoc struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name        string            `yaml:"name"`
		Annotations map[string]string `yaml:"annotations"`
	} `yaml:"metadata"`
}

// waveHealthFinding is one negative-wave resource and its verdict.
type waveHealthFinding struct {
	file, groupKind, name string
	wave                  int
	rule                  waveHealthKindRule
	allowed               bool
}

func ciWaveHealthGuardCmd() *cobra.Command {
	var root string
	cmd := &cobra.Command{
		Use:   "wave-health-guard",
		Short: "fail when a negative-sync-wave resource kind could health-wedge the platform-bootstrap sync",
		Long: "Static guard for the PR #142 wedge class: Argo sync waves gate on per-resource\n" +
			"health, so any kind at a negative wave in apl-values/_shared/manifest/ or\n" +
			"apl-values/components/ must be health-inert or neutralized by a\n" +
			"resource.customizations.health override in _shared/values.yaml. Unknown kinds at\n" +
			"negative waves fail with remediation guidance; kinds whose safety depends on a\n" +
			"values override fail if the override key is missing.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIWaveHealthGuard(root) },
	}
	cmd.Flags().StringVar(&root, "root", ".", "repo root (template or instance layout)")
	return cmd
}

func runCIWaveHealthGuard(root string) error {
	aplDir := esRepoPath(root, "apl-values")
	valuesPath := filepath.Join(aplDir, "_shared", "values.yaml")
	valuesRaw, err := os.ReadFile(valuesPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", valuesPath, err)
	}
	findings, err := collectWaveHealthFindings(
		[]string{filepath.Join(aplDir, "_shared", "manifest"), filepath.Join(aplDir, "components")},
		string(valuesRaw))
	if err != nil {
		return err
	}
	failed := false
	for _, f := range findings {
		if f.allowed {
			fmt.Printf("  ok: %s %s/%s wave %d — %s\n", f.file, f.groupKind, f.name, f.wave, f.rule.reason)
			continue
		}
		failed = true
		if f.rule.overrideKey != "" {
			fmt.Printf("::error file=%s::%s/%s at sync-wave %d needs the %q health override in _shared/values.yaml (apps.argocd._rawValues.configs.cm) — it is missing. Without it this kind can wedge the platform-bootstrap sync before OpenBao (wave 0); see PR #142.\n",
				f.file, f.groupKind, f.name, f.wave, f.rule.overrideKey)
			continue
		}
		fmt.Printf("::error file=%s::%s/%s sits at sync-wave %d but %q is not a known health-safe kind. Argo gates waves on per-resource health: if this kind can be not-Ready on a fresh cluster it will wedge the bootstrap before OpenBao (wave 0) — the PR #142 failure class. Either add a resource.customizations.health override in _shared/values.yaml and register the kind in waveHealthAllowedKinds (ci_wave_health_guard.go) with the override key, or register it with a documented reason it cannot wedge.\n",
			f.file, f.groupKind, f.name, f.wave, f.groupKind)
	}
	if failed {
		return fmt.Errorf("wave-health-guard: unvetted kinds at negative sync waves")
	}
	fmt.Println("wave-health-guard: every negative-wave kind is health-safe or override-backed.")
	return nil
}

// collectWaveHealthFindings walks the given dirs and classifies every
// negative-wave resource against waveHealthAllowedKinds + the values overrides.
func collectWaveHealthFindings(dirs []string, values string) ([]waveHealthFinding, error) {
	var findings []waveHealthFinding
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".yaml") {
				return err
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, doc := range splitWaveHealthDocs(string(raw)) {
				f, ok := classifyWaveHealthDoc(path, doc, values)
				if ok {
					findings = append(findings, f)
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].file != findings[j].file {
			return findings[i].file < findings[j].file
		}
		return findings[i].name < findings[j].name
	})
	return findings, nil
}

// splitWaveHealthDocs parses a multi-doc YAML file, skipping docs that fail to
// parse (kustomize patches etc. are not this guard's concern).
func splitWaveHealthDocs(raw string) []waveHealthDoc {
	var docs []waveHealthDoc
	dec := yaml.NewDecoder(strings.NewReader(raw))
	for {
		var d waveHealthDoc
		if err := dec.Decode(&d); err != nil {
			break
		}
		if d.Kind != "" {
			docs = append(docs, d)
		}
	}
	return docs
}

// classifyWaveHealthDoc returns a finding for docs at negative sync waves;
// ok=false for wave >= 0 (not gated) and non-resource docs.
func classifyWaveHealthDoc(path string, d waveHealthDoc, values string) (waveHealthFinding, bool) {
	waveStr, has := d.Metadata.Annotations["argocd.argoproj.io/sync-wave"]
	if !has {
		return waveHealthFinding{}, false // default wave 0 — does not gate wave 0
	}
	wave, err := strconv.Atoi(strings.TrimSpace(waveStr))
	if err != nil || wave >= 0 {
		return waveHealthFinding{}, false
	}
	group := ""
	if gv := strings.SplitN(d.APIVersion, "/", 2); len(gv) == 2 {
		group = gv[0]
	}
	groupKind := group + "/" + d.Kind
	f := waveHealthFinding{file: path, groupKind: groupKind, name: d.Metadata.Name, wave: wave}
	if rule, ok := waveHealthAllowedNames[groupKind+"/"+d.Metadata.Name]; ok {
		f.rule, f.allowed = rule, true
		return f, true
	}
	rule, known := waveHealthAllowedKinds[groupKind]
	if !known {
		return f, true // allowed=false: unvetted kind
	}
	f.rule = rule
	f.allowed = rule.overrideKey == "" || strings.Contains(values, rule.overrideKey+":")
	return f, true
}
