package main

// ci_wave_health_audit.go implements `llz ci wave-health-audit` — the RUNTIME
// counterpart to the static `llz ci wave-health-guard` and the llz-wave-health-guard
// ValidatingAdmissionPolicy.
//
// The static guard scans the git tree at PR time; the VAP enforces at admission.
// Neither sees what a CONVERGED cluster actually runs at negative sync-waves that
// arrives from paths the platform-bootstrap kustomize tree does not contain —
// child-App-rendered content (Argo Events Sensors, Workflows), apl-core resources,
// or an operator writeback. This audit enumerates every LIVE resource at a negative
// sync-wave and applies the SAME decision the VAP makes (the wave-health allowlist +
// name exceptions, minus the VAP-excluded groups), flagging any kind the VAP would
// DENY. Each flag is either a coverage gap (vet the kind and add it to the allowlist)
// or a latent false-positive (the VAP would reject a legitimate resource — as the
// argoproj.io Events kinds would have, found by a manual census; this makes that
// census a standing check).

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// liveResource is a cluster resource carrying a (negative) sync-wave.
type liveResource struct {
	group, kind, name, namespace string
	wave                         int
	// hook is true when the resource carries an argocd.argoproj.io/hook annotation.
	// Argo hooks (PreSync/Sync/PostSync/SyncFail) are NOT part of the application's
	// tracked resource tree, so Argo never wave-gates on their health — a hook at a
	// negative sync-wave cannot cause the health-wedge this guard exists to prevent.
	hook bool
}

func (r liveResource) groupKind() string { return r.group + "/" + r.kind }

func (r liveResource) where() string {
	if r.namespace == "" {
		return r.name
	}
	return r.namespace + "/" + r.name
}

// waveHealthAuditExcludedGroups are the API groups the llz-wave-health-guard VAP
// excludes from matching (admission/wave-health-policy.yaml excludeResourceRules).
// Their CRs at negative waves are health-inert (Application/AppProject) or managed by
// CHILD Apps that health-gate their own content (Sensor/EventSource/EventBus/Workflow)
// — outside this guard's platform-bootstrap tree. Keep in lockstep with the VAP.
var waveHealthAuditExcludedGroups = map[string]bool{"argoproj.io": true}

// auditNegativeWave returns the live resources a negative-wave audit flags: those at
// a negative sync-wave that the wave-health guard does not vouch for AND the VAP does
// not exclude — i.e. the ones the VAP would deny. Pure (kubectl I/O is separate) so
// the decision is unit-tested against the same allowlist the guard + VAP use.
func auditNegativeWave(resources []liveResource) []liveResource {
	var flagged []liveResource
	for _, r := range resources {
		if r.wave >= 0 {
			continue // only negative waves gate a fresh-cluster bootstrap
		}
		if r.hook {
			continue // Argo hook — not a wave-gated tree resource, cannot health-wedge
		}
		if waveHealthAuditExcludedGroups[r.group] {
			continue // VAP excludes the group — not this guard's concern
		}
		gk := r.groupKind()
		if _, ok := waveHealthAllowedKinds[gk]; ok {
			continue // kind-level vetted (health-inert or override-backed)
		}
		if _, ok := waveHealthAllowedNames[gk+"/"+r.name]; ok {
			continue // name-scoped exception
		}
		flagged = append(flagged, r)
	}
	sort.Slice(flagged, func(i, j int) bool {
		if flagged[i].groupKind() != flagged[j].groupKind() {
			return flagged[i].groupKind() < flagged[j].groupKind()
		}
		return flagged[i].where() < flagged[j].where()
	})
	return flagged
}

// waveHealthAuditSkipTypes are high-churn resource types that never carry a sync-wave
// — skipped for speed (a `get -A -o json` of every Secret/Event is slow and pointless).
var waveHealthAuditSkipTypes = map[string]bool{
	"events":                          true,
	"events.events.k8s.io":            true,
	"endpoints":                       true,
	"endpointslices.discovery.k8s.io": true,
	"leases.coordination.k8s.io":      true,
	"componentstatuses":               true,
	"pods":                            true,
}

