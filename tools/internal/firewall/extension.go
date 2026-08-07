package firewall

// extension.go — `cloud-firewall-bootstrap` declares itself: the lane that seeds
// the in-cluster state the firewall-controller reconciles from.
//
// SIXTY-FIFTH EXTENSION. Closure 3 before this iteration's enabler, closure 1
// after: `ciToken` and `readRegionTFVars` were its only real edges and both moved
// into packages that already existed — `internal/linode` and `internal/tfvars`.
//
// THAT ENABLER IS THE FOURTH OF ITS KIND and the pattern is now unmistakable.
// cmd/llz/ci_shared.go was 66 lines shared by six files, holding a Linode client
// constructor and a tfvars reader that had nothing to do with each other. Neither
// half needed a new package: the client belongs with `linode.NewClient`, the
// reader with the tfvars read/write pair extracted two moves ago. A "shared
// helpers" file is usually two or three things that were never named.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/extension"

// Extension is the `cloud-firewall-bootstrap` declaration.
//
//	transition:provisioned[read-repo, cloud-read, cluster-write]
//
// IT SEEDS STATE FOR A CONTROLLER RATHER THAN CONFIGURING A FIREWALL, and the
// grants say so. It reads the region's tfvars and asks Linode which firewall
// exists (`read-repo`, `cloud-read`), then writes a Secret and a ConfigMap into
// the cluster (`cluster-write`) — the firewall-controller does the actual
// reconciling, continuously, and that is `reconciler-runtime`'s cidr-firewall
// invariant, not this.
//
// NO `cloud-mutate`, deliberately: this lane does not change the Linode firewall.
// Claiming it would make the word mean less on the lanes that do, and it would
// misdescribe the split that exists precisely so a one-shot bootstrap and a
// continuous reconciler cannot fight over the same resource.
//
// `provisioned` because the cluster must exist to write into, and the controller
// it seeds has to be running before anything converges on top.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "cloud-firewall-bootstrap",
		Short:  "seed the node-pool Cloud Firewall state the in-cluster firewall-controller reconciles from",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:  extension.Transition,
			State: extension.Provisioned,
			Grants: []extension.Grant{
				extension.ReadRepo, extension.CloudRead, extension.ClusterWrite,
			},
		}},
	}
}
