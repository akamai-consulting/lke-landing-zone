package reconciler

import (
	"context"
	"strings"
	"testing"
)

// The two optional discoveries (LKE cluster ID, VPC CIDR) each announce
// themselves when they come up empty — that line is the only signal that the
// control-plane ACL lane is disabled / the chart's VPC default is in force.
func TestDiscoverFirewallConfigAnnouncesUndiscoverableOptionals(t *testing.T) {
	k := newFakeKube()
	k.objects["/api/v1/nodes/worker-1"] = nodeObj("linode://42")
	d := fullDiscoverer()
	d.lkeClusterID = 0 // no lke_cluster_id on the instance...
	d.configs = nil    // ...and no VPC interface either
	seamDiscover(t, k, d)
	t.Setenv("NODE_NAME", "worker-1") // a node name the lke<id>- fallback cannot parse

	var err error
	out := captureStdout(t, func() { err = runCIDiscoverFirewallConfig(context.Background()) })
	if err != nil {
		t.Fatalf("both optionals are best-effort: %v", err)
	}
	for _, want := range []string{
		"LKE cluster ID not discoverable",
		"no VPC interface on the node",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
}

// ...and stays silent when both resolve, so the notices mean something.
func TestDiscoverFirewallConfigSilentWhenOptionalsResolve(t *testing.T) {
	k := newFakeKube()
	k.objects["/api/v1/nodes/lke393244-59879-0a1b"] = nodeObj("linode://42")
	seamDiscover(t, k, fullDiscoverer())

	var err error
	out := captureStdout(t, func() { err = runCIDiscoverFirewallConfig(context.Background()) })
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, unwanted := range []string{"not discoverable", "no VPC interface"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("both optionals resolved, yet stdout says %q:\n%s", unwanted, out)
		}
	}
}
