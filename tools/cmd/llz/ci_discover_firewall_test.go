package main

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/kube"
)

// fakeKube is an in-memory kubeAPI: path → object. Records patches and creates.
type fakeKube struct {
	objects map[string]map[string]any
	patches map[string][]any
	created []string
}

func newFakeKube() *fakeKube {
	return &fakeKube{objects: map[string]map[string]any{}, patches: map[string][]any{}}
}

func (f *fakeKube) GetJSON(_ context.Context, path string) (map[string]any, int, error) {
	if obj, ok := f.objects[path]; ok {
		return obj, 200, nil
	}
	return nil, 404, nil
}

func (f *fakeKube) CreateJSON(_ context.Context, path string, obj any) (int, error) {
	f.created = append(f.created, path)
	return 201, nil
}

func (f *fakeKube) MergePatch(_ context.Context, path string, patch any) error {
	f.patches[path] = append(f.patches[path], patch)
	return nil
}

// fakeDiscoverer is an in-memory firewallDiscoverer.
type fakeDiscoverer struct {
	firewalls    []map[string]any
	lkeClusterID uint64
	configs      []map[string]any
	subnets      []map[string]any
}

func (f *fakeDiscoverer) InstanceFirewalls(context.Context, uint64) ([]map[string]any, error) {
	return f.firewalls, nil
}
func (f *fakeDiscoverer) InstanceLKEClusterID(context.Context, uint64) (uint64, error) {
	return f.lkeClusterID, nil
}
func (f *fakeDiscoverer) InstanceConfigs(context.Context, uint64) ([]map[string]any, error) {
	return f.configs, nil
}
func (f *fakeDiscoverer) ListVPCSubnets(context.Context, uint64) ([]map[string]any, error) {
	return f.subnets, nil
}

// seamDiscover installs fake kube + discoverer seams and env, restoring on cleanup.
func seamDiscover(t *testing.T, k kubeAPI, d firewallDiscoverer) {
	t.Helper()
	origKube, origNew := discoverKubeFn, newFirewallDiscoverer
	t.Cleanup(func() { discoverKubeFn, newFirewallDiscoverer = origKube, origNew })
	discoverKubeFn = func() (kubeAPI, error) { return k, nil }
	newFirewallDiscoverer = func(token string) firewallDiscoverer {
		if token != "tok" {
			t.Errorf("discoverer token = %q", token)
		}
		return d
	}
	t.Setenv("NODE_NAME", "lke393244-59879-0a1b")
	t.Setenv("LINODE_TOKEN", "tok")
}

// fullDiscoverer returns a fake resolving fid 7, cluster 393244 (via the
// instance field), and VPC subnet 10.0.0.0/24.
func fullDiscoverer() *fakeDiscoverer {
	return &fakeDiscoverer{
		firewalls:    []map[string]any{{"id": float64(7), "label": "primary-nodes-fw"}},
		lkeClusterID: 393244,
		configs: []map[string]any{{"interfaces": []any{
			map[string]any{"purpose": "vpc", "vpc_id": float64(77), "subnet_id": float64(88)},
		}}},
		subnets: []map[string]any{{"id": float64(88), "ipv4": "10.0.0.0/24"}},
	}
}

func nodeObj(providerID string) map[string]any {
	return map[string]any{"spec": map[string]any{"providerID": providerID}}
}

