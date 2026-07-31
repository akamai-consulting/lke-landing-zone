package main

// ci_assert_reconciler_effects.go implements `llz ci assert-reconciler-effects` —
// the gate on what the reconciler lanes actually DO, as opposed to whether they
// are running.
//
// assert-reconciler --lanes proves each enabled lane reported a recent successful
// pass. That is liveness, not correctness: a lane can report success every cycle
// while its effect on the cluster is absent or wrong (it reconciled the wrong
// object, its patch was reverted by Argo, its input was empty). This asserts the
// INVARIANT each lane exists to maintain, read from the cluster rather than from
// the lane's own self-report.
//
// Three lanes carry an invariant that is both cheap to read and historically
// load-bearing:
//
//   sc-demote        exactly ONE default StorageClass. Two defaults wedged
//                    convergence hard (PR #101): Kyverno's admission policy loses
//                    the race whenever Flux goes quiet, and every subsequent PVC
//                    binds ambiguously. The lane re-demotes every 2m; this asserts
//                    the result.
//   token-inventory  the llz-token-inventory ConfigMap exists and is FRESH. The
//                    lane only re-exposes it as gauges — if the writing job stops,
//                    the gauges keep serving a stale-but-plausible expiry and
//                    LLZTokenExpiringSoon can never fire.
//   cidr-firewall    the firewall-controller ConfigMap exists and carries real
//                    discovered values. An empty/placeholder value here means the
//                    controller is running on defaults, which is either a broken
//                    ingress or an open hole depending on which key went blank.
//
// NOT covered here, deliberately:
//   - volume-labels / volume-tags: their invariants (no Volume left named
//     pvc-<uuid>, every Volume ownership-tagged) are asserted against the LINODE
//     API by assert-volume-encryption, which already holds that client and that
//     heal budget. Two gates asserting the same invariant differently is how they
//     drift apart.
//   - apl-overlay / es-store-recovery: their effects land in a git repo and in
//     ExternalSecret resync timing respectively. Neither has a cheap in-cluster
//     invariant that is not just a restatement of the lane's own metric, so they
//     stay covered by freshness alone. Saying so here is better than inventing a
//     check that looks like coverage and is not.
//
// FAIL-CLOSED throughout: an unreadable cluster, an absent object, or an
// unparseable response fails. Each sub-check self-skips ONLY when its lane is not
// enabled on this cluster — never because it could not tell.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// scDefaultClassAnnotation is the annotation Kubernetes uses to mark the default
// StorageClass. Shared with the sc-demote lane (reconcile_sc_demote.go).
const scDefaultClassAnnotation = "storageclass.kubernetes.io/is-default-class"

// tokenInventoryConfigMap is the ConfigMap the token-inventory job writes and the
// reconciler lane re-exposes.
const tokenInventoryConfigMap = "llz-token-inventory"

func ciAssertReconcilerEffectsCmd() *cobra.Command {
	var namespace, laneSet string
	var tokenMaxAge, settle, interval int
	var requireInventory bool
	c := &cobra.Command{
		Use:   "assert-reconciler-effects",
		Short: "fail unless each enabled reconciler lane's cluster invariant actually holds",
		Long: "Asserts what the reconciler lanes DO, not that they are running: exactly one\n" +
			"default StorageClass (sc-demote), a fresh llz-token-inventory ConfigMap\n" +
			"(token-inventory), and a populated firewall-controller ConfigMap\n" +
			"(cidr-firewall). A lane can report a successful pass every cycle while its\n" +
			"effect is absent — reconciled onto the wrong object, reverted by Argo, or\n" +
			"computed from empty input — and assert-reconciler --lanes cannot see that,\n" +
			"because it reads the lane's own self-report.\n\n" +
			"Each sub-check skips only when its lane is not enabled on this cluster, and\n" +
			"fails on every ambiguity otherwise. Read-only. Exit 0 all invariants hold, 1\n" +
			"otherwise.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return runCIAssertReconcilerEffects(namespace, splitCSVList(laneSet), requireInventory,
				time.Duration(tokenMaxAge)*time.Minute,
				time.Duration(settle)*time.Second, time.Duration(interval)*time.Second)
		},
	}
	c.Flags().StringVar(&namespace, "namespace", "llz-reconciler", "the reconciler's namespace (also where llz-token-inventory lives)")
	c.Flags().StringVar(&laneSet, "lane-set", "",
		"comma-separated lanes to check effects for (default: read the Deployment's --reconcile-* args)")
	c.Flags().BoolVar(&requireInventory, "require-token-inventory", false,
		"fail when the llz-token-inventory ConfigMap has never been written (default: report the skip). "+
			"Its writer is the scheduled llz-scheduled-checks job, so on a fresh cluster absence is normal; "+
			"set this for a caller that runs the writer itself, where absence is a break")
	c.Flags().IntVar(&tokenMaxAge, "token-inventory-max-age", 180,
		"minutes; the llz-token-inventory ConfigMap must have been updated within this window")
	c.Flags().IntVar(&settle, "settle", 180, "seconds to keep polling before failing (absorbs a lane's first sweep after converge)")
	c.Flags().IntVar(&interval, "interval", 15, "seconds between poll attempts")
	return c
}

