package openbaobackup

// extension.go — `openbao-backup` declares itself.
//
// OpenBao holds the one kind of platform state that cannot be re-derived: the
// secrets themselves, and the policies, roles and tokens around them. Everything
// else a lost cluster needs comes back from Git, Terraform state or a re-mint.
// This extension is the in-cluster half of keeping a copy — the scheduled Raft
// snapshot — plus the gate that proves a copy restores.
//
// TWO BINDINGS, AND THEIR GRANTS ARE DELIBERATELY NOT UNIONED.
//
//	invariant:operating/backup         [cluster-read, secret-custody]
//	transition:seeded/restore-drill    [cluster-read, cluster-write, secret-custody, cloud-mutate]
//	                                   (requires operating)
//
// `backup` is the CronJob verb. cluster-read is for the ServiceAccount JWT it
// exchanges at the kubernetes auth mount; secret-custody is what it then holds — a
// token that can read a full (encrypted) copy of the store, and the object-storage
// key it writes that copy with. It needs no cluster-write and no cloud grant: it
// never touches a Kubernetes object or the Linode API, only OpenBao and one bucket.
//
// `restore-drill` is the gate, and it is a TRANSITION, not an assertion, because
// you cannot prove a restore restores without performing one. It writes a canary,
// runs a backup Job, mints a temporary bucket key and rolls OpenBao back. Each of
// those is a grant it really uses, and an assertion may hold none of them — the
// same reason assert-secrets declares its broad-PAT exercise as a transition.
// Requires operating, like `openbao`'s regen-root: it restores a platform that is
// already up.
//
// The operator-facing restore (`llz openbao restore`) is NOT here. It is declared
// by the `openbao` extension, which owns `llz openbao`; both call the same
// internal/shared/baosnap functions, which is what keeps the drill honest about
// the command it stands in for.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `openbao-backup` declaration.
func Extension() extension.Extension {
	return extension.Extension{
		Name:  "openbao-backup",
		Short: "snapshot OpenBao's Raft store to object storage on a schedule, and prove a snapshot restores",
		// FOLLOWS `openbaoBackup`: the CronJob is that component, and a disabled
		// component means no producer, so neither binding has anything to act on.
		Component: "openbaoBackup",
		Bindings: []extension.Binding{
			{
				Kind:  extension.Invariant,
				Name:  "backup",
				State: extension.Operating,
				Grants: []extension.Grant{
					extension.ClusterRead, extension.SecretCustody,
				},
			},
			{
				Kind:     extension.Transition,
				Name:     "restore-drill",
				State:    extension.Seeded,
				Requires: extension.Operating,
				Grants: []extension.Grant{
					extension.ClusterRead, extension.ClusterWrite,
					extension.SecretCustody, extension.CloudMutate,
				},
			},
		},
	}
}