func TestDiscoverFirewallConfigCreatesPatchesAndSkipsAbsentDeployment(t *testing.T) {
	k := newFakeKube()
	k.objects["/api/v1/nodes/lke393244-59879-0a1b"] = nodeObj("linode://42")
	seamDiscover(t, k, fullDiscoverer())

	if err := runCIDiscoverFirewallConfig(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	// CM was absent: created, then patched with all three keys.
	if len(k.created) != 1 || k.created[0] != firewallConfigMapsPath {
		t.Errorf("created = %v", k.created)
	}
	cmPatches := k.patches[firewallConfigMapPath]
	if len(cmPatches) != 1 {
		t.Fatalf("cm patches = %v", cmPatches)
	}
	want := map[string]any{"data": map[string]string{
		"LINODE_FIREWALL_ID": "7", "LKE_CLUSTER_ID": "393244", "VPC_CIDR": "10.0.0.0/24",
	}}
	if !reflect.DeepEqual(cmPatches[0], want) {
		t.Errorf("cm patch = %#v, want %#v", cmPatches[0], want)
	}
	// Deployment absent → no restart patch.
	if len(k.patches[firewallDeploymentPath]) != 0 {
		t.Errorf("deployment should not be patched when absent: %v", k.patches[firewallDeploymentPath])
	}
}

func TestDiscoverFirewallConfigSteadyStateIsANoOp(t *testing.T) {
	k := newFakeKube()
	k.objects["/api/v1/nodes/lke393244-59879-0a1b"] = nodeObj("linode://42")
	k.objects[firewallConfigMapPath] = map[string]any{"data": map[string]any{
		"LINODE_FIREWALL_ID": "7", "LKE_CLUSTER_ID": "393244", "VPC_CIDR": "10.0.0.0/24",
		"FIREWALL_TEMPLATE_ID": "chart-owned", // untouched foreign key
	}}
	k.objects[firewallDeploymentPath] = map[string]any{"kind": "Deployment"}
	seamDiscover(t, k, fullDiscoverer())

	if err := runCIDiscoverFirewallConfig(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(k.patches) != 0 || len(k.created) != 0 {
		t.Errorf("steady state must not write: patches=%v created=%v", k.patches, k.created)
	}
}

func TestDiscoverFirewallConfigRollsDeploymentOnChange(t *testing.T) {
	k := newFakeKube()
	k.objects["/api/v1/nodes/lke393244-59879-0a1b"] = nodeObj("linode://42")
	// Chart placeholders present, values empty → both keys change.
	k.objects[firewallConfigMapPath] = map[string]any{"data": map[string]any{
		"LINODE_FIREWALL_ID": "", "LKE_CLUSTER_ID": "",
	}}
	k.objects[firewallDeploymentPath] = map[string]any{"kind": "Deployment"}
	// No VPC interface on the fake → VPC_CIDR never patched (chart default preserved).
	d := fullDiscoverer()
	d.configs = nil
	seamDiscover(t, k, d)

	if err := runCIDiscoverFirewallConfig(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	cmPatch := k.patches[firewallConfigMapPath][0].(map[string]any)["data"].(map[string]string)
	if _, has := cmPatch["VPC_CIDR"]; has {
		t.Error("empty vpcCIDR must not be patched")
	}
	if len(k.patches[firewallDeploymentPath]) != 1 {
		t.Fatalf("deployment restart patches = %v", k.patches[firewallDeploymentPath])
	}
	// The restart patch carries the kubectl restartedAt annotation.
	b := fmt.Sprintf("%v", k.patches[firewallDeploymentPath][0])
	if !strings.Contains(b, "kubectl.kubernetes.io/restartedAt") {
		t.Errorf("restart patch = %s", b)
	}
}

func TestDiscoverFirewallConfigInputValidation(t *testing.T) {
	k := newFakeKube()
	seamDiscover(t, k, fullDiscoverer())

	t.Setenv("NODE_NAME", "")
	if err := runCIDiscoverFirewallConfig(context.Background()); err == nil || !strings.Contains(err.Error(), "NODE_NAME") {
		t.Errorf("missing NODE_NAME: %v", err)
	}
	t.Setenv("NODE_NAME", "n1")
	t.Setenv("LINODE_TOKEN", "")
	if err := runCIDiscoverFirewallConfig(context.Background()); err == nil || !strings.Contains(err.Error(), "LINODE_TOKEN") {
		t.Errorf("missing LINODE_TOKEN: %v", err)
	}
	t.Setenv("LINODE_TOKEN", "tok")
	// Node absent in fake → hard error (NODE_NAME must be real).
	if err := runCIDiscoverFirewallConfig(context.Background()); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("missing node: %v", err)
	}
	// Non-linode providerID.
	k.objects["/api/v1/nodes/n1"] = nodeObj("aws:///us-east-1a/i-abc")
	if err := runCIDiscoverFirewallConfig(context.Background()); err == nil || !strings.Contains(err.Error(), "providerID") {
		t.Errorf("bad providerID: %v", err)
	}
}

func TestFirewallConfigChanges(t *testing.T) {
	// Empty discoveries never clobber existing values.
	existing := map[string]string{"LKE_CLUSTER_ID": "1", "VPC_CIDR": "10.0.0.0/8"}
	got := firewallConfigChanges(existing, "7", "", "")
	if !reflect.DeepEqual(got, map[string]string{"LINODE_FIREWALL_ID": "7"}) {
		t.Errorf("changes = %v", got)
	}
	// Drift on one key patches only that key.
	existing = map[string]string{"LINODE_FIREWALL_ID": "7", "LKE_CLUSTER_ID": "1", "VPC_CIDR": "10.0.0.0/8"}
	got = firewallConfigChanges(existing, "9", "1", "10.0.0.0/8")
	if !reflect.DeepEqual(got, map[string]string{"LINODE_FIREWALL_ID": "9"}) {
		t.Errorf("changes = %v", got)
	}
}

// TestResolveFirewallInputs exercises the resolver walk: firewall cardinality,
// the lke_cluster_id → node-name fallback, and the optional VPC CIDR.
func TestResolveFirewallInputs(t *testing.T) {
	ctx := context.Background()

	// Full walk via the instance's lke_cluster_id.
	fid, cid, cidr, err := resolveFirewallInputs(ctx, fullDiscoverer(), 42, "lke393244-59879-0a1b")
	if err != nil || fid != "7" || cid != "393244" || cidr != "10.0.0.0/24" {
		t.Errorf("resolve = (%s,%s,%s,%v)", fid, cid, cidr, err)
	}

	// lke_cluster_id absent → node-name fallback.
	d := fullDiscoverer()
	d.lkeClusterID = 0
	if _, cid, _, _ := resolveFirewallInputs(ctx, d, 42, "lke555-1-x"); cid != "555" {
		t.Errorf("node-name fallback cid = %q, want 555", cid)
	}

	// No attached firewall is a hard error (the module wasn't applied).
	d = fullDiscoverer()
	d.firewalls = nil
	if _, _, _, err := resolveFirewallInputs(ctx, d, 42, "n"); err == nil || !strings.Contains(err.Error(), "no attached Cloud Firewall") {
		t.Errorf("no-firewall err = %v", err)
	}

	// More than one attached firewall is a hard error naming the labels.
	d = fullDiscoverer()
	d.firewalls = append(d.firewalls, map[string]any{"id": float64(9), "label": "second"})
	if _, _, _, err := resolveFirewallInputs(ctx, d, 42, "n"); err == nil || !strings.Contains(err.Error(), "second") {
		t.Errorf("multi-firewall err = %v", err)
	}
}

var _ kubeAPI = (*kube.Client)(nil) // the production client satisfies the seam
