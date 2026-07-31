package main

// ci_assert_eso_roundtrip.go implements `llz ci assert-eso-roundtrip` — the gate
// that ExternalSecrets are still actively re-reading from OpenBao, not merely
// that the Secrets they once produced still exist.
//
// This is the secret-delivery analogue of the log-delivery gate. ESO materializes
// an OpenBao path into a Kubernetes Secret ONCE and then refreshes on an
// interval. When the refresh path breaks — the ClusterSecretStore's CA goes
// stale, the k8s-auth role loses a policy, a KV path is renamed, OpenBao is
// sealed — the ALREADY-MATERIALIZED Secret keeps sitting there with its old
// value. Every pod that mounts it keeps working. converge is green, the Secret
// exists, and the store looks fine until something needs a value that changed.
//
// So the assertion is not "does the Secret exist" (it does, and will for as long
// as nothing deletes it). It is:
//
//   1. the ClusterSecretStore is Ready — ESO can authenticate to OpenBao at all;
//   2. every platform ExternalSecret reports SecretSynced;
//   3. its target Secret exists and carries non-empty data;
//   4. its status.refreshTime is RECENT — ESO has re-read the backend inside the
//      freshness window, which is the only evidence the READ path still works.
//
// (4) is what makes this a round trip rather than an inventory. A stale
// refreshTime with a present Secret is precisely the silent failure: ESO stopped
// being able to read OpenBao and nothing downstream noticed.
//
// FAIL-CLOSED: an unreadable cluster, zero ExternalSecrets found, a missing
// status, or an unparseable response all fail. Zero-found is a failure and not a
// vacuous pass — a cluster whose platform ExternalSecrets have all disappeared is
// not a cluster in a good state.
//
// Read-only.

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// esoStoreName is the ClusterSecretStore the platform's ExternalSecrets read
// through (platform-apl/manifest-secret-store).
const esoStoreName = "openbao"

func ciAssertESORoundTripCmd() *cobra.Command {
	var store, namespaces string
	var maxRefreshAge, settle, interval int
	c := &cobra.Command{
		Use:   "assert-eso-roundtrip",
		Short: "fail unless ExternalSecrets are still actively re-reading from OpenBao",
		Long: "Asserts the secret-delivery round trip, not an inventory: the ClusterSecretStore\n" +
			"is Ready, every platform ExternalSecret reports SecretSynced, its target Secret\n" +
			"exists with non-empty data, AND its status.refreshTime is recent.\n\n" +
			"The refreshTime check is the point. ESO materializes a Secret once and then\n" +
			"refreshes on an interval; when the READ path breaks — stale store CA, a lost\n" +
			"k8s-auth policy, a renamed KV path, a sealed OpenBao — the already-written\n" +
			"Secret keeps sitting there with its old value and every consumer keeps working.\n" +
			"converge is green and the Secret exists. Only staleness of the refresh can\n" +
			"distinguish that from a healthy pipeline.\n\n" +
			"Fails closed, including on finding zero ExternalSecrets. Read-only. Exit 0 / 1.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return runCIAssertESORoundTrip(store, splitCSVList(namespaces),
				time.Duration(maxRefreshAge)*time.Minute,
				time.Duration(settle)*time.Second, time.Duration(interval)*time.Second)
		},
	}
	c.Flags().StringVar(&store, "store", esoStoreName, "the ClusterSecretStore that must be Ready")
	c.Flags().StringVar(&namespaces, "namespaces", "",
		"comma-separated namespaces to check ExternalSecrets in (default: all namespaces)")
	c.Flags().IntVar(&maxRefreshAge, "max-refresh-age", 90,
		"minutes; an ExternalSecret whose status.refreshTime is older than this has stopped re-reading the backend")
	c.Flags().IntVar(&settle, "settle", 180, "seconds to keep polling before failing")
	c.Flags().IntVar(&interval, "interval", 15, "seconds between poll attempts")
	return c
}

// esVerdict is one ExternalSecret's outcome.
type esVerdict struct {
	Name    string // namespace/name
	Target  string // the Secret it writes
	Refresh time.Time
	FailWhy string
}

