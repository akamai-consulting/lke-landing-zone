package tofudriver

// extension.go — `tofu-driver` declares itself.
//
// TWENTY-FOURTH EXTENSION. Three thin verbs over OpenTofu: plan, output, destroy.
// The catalog is right that it is thin — "the real driver is internal/terraform" —
// and closure 3 confirms it. What is NOT right is the single row it gets.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `tofu-driver` declaration.
//
//	assertion:provisioned "plan"    [cloud-read]
//	assertion:provisioned "output"  [cloud-read]
//	transition:destroyed  "destroy" [cloud-mutate]
//
// THE CATALOG FILES ALL THREE UNDER `→ provisioned` WITH `cloud-mutate`, and two
// of the three mutate nothing. `tf-plan` computes a plan and reports
// changed/unchanged — that is the whole contract the calling job gates on, and a
// plan that applied something would be a plan nobody could trust. `tf-output`
// reads values out of state. Handing either a cloud-mutate grant would be the
// over-granting per-binding grants exist to prevent, and it would do it on the two
// verbs a reviewer is most likely to assume are safe.
//
// `tf-destroy` is the one that mutates, and it belongs at `destroyed` rather than
// `provisioned`: it is not a step toward having infrastructure, it is the step
// that ends it.
//
// WHY THIS IS NOT `teardown` DUPLICATED. `teardown` (extracted sixth) holds
// transition:destroyed too, and the two are genuinely different: teardown is the
// ORCHESTRATION — detach volumes, sweep orphans, assert nothing is left billing —
// while this is the raw `tofu destroy` it drives. One is the plan for ending a
// cluster's life; the other is a verb in it. They share a state because they are
// about the same moment, which is what states are for.
//
// No ceiling change: cloud-mutate is legal at `destroyed` — that row exists for
// exactly this — and cloud-read is unrestricted.
// TWO ROWS ADDED WHEN apply/import FINALLY MOVED, and they are why those two sat
// in internal/cli long after plan/output/destroy left: this extension declared
// only ASSERTIONS at `provisioned`, and an apply is a TRANSITION. Moving them
// without arguing a binding would have put a cloud write behind a `cloud-read`
// declaration.
//
// `import` holds cloud-mutate and not cloud-read, which is worth stating because
// the verb sounds read-only. It writes TERRAFORM STATE, and this instance's state
// lives in a remote object-storage backend — so importing mutates something in the
// cloud even though it creates no Linode resource.
//
// No ceiling change either way: cloud-mutate is legal at `provisioned`.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "tofu-driver",
		Short:  "plan, apply, import, read and destroy the Terraform roots — the thin verbs over internal/terraform",
		Always: true,
		Bindings: []extension.Binding{
			{
				Kind:   extension.Assertion,
				Name:   "plan",
				State:  extension.Provisioned,
				Grants: []extension.Grant{extension.CloudRead},
			},
			{
				Kind:   extension.Assertion,
				Name:   "output",
				State:  extension.Provisioned,
				Grants: []extension.Grant{extension.CloudRead},
			},
			{
				Kind:   extension.Transition,
				Name:   "apply",
				State:  extension.Provisioned,
				Grants: []extension.Grant{extension.CloudMutate},
			},
			{
				Kind:   extension.Transition,
				Name:   "import",
				State:  extension.Provisioned,
				Grants: []extension.Grant{extension.CloudMutate},
			},
			{
				Kind:   extension.Transition,
				Name:   "destroy",
				State:  extension.Destroyed,
				Grants: []extension.Grant{extension.CloudMutate},
			},
			{
				// THE WIDEST GRANT IT COULD EXERCISE, because the passthrough does not
				// choose a verb — the operator types it after `--`, so `llz tofu --
				// apply` is reachable and nothing here can narrow it. At `provisioned`
				// rather than `destroyed` because `destroy` is a thing you can type,
				// not what the verb is for; `tf-destroy` above is the declared path.
				Kind:   extension.Transition,
				Name:   "tofu",
				State:  extension.Provisioned,
				Grants: []extension.Grant{extension.CloudMutate},
			},
		},
	}
}

// cloudBinding is the binding a Linode call is made under, chosen by NAME because
// this extension declares several with DIFFERENT cloud permissions.
//
// THE RULE IS THE NARROWEST BINDING THAT COVERS WHAT THE CALL ACTUALLY DOES, and
// it is applied by reading the HTTP verb rather than the function name. `RunCITFImport` and `HealFirewallCollision` both sit in mutating code paths and
// both call only ListVPCs/ListVPCSubnets/ListFirewalls/GetKubeconfig — GETs. They
// take `plan`, the read assertion, not `import` or `apply`. Naming them for the
// path they run in would have handed two read-only calls the grant that destroys.
func cloudBinding(name string) extension.Binding {
	for _, b := range Extension().Bindings {
		if b.Name == name {
			return b
		}
	}
	panic("tofu-driver: no binding named " + name + " — its Linode client is built from one, " +
		"so its absence is a wiring bug")
}