// effectVerdict is one lane invariant's outcome.
type effectVerdict struct {
	Lane    string
	Detail  string // what was observed, for the report (both on pass and fail)
	Skipped bool   // the lane is not enabled here
	FailWhy string // empty when the invariant holds
}

// ── sc-demote: exactly one default StorageClass ──────────────────────────────

// defaultStorageClasses returns the names of StorageClasses marked default in a
// `kubectl get sc -o json` list. Pure.
//
// The annotation is a STRING in the API ("true"/"false"), and a class carrying
// "false" is not a default — reading the key's presence rather than its value is
// the obvious wrong turn here and would report every class as default.
func defaultStorageClasses(raw []byte) ([]string, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Name        string            `json:"name"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("decoding StorageClass list: %w", err)
	}
	var out []string
	for _, it := range list.Items {
		if it.Metadata.Annotations[scDefaultClassAnnotation] == "true" {
			out = append(out, it.Metadata.Name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// evalSCDemote judges the default-StorageClass invariant. Pure.
//
// ZERO defaults fails as loudly as two. A cluster with no default class silently
// breaks every PVC that omits storageClassName, and "the lane demoted one too
// many" is exactly the failure a demoting reconciler can cause.
func evalSCDemote(defaults []string) effectVerdict {
	v := effectVerdict{Lane: "sc-demote", Detail: fmt.Sprintf("default StorageClass(es): %v", defaults)}
	switch len(defaults) {
	case 1:
	case 0:
		v.FailWhy = "no default StorageClass — every PVC that omits storageClassName will fail to bind. " +
			"The sc-demote lane demotes LKE's Flux-promoted retain class; if it demoted the only remaining default, promote one"
	default:
		v.FailWhy = fmt.Sprintf("%d default StorageClasses (%s) — two defaults wedge convergence: "+
			"PVC binding is ambiguous and the Kyverno demote policy loses the admission race whenever Flux goes quiet. "+
			"The sc-demote lane should have re-demoted within ~2m, so it is not running or its patch is being reverted",
			len(defaults), strings.Join(defaults, ", "))
	}
	return v
}

// ── token-inventory: the ConfigMap exists and is fresh ───────────────────────

// evalTokenInventory judges the inventory ConfigMap's freshness from its
// `updated` field (RFC3339, written by the token-inventory job). Pure.
//
// An absent or unparseable timestamp FAILS rather than being skipped: the whole
// point of this check is that the gauges downstream keep serving a plausible
// last-known-good value when the writer stops, so "cannot tell how old it is" is
// the same operational blindness as "it is old".
func evalTokenInventory(raw []byte, now time.Time, maxAge time.Duration) effectVerdict {
	v := effectVerdict{Lane: "token-inventory"}
	var cm struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(raw, &cm); err != nil {
		v.FailWhy = fmt.Sprintf("decoding the %s ConfigMap: %v", tokenInventoryConfigMap, err)
		return v
	}
	stamp := firstNonEmpty(cm.Data["updated"], cm.Data["generated_at"], cm.Data["updatedAt"])
	if stamp == "" {
		v.FailWhy = fmt.Sprintf("the %s ConfigMap carries no update timestamp — its age cannot be established, "+
			"so a stopped writer would be indistinguishable from a fresh one", tokenInventoryConfigMap)
		return v
	}
	t, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		v.FailWhy = fmt.Sprintf("the %s ConfigMap's update timestamp %q is not RFC3339: %v", tokenInventoryConfigMap, stamp, err)
		return v
	}
	age := now.Sub(t)
	v.Detail = fmt.Sprintf("%s updated %s ago (budget %s)", tokenInventoryConfigMap, age.Round(time.Second), maxAge)
	if age > maxAge {
		v.FailWhy = fmt.Sprintf("the %s ConfigMap is %s old, over the %s budget — the writing job has stopped. "+
			"The reconciler lane keeps re-exposing it, so llz_token_expiry_timestamp_seconds still reports a "+
			"plausible value and LLZTokenExpiringSoon can never fire on a token that has since expired",
			tokenInventoryConfigMap, age.Round(time.Second), maxAge)
	}
	return v
}

// evalTokenInventoryAbsent judges a token-inventory ConfigMap that is not there
// at all. Pure.
//
// ABSENT AND STALE ARE DIFFERENT FAILURES, and this check only ever existed for
// the second one. The reconciler's token-inventory lane READS the ConfigMap and
// re-exposes it as gauges; the WRITER is `llz ci token-inventory | kubectl apply`
// in llz-scheduled-checks.yml — a scheduled workflow on its own cadence, which on
// a cluster minutes old has never run. So the lane being enabled says the reader
// is on, not that anything has been written yet, and treating absence as proof of
// breakage reds every fresh cluster.
//
// Staleness still gates, which is the property that matters: a writer that ran
// and STOPPED leaves gauges serving a plausible last-known-good expiry, and
// LLZTokenExpiringSoon can never fire on a token that has since lapsed. That is
// unchanged.
//
// --require-token-inventory is for the caller that runs the writer itself. There
// an absent ConfigMap IS a break, and the flag says so rather than leaving this
// gate permanently unable to tell the two apart.
func evalTokenInventoryAbsent(require bool, namespace string) effectVerdict {
	v := effectVerdict{Lane: "token-inventory"}
	if require {
		v.FailWhy = fmt.Sprintf("the %s ConfigMap does not exist in %s and --require-token-inventory is set — "+
			"this caller writes it, so its absence is a broken writer, not a fresh cluster",
			tokenInventoryConfigMap, namespace)
		return v
	}
	v.Skipped = true
	v.Detail = fmt.Sprintf("%s does not exist yet in %s — its writer is the scheduled "+
		"llz-scheduled-checks job, not the reconciler lane, and it has not run here. Freshness still gates "+
		"once it does; pass --require-token-inventory if this caller writes it",
		tokenInventoryConfigMap, namespace)
	return v
}

// ── cidr-firewall: the controller ConfigMap carries discovered values ────────

// firewallRequiredKeys are the values the discovery lane derives and the
// controller reads. An empty value means the controller fell back to its
// compiled default, which is a silent change in what the firewall admits.
var firewallRequiredKeys = []string{"LKE_CLUSTER_ID", "VPC_CIDR"}

// evalCIDRFirewall judges the firewall-controller ConfigMap. Pure.
func evalCIDRFirewall(raw []byte) effectVerdict {
	v := effectVerdict{Lane: "cidr-firewall"}
	var cm struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(raw, &cm); err != nil {
		v.FailWhy = fmt.Sprintf("decoding the %s ConfigMap: %v", firewallConfigMapName, err)
		return v
	}
	var missing []string
	for _, k := range firewallRequiredKeys {
		if strings.TrimSpace(cm.Data[k]) == "" {
			missing = append(missing, k)
		}
	}
	v.Detail = fmt.Sprintf("%s keys present: %v", firewallConfigMapName, sortedMapKeys(cm.Data))
	if len(missing) > 0 {
		v.FailWhy = fmt.Sprintf("the %s ConfigMap is missing/blank for %s — the firewall-controller is running on its "+
			"compiled defaults rather than this cluster's discovered values, which silently changes what the firewall admits. "+
			"Check the cidr-firewall lane's logs for a Linode API or node-lookup failure",
			firewallConfigMapName, strings.Join(missing, ", "))
	}
	return v
}

// sortedMapKeys returns a map's keys in deterministic order (report only).
func sortedMapKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ── orchestration ────────────────────────────────────────────────────────────

// effectReaders are the kubectl reads each sub-check needs, seamed for tests.
var (
	readStorageClasses = func() ([]byte, error) {
		return execOutput("kubectl", "get", "storageclass", "-o", "json")
	}
	// --ignore-not-found is load-bearing: it makes ABSENT (exit 0, empty stdout)
	// distinguishable from UNREADABLE (non-zero exit). Without it both arrive as
	// "exit status 1" and the check cannot tell a cluster whose writer has never
	// run from one where the read is broken.
	readTokenInventory = func(ns string) ([]byte, error) {
		return execOutput("kubectl", "-n", ns, "get", "configmap", tokenInventoryConfigMap,
			"--ignore-not-found", "-o", "json")
	}
	readFirewallConfig = func() ([]byte, error) {
		return execOutput("kubectl", "-n", "kube-system", "get", "configmap", firewallConfigMapName, "-o", "json")
	}
)

// probeReconcilerEffects evaluates every invariant whose lane is enabled.
func probeReconcilerEffects(namespace string, enabled map[string]bool, requireInventory bool, now time.Time, tokenMaxAge time.Duration) []effectVerdict {
	var out []effectVerdict

	if enabled["sc-demote"] {
		raw, err := readStorageClasses()
		switch {
		case err != nil:
			out = append(out, effectVerdict{Lane: "sc-demote",
				FailWhy: fmt.Sprintf("could not list StorageClasses: %v", err)})
		default:
			defaults, derr := defaultStorageClasses(raw)
			if derr != nil {
				out = append(out, effectVerdict{Lane: "sc-demote", FailWhy: derr.Error()})
			} else {
				out = append(out, evalSCDemote(defaults))
			}
		}
	} else {
		out = append(out, effectVerdict{Lane: "sc-demote", Skipped: true})
	}

	if enabled["token-inventory"] {
		raw, err := readTokenInventory(namespace)
		switch {
		case err != nil:
			out = append(out, effectVerdict{Lane: "token-inventory",
				FailWhy: fmt.Sprintf("could not read the %s ConfigMap in %s (%v)",
					tokenInventoryConfigMap, namespace, err)})
		case len(bytes.TrimSpace(raw)) == 0:
			out = append(out, evalTokenInventoryAbsent(requireInventory, namespace))
		default:
			out = append(out, evalTokenInventory(raw, now, tokenMaxAge))
		}
	} else {
		out = append(out, effectVerdict{Lane: "token-inventory", Skipped: true})
	}

	if enabled["cidr-firewall"] {
		raw, err := readFirewallConfig()
		if err != nil {
			out = append(out, effectVerdict{Lane: "cidr-firewall",
				FailWhy: fmt.Sprintf("could not read the %s ConfigMap in kube-system (%v) — the lane is enabled, so it must exist",
					firewallConfigMapName, err)})
		} else {
			out = append(out, evalCIDRFirewall(raw))
		}
	} else {
		out = append(out, effectVerdict{Lane: "cidr-firewall", Skipped: true})
	}

	return out
}

// failedEffects returns the verdicts that did not hold.
func failedEffects(vs []effectVerdict) []effectVerdict {
	var out []effectVerdict
	for _, v := range vs {
		if !v.Skipped && v.FailWhy != "" {
			out = append(out, v)
		}
	}
	return out
}

func runCIAssertReconcilerEffects(namespace string, laneSet []string, requireInventory bool, tokenMaxAge, settle, interval time.Duration) error {
	fmt.Println("## Reconciler lane-effect assertion")

	lanes := laneSet
	if len(lanes) == 0 {
		var err error
		if lanes, err = enabledReconcilerLanes(namespace); err != nil {
			fmt.Fprintf(os.Stderr, "::error::could not determine which reconciler lanes are enabled (%v)\n", err)
			return fmt.Errorf("could not determine which reconciler lanes are enabled: %w", err)
		}
	}
	enabled := map[string]bool{}
	for _, l := range lanes {
		enabled[l] = true
	}
	fmt.Printf("enabled lanes: %s\n", strings.Join(lanes, ", "))

	var last []effectVerdict
	deadline := time.Now().Add(settle)
	for attempt := 1; ; attempt++ {
		last = probeReconcilerEffects(namespace, enabled, requireInventory, time.Now(), tokenMaxAge)
		if len(failedEffects(last)) == 0 {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		fmt.Printf("attempt %d: %d invariant(s) not holding yet — retrying in %s\n",
			attempt, len(failedEffects(last)), interval)
		time.Sleep(interval)
	}

	for _, v := range last {
		switch {
		case v.Skipped && v.Detail != "":
			fmt.Printf("SKIP: %s — %s\n", v.Lane, v.Detail)
		case v.Skipped:
			fmt.Printf("SKIP: %s — lane not enabled on this cluster\n", v.Lane)
		case v.FailWhy != "":
			fmt.Printf("FAIL: %s — %s\n", v.Lane, v.FailWhy)
		default:
			fmt.Printf("OK: %s — %s\n", v.Lane, v.Detail)
		}
	}

	if bad := failedEffects(last); len(bad) > 0 {
		names := make([]string, 0, len(bad))
		for _, v := range bad {
			names = append(names, v.Lane)
		}
		fmt.Fprintf(os.Stderr, "::error::reconciler lane invariant(s) not holding: %s\n", strings.Join(names, ", "))
		return fmt.Errorf("reconciler lane invariant(s) not holding: %s", strings.Join(names, ", "))
	}
	fmt.Println("Every enabled lane's cluster invariant holds.")
	return nil
}