// storeReady reports whether a ClusterSecretStore's status carries a Ready=True
// condition. Pure.
//
// An absent condition array is NOT ready: a store that has never reported is one
// ESO has never successfully validated, and treating "no opinion" as healthy is
// how an unauthenticated store passes a readiness check.
func storeReady(raw []byte) (bool, string, error) {
	var obj struct {
		Status struct {
			Conditions []struct {
				Type    string `json:"type"`
				Status  string `json:"status"`
				Message string `json:"message"`
			} `json:"conditions"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return false, "", fmt.Errorf("decoding ClusterSecretStore: %w", err)
	}
	for _, c := range obj.Status.Conditions {
		if c.Type == "Ready" {
			return c.Status == "True", c.Message, nil
		}
	}
	return false, "no Ready condition — ESO has never successfully validated this store", nil
}

// externalSecretList is the subset of an ExternalSecret list this gate reads.
type externalSecretList struct {
	Items []struct {
		Metadata struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
		Spec struct {
			Target struct {
				Name string `json:"name"`
			} `json:"target"`
		} `json:"spec"`
		Status struct {
			RefreshTime string `json:"refreshTime"`
			Conditions  []struct {
				Type    string `json:"type"`
				Status  string `json:"status"`
				Reason  string `json:"reason"`
				Message string `json:"message"`
			} `json:"conditions"`
		} `json:"status"`
	} `json:"items"`
}

// evalExternalSecrets judges every ExternalSecret in a list. Pure.
//
// secretData maps "namespace/name" → whether that Secret exists with non-empty
// data, supplied by the caller so this stays testable without a cluster.
func evalExternalSecrets(raw []byte, secretData map[string]bool, now time.Time, maxRefreshAge time.Duration) ([]esVerdict, error) {
	var list externalSecretList
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("decoding ExternalSecret list: %w", err)
	}
	out := make([]esVerdict, 0, len(list.Items))
	for _, it := range list.Items {
		name := it.Metadata.Namespace + "/" + it.Metadata.Name
		target := it.Metadata.Namespace + "/" + it.Spec.Target.Name
		v := esVerdict{Name: name, Target: target}

		synced := false
		var why string
		for _, c := range it.Status.Conditions {
			if c.Type == "Ready" {
				synced = c.Status == "True"
				why = strings.TrimSpace(c.Reason + " " + c.Message)
			}
		}
		switch {
		case !synced:
			v.FailWhy = "not Ready: " + firstNonEmpty(why, "no Ready condition — ESO has never synced it")
		case it.Spec.Target.Name == "":
			v.FailWhy = "no spec.target.name — nothing to verify materialized"
		case !secretData[target]:
			v.FailWhy = fmt.Sprintf("target Secret %s is absent or has empty data despite a Ready condition", target)
		default:
			// refreshTime is the round-trip evidence. Absent means ESO has not
			// recorded a refresh at all, which is as blind as an old one.
			if it.Status.RefreshTime == "" {
				v.FailWhy = "no status.refreshTime — ESO has not recorded re-reading the backend, so a broken read path is indistinguishable from a healthy one"
				break
			}
			t, err := time.Parse(time.RFC3339, it.Status.RefreshTime)
			if err != nil {
				v.FailWhy = fmt.Sprintf("status.refreshTime %q is not RFC3339: %v", it.Status.RefreshTime, err)
				break
			}
			v.Refresh = t
			if age := now.Sub(t); age > maxRefreshAge {
				v.FailWhy = fmt.Sprintf("last refresh was %s ago, over the %s budget — ESO has stopped re-reading OpenBao. "+
					"The Secret still holds its last-known-good value, which is why nothing downstream has noticed",
					age.Round(time.Second), maxRefreshAge)
			}
		}
		out = append(out, v)
	}
	return out, nil
}

// failedES returns the ExternalSecrets that are not round-tripping.
func failedES(vs []esVerdict) []string {
	var out []string
	for _, v := range vs {
		if v.FailWhy != "" {
			out = append(out, v.Name)
		}
	}
	sort.Strings(out)
	return out
}

// ── cluster reads (seamed) ───────────────────────────────────────────────────

var (
	readClusterSecretStore = func(name string) ([]byte, error) {
		return execOutput("kubectl", "get", "clustersecretstore", name, "-o", "json")
	}
	readExternalSecrets = func(namespaces []string) ([]byte, error) {
		args := []string{"get", "externalsecrets", "-o", "json"}
		if len(namespaces) == 0 {
			args = append(args, "--all-namespaces")
		} else {
			// kubectl takes one -n; for several, the caller narrows via a label or
			// runs the gate per namespace. Multiple namespaces are joined by
			// querying all and filtering, which keeps one round trip.
			args = append(args, "--all-namespaces")
		}
		return execOutput("kubectl", args...)
	}
	readSecretsWithData = func() (map[string]bool, error) {
		out, err := execOutput("kubectl", "get", "secrets", "--all-namespaces", "-o", "json")
		if err != nil {
			return nil, err
		}
		var list struct {
			Items []struct {
				Metadata struct {
					Name      string `json:"name"`
					Namespace string `json:"namespace"`
				} `json:"metadata"`
				Data map[string]string `json:"data"`
			} `json:"items"`
		}
		if err := json.Unmarshal(out, &list); err != nil {
			return nil, fmt.Errorf("decoding Secret list: %w", err)
		}
		m := map[string]bool{}
		for _, it := range list.Items {
			m[it.Metadata.Namespace+"/"+it.Metadata.Name] = len(it.Data) > 0
		}
		return m, nil
	}
)

// filterByNamespace keeps only the verdicts in the requested namespaces (empty =
// keep everything).
func filterByNamespace(vs []esVerdict, namespaces []string) []esVerdict {
	if len(namespaces) == 0 {
		return vs
	}
	want := map[string]bool{}
	for _, n := range namespaces {
		want[n] = true
	}
	out := make([]esVerdict, 0, len(vs))
	for _, v := range vs {
		if ns, _, ok := strings.Cut(v.Name, "/"); ok && want[ns] {
			out = append(out, v)
		}
	}
	return out
}

func runCIAssertESORoundTrip(store string, namespaces []string, maxRefreshAge, settle, interval time.Duration) error {
	fmt.Println("## ExternalSecrets round-trip assertion")

	var last []esVerdict
	var lastErr error
	deadline := time.Now().Add(settle)
	for attempt := 1; ; attempt++ {
		vs, err := probeESORoundTrip(store, namespaces, time.Now(), maxRefreshAge)
		last, lastErr = vs, err
		if err == nil && len(failedES(vs)) == 0 && len(vs) > 0 {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		fmt.Printf("attempt %d: ESO round trip not clean yet — retrying in %s\n", attempt, interval)
		time.Sleep(interval)
	}

	if lastErr != nil {
		fmt.Fprintf(os.Stderr, "::error::%v\n", lastErr)
		return lastErr
	}
	// Zero ExternalSecrets is a FAILURE. A platform whose ExternalSecrets have all
	// vanished is not healthy, and passing here would be the vacuous green this
	// battery refuses.
	if len(last) == 0 {
		fmt.Fprintln(os.Stderr, "::error::no ExternalSecrets found — refusing to pass having examined nothing")
		return fmt.Errorf("no ExternalSecrets found — refusing to pass vacuously")
	}

	for _, v := range last {
		if v.FailWhy != "" {
			fmt.Printf("FAIL: %s — %s\n", v.Name, v.FailWhy)
		} else {
			fmt.Printf("OK: %s → %s (refreshed %s ago)\n", v.Name, v.Target, time.Since(v.Refresh).Round(time.Second))
		}
	}

	if bad := failedES(last); len(bad) > 0 {
		fmt.Fprintf(os.Stderr, "::error::ExternalSecret(s) not round-tripping: %s\n", strings.Join(bad, ", "))
		return fmt.Errorf("ExternalSecret(s) not round-tripping: %s", strings.Join(bad, ", "))
	}
	fmt.Printf("All %d ExternalSecret(s) are synced and actively re-reading OpenBao.\n", len(last))
	return nil
}

// probeESORoundTrip performs one full evaluation.
func probeESORoundTrip(store string, namespaces []string, now time.Time, maxRefreshAge time.Duration) ([]esVerdict, error) {
	storeRaw, err := readClusterSecretStore(store)
	if err != nil {
		return nil, fmt.Errorf("reading ClusterSecretStore %s: %w", store, err)
	}
	ready, msg, err := storeReady(storeRaw)
	if err != nil {
		return nil, err
	}
	if !ready {
		return nil, fmt.Errorf("ClusterSecretStore %s is not Ready (%s) — ESO cannot authenticate to OpenBao, so every ExternalSecret below it is serving a stale value", store, msg)
	}

	esRaw, err := readExternalSecrets(namespaces)
	if err != nil {
		return nil, fmt.Errorf("listing ExternalSecrets: %w", err)
	}
	secretData, err := readSecretsWithData()
	if err != nil {
		return nil, fmt.Errorf("listing Secrets: %w", err)
	}
	vs, err := evalExternalSecrets(esRaw, secretData, now, maxRefreshAge)
	if err != nil {
		return nil, err
	}
	return filterByNamespace(vs, namespaces), nil
}