func ciWaveHealthAuditCmd() *cobra.Command {
	var failOnUnvetted bool
	cmd := &cobra.Command{
		Use:   "wave-health-audit",
		Short: "audit a live cluster for negative-sync-wave kinds the wave-health guard/VAP would flag",
		Long: "Runtime counterpart to `llz ci wave-health-guard` + the llz-wave-health-guard\n" +
			"ValidatingAdmissionPolicy. Enumerates every live resource at a negative sync-wave\n" +
			"and flags any kind the VAP would deny (unvetted by the allowlist, not an excluded\n" +
			"group) — a coverage gap to vet, or a latent VAP false-positive. Report-only by\n" +
			"default (the job summary is the deliverable); --fail-on-unvetted gates. Uses\n" +
			"kubectl with the ambient KUBECONFIG.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			kubectl := func(args ...string) (string, bool) {
				c := exec.Command("kubectl", args...)
				// stdout and stderr into SEPARATE buffers — kubectl writes warnings
				// (e.g. a client/server version-skew notice) to stderr, and merging
				// them would corrupt the JSON these calls parse. Return stdout only;
				// the exit code carries success.
				var stdout, stderr strings.Builder
				c.Stdout, c.Stderr = &stdout, &stderr
				c.Env = os.Environ()
				// Run BEFORE reading stdout — a `return stdout.String(), c.Run()==nil`
				// evaluates stdout.String() first (empty, pre-run) per Go's left-to-right
				// operand order.
				ok := c.Run() == nil
				return stdout.String(), ok
			}
			return runWaveHealthAudit(kubectl, failOnUnvetted)
		},
	}
	cmd.Flags().BoolVar(&failOnUnvetted, "fail-on-unvetted", false, "exit non-zero when an unvetted negative-wave kind is found (default report-only)")
	return cmd
}

func runWaveHealthAudit(kubectl func(...string) (string, bool), failOnUnvetted bool) error {
	resources, ok := collectNegativeWaveResources(kubectl)
	if !ok {
		// Cluster unreachable (the scheduled-checks job also gates on `available`,
		// but tolerate it here too): nothing to audit, not a failure.
		fmt.Println("wave-health-audit: cluster unreachable or kubectl unavailable — skipping.")
		return nil
	}
	flagged := auditNegativeWave(resources)
	fmt.Printf("wave-health-audit: scanned %d resource(s) at negative sync-waves.\n", len(resources))
	if len(flagged) == 0 {
		fmt.Println("wave-health-audit: every negative-wave kind is allowlisted or an excluded group — clean.")
		return nil
	}
	for _, f := range flagged {
		fmt.Printf("::warning::wave-health-audit: %s %s at sync-wave %d is not vetted by the wave-health allowlist and is not an excluded group — the llz-wave-health-guard VAP would DENY it. Either vet the kind and add it to waveHealthAllowedKinds (+ the VAP allowlist), or, if it is child-App-managed, add its group to the VAP's excludeResourceRules (and waveHealthAuditExcludedGroups).\n",
			f.groupKind(), f.where(), f.wave)
	}
	if failOnUnvetted {
		return fmt.Errorf("wave-health-audit: %d unvetted negative-wave resource(s)", len(flagged))
	}
	return nil
}

// collectNegativeWaveResources enumerates every listable resource type and returns
// those carrying a negative argocd sync-wave. ok=false only when the cluster is
// unreachable (api-resources itself fails); an unlistable individual type is skipped.
func collectNegativeWaveResources(kubectl func(...string) (string, bool)) ([]liveResource, bool) {
	out, ok := kubectl("api-resources", "--verbs=list", "-o", "name")
	if !ok {
		return nil, false
	}
	seen := map[string]bool{}
	var resources []liveResource
	for _, t := range strings.Fields(out) {
		if t == "" || seen[t] || waveHealthAuditSkipTypes[t] {
			continue
		}
		seen[t] = true
		j, ok := kubectl("get", t, "-A", "-o", "json")
		if !ok {
			continue // not listable at cluster scope / RBAC / subresource — skip
		}
		resources = append(resources, parseNegativeWaveItems(j)...)
	}
	return resources, true
}

// parseNegativeWaveItems extracts the negative-sync-wave resources from a
// `kubectl get <type> -A -o json` list payload.
func parseNegativeWaveItems(raw string) []liveResource {
	var list struct {
		Items []struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
			Metadata   struct {
				Name        string            `json:"name"`
				Namespace   string            `json:"namespace"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return nil
	}
	var out []liveResource
	for _, it := range list.Items {
		w, ok := it.Metadata.Annotations["argocd.argoproj.io/sync-wave"]
		if !ok {
			continue
		}
		wave, err := strconv.Atoi(strings.TrimSpace(w))
		if err != nil || wave >= 0 {
			continue
		}
		group := ""
		if gv := strings.SplitN(it.APIVersion, "/", 2); len(gv) == 2 {
			group = gv[0]
		}
		_, isHook := it.Metadata.Annotations["argocd.argoproj.io/hook"]
		out = append(out, liveResource{
			group: group, kind: it.Kind,
			name: it.Metadata.Name, namespace: it.Metadata.Namespace, wave: wave,
			hook: isHook,
		})
	}
	return out
}
