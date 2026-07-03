package main

// ci_discover_firewall.go implements `llz ci discover-firewall-config` — the
// IN-CLUSTER replacement for the `bootstrap-cloud-firewall` CI seed. Runs on
// the slim llz image via the cidrFirewall component's CronJob and derives every
// value the firewall-controller needs from the pod's own vantage point:
//
//	NODE_NAME (downward API) → Node.spec.providerID → the node's Linode ID →
//	  - LINODE_FIREWALL_ID: the firewall attached to that instance (Linode caps
//	    Cloud Firewalls at one per linode, so the attached one IS the node-pool
//	    firewall the llz-node-firewall module created)
//	  - LKE_CLUSTER_ID: the instance's lke_cluster_id (node-name parse fallback)
//	  - VPC_CIDR: the instance's VPC-interface subnet ipv4 range
//
// No tfvars, no TF outputs, no account-wide label scan, no control-plane ACL
// lease: controllers discover, they don't get told. The values are then
// reconciled into the controller's ConfigMap exactly as the CI seed did
// (create-if-missing + merge-patch, SSA-compatible with the Argo Application's
// ignoreDifferences), and the controller Deployment is rolled ONLY when a
// value actually changed (configMapKeyRef env is read once at pod creation).
// Steady state is a pure no-op, so the CronJob cadence is free to be tight.
//
// Env contract:
//   NODE_NAME     — the node this pod landed on (downward API spec.nodeName);
//                   any node works, they all share the pool firewall
//   LINODE_TOKEN  — Linode API token (the ESO-synced secret/linode/api-token,
//                   the same rotating credential the other in-cluster
//                   components read)
//   SA_TOKEN_FILE — optional ServiceAccount token path override (tests)

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/kube"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/linode"
	"github.com/spf13/cobra"
)

// kubeAPI is the slice of internal/kube the discover flow uses; seamed so tests
// drive the reconcile logic without an apiserver.
type kubeAPI interface {
	GetJSON(ctx context.Context, path string) (map[string]any, int, error)
	CreateJSON(ctx context.Context, path string, obj any) (int, error)
	MergePatch(ctx context.Context, path string, patch any) error
}

// discoverKubeFn opens the in-cluster kube client. Seamed for tests.
var discoverKubeFn = func() (kubeAPI, error) { return kube.NewInCluster() }

// firewallDiscoverer is the slice of the Linode client the resolver walks;
// seamed (same pattern as ci_cred_audit's credLister) so tests drive the walk
// without a live account.
type firewallDiscoverer interface {
	InstanceFirewalls(ctx context.Context, linodeID uint64) ([]map[string]any, error)
	InstanceLKEClusterID(ctx context.Context, linodeID uint64) (uint64, error)
	InstanceConfigs(ctx context.Context, linodeID uint64) ([]map[string]any, error)
	ListVPCSubnets(ctx context.Context, vpcID uint64) ([]map[string]any, error)
}

var newFirewallDiscoverer = func(token string) firewallDiscoverer {
	return linode.NewClient(token, 60*time.Second)
}

// resolveFirewallInputs walks the Linode API from the node's instance ID to the
// three controller inputs. clusterID / vpcCIDR may be "" (optional — matching
// the CI seed's semantics: ACL reconciliation disabled / chart default kept).
func resolveFirewallInputs(ctx context.Context, client firewallDiscoverer, linodeID uint64, nodeName string) (firewallID, clusterID, vpcCIDR string, err error) {
	fws, err := client.InstanceFirewalls(ctx, linodeID)
	if err != nil {
		return "", "", "", fmt.Errorf("list firewalls of instance %d: %w", linodeID, err)
	}
	if len(fws) == 0 {
		return "", "", "", fmt.Errorf("instance %d has no attached Cloud Firewall — was the llz-node-firewall module applied?", linodeID)
	}
	if len(fws) > 1 {
		labels := make([]string, 0, len(fws))
		for _, fw := range fws {
			labels = append(labels, linode.MapString(fw, "label"))
		}
		return "", "", "", fmt.Errorf("instance %d has %d attached Cloud Firewalls (%v) — expected exactly the node-pool firewall", linodeID, len(fws), labels)
	}
	firewallID = strconv.FormatUint(linode.MapUint(fws[0], "id"), 10)

	// lke_cluster_id from the instance object; node-name parse as the fallback
	// for API responses that omit the field. Empty is tolerated (optional).
	cid, err := client.InstanceLKEClusterID(ctx, linodeID)
	if err != nil {
		return "", "", "", err
	}
	if cid != 0 {
		clusterID = strconv.FormatUint(cid, 10)
	} else {
		clusterID = linode.LKEClusterIDFromNodeName(nodeName)
	}

	// VPC subnet CIDR from the instance's vpc interface. Both lookups are
	// best-effort: a cluster without a VPC keeps the chart default.
	cfgs, err := client.InstanceConfigs(ctx, linodeID)
	if err != nil {
		return "", "", "", fmt.Errorf("list configs of instance %d: %w", linodeID, err)
	}
	if vpcID, subnetID, ok := linode.VPCInterface(cfgs); ok {
		subnets, err := client.ListVPCSubnets(ctx, vpcID)
		if err != nil {
			return "", "", "", fmt.Errorf("list subnets of VPC %d: %w", vpcID, err)
		}
		vpcCIDR, _ = linode.SubnetIPv4(subnets, subnetID)
	}
	return firewallID, clusterID, vpcCIDR, nil
}

func ciDiscoverFirewallConfigCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "discover-firewall-config",
		Short: "self-discover the firewall-controller config from this pod's node and reconcile the ConfigMap",
		Long: "In-cluster replacement for the bootstrap-cloud-firewall CI seed. Resolves the\n" +
			"node-pool firewall ID, LKE cluster ID and VPC subnet CIDR from this pod's own\n" +
			"node via the Linode API (providerID → instance → attached firewall / VPC\n" +
			"interface), reconciles them into the " + firewallConfigMapName + " ConfigMap,\n" +
			"and rolls the controller Deployment only when a value changed.\n\n" +
			"Env: NODE_NAME (downward API), LINODE_TOKEN (ESO-synced rotating token).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runCIDiscoverFirewallConfig(context.Background())
		},
	}
}

const (
	firewallConfigMapPath  = "/api/v1/namespaces/kube-system/configmaps/" + firewallConfigMapName
	firewallConfigMapsPath = "/api/v1/namespaces/kube-system/configmaps"
	firewallDeploymentPath = "/apis/apps/v1/namespaces/kube-system/deployments/" + firewallDeploymentName
)

func runCIDiscoverFirewallConfig(ctx context.Context) error {
	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		return fmt.Errorf("NODE_NAME must be set (downward API spec.nodeName)")
	}
	token := os.Getenv("LINODE_TOKEN")
	if token == "" {
		return fmt.Errorf("LINODE_TOKEN must be set (the ESO-synced linode-api-token Secret)")
	}

	k, err := discoverKubeFn()
	if err != nil {
		return err
	}

	// ── Own node → Linode instance ID ────────────────────────────────────────
	node, status, err := k.GetJSON(ctx, "/api/v1/nodes/"+nodeName)
	if err != nil {
		return err
	}
	if status == 404 || node == nil {
		return fmt.Errorf("node %q not found — NODE_NAME must come from the downward API", nodeName)
	}
	spec, _ := node["spec"].(map[string]any)
	providerID, _ := spec["providerID"].(string)
	linodeID, ok := linode.LinodeIDFromProviderID(providerID)
	if !ok {
		return fmt.Errorf("node %q providerID %q is not linode://<id> — cannot resolve the instance", nodeName, providerID)
	}

	// ── Instance → firewall / cluster / VPC CIDR ─────────────────────────────
	firewallID, clusterID, vpcCIDR, err := resolveFirewallInputs(ctx, newFirewallDiscoverer(token), linodeID, nodeName)
	if err != nil {
		return err
	}
	if clusterID == "" {
		fmt.Println("LKE cluster ID not discoverable — LKE control-plane ACL reconciliation will be disabled")
	}
	if vpcCIDR == "" {
		fmt.Println("no VPC interface on the node — VPC_CIDR left at the chart default")
	}

	// ── Reconcile the ConfigMap ──────────────────────────────────────────────
	cm, status, err := k.GetJSON(ctx, firewallConfigMapPath)
	if err != nil {
		return err
	}
	if status == 404 {
		// Same create-if-missing the CI seed did: the controller chart renders
		// this ConfigMap with empty placeholders, but the component may sync
		// before the consumer installs the controller Application.
		if _, err := k.CreateJSON(ctx, firewallConfigMapsPath, map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]string{"name": firewallConfigMapName, "namespace": "kube-system"},
		}); err != nil {
			return fmt.Errorf("create %s: %w", firewallConfigMapName, err)
		}
		cm = map[string]any{}
	}
	existing := map[string]string{}
	if data, ok := cm["data"].(map[string]any); ok {
		for key, v := range data {
			if s, isStr := v.(string); isStr {
				existing[key] = s
			}
		}
	}

	changes := firewallConfigChanges(existing, firewallID, clusterID, vpcCIDR)
	if len(changes) == 0 {
		fmt.Printf("%s already up to date (LINODE_FIREWALL_ID=%s) — nothing to do\n", firewallConfigMapName, firewallID)
		return nil
	}
	if err := k.MergePatch(ctx, firewallConfigMapPath, map[string]any{"data": changes}); err != nil {
		return err
	}
	keys := make([]string, 0, len(changes))
	for key := range changes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Printf("Set %s=%s in %s\n", key, changes[key], firewallConfigMapName)
	}

	// ── Roll the controller so configMapKeyRef env re-reads the new values ───
	// A 404 is benign: the consumer has not installed the controller (or Argo
	// has not synced it yet) — it will start fresh from the patched ConfigMap.
	if _, status, err := k.GetJSON(ctx, firewallDeploymentPath); err != nil {
		return err
	} else if status == 404 {
		fmt.Printf("Deployment %s not present — skipping restart (it will start from the patched ConfigMap)\n", firewallDeploymentName)
		return nil
	}
	restartedAt := time.Now().UTC().Format(time.RFC3339)
	if err := k.MergePatch(ctx, firewallDeploymentPath, map[string]any{
		"spec": map[string]any{"template": map[string]any{"metadata": map[string]any{
			"annotations": map[string]string{"kubectl.kubernetes.io/restartedAt": restartedAt},
		}}},
	}); err != nil {
		return fmt.Errorf("restart %s after config change: %w", firewallDeploymentName, err)
	}
	fmt.Printf("Rolled deployment %s (config changed)\n", firewallDeploymentName)
	return nil
}

// firewallConfigChanges returns the ConfigMap data keys that must be patched:
// the discovered values that differ from what is already there. Empty
// discoveries (optional clusterID / vpcCIDR) never overwrite an existing value.
func firewallConfigChanges(existing map[string]string, firewallID, clusterID, vpcCIDR string) map[string]string {
	desired := map[string]string{"LINODE_FIREWALL_ID": firewallID}
	if clusterID != "" {
		desired["LKE_CLUSTER_ID"] = clusterID
	}
	if vpcCIDR != "" {
		desired["VPC_CIDR"] = vpcCIDR
	}
	changes := map[string]string{}
	for key, want := range desired {
		if existing[key] != want {
			changes[key] = want
		}
	}
	return changes
}
