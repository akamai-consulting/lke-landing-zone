package tofudriver

// extension.go — `tofu-driver` declares itself.
//
// TWENTY-FOURTH EXTENSION. Three thin verbs over OpenTofu: plan, output, destroy.
// The catalog is right that it is thin — "the real driver is internal/terraform" —
// and closure 3 confirms it. What is NOT right is the single row it gets.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/extension"

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
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "tofu-driver",
		Short:  "plan, read and destroy the Terraform roots — the thin verbs over internal/terraform",
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
				Name:   "destroy",
				State:  extension.Destroyed,
				Grants: []extension.Grant{extension.CloudMutate},
			},
		},
	}
}
