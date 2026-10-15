package velero

// extension.go — `velero` declares itself.
//
// The velero component backs up Kubernetes resources to object storage. What a
// cluster cannot show is whether a backup it took will restore: the Schedule's
// Application goes Healthy the moment the Deployment does, a BackupStorageLocation
// reads Available against an empty bucket, and a bucket full of objects says
// nothing about whether Velero can read them back. This extension is the gate.
//
// ONE BINDING, AND IT IS A TRANSITION, for the reason openbao-backup's drill is:
// you cannot prove a backup restores without taking one and restoring it.
//
//	transition:seeded/restore-drill  [cluster-read, cluster-write, secret-custody, cloud-mutate]
//	                                 (requires operating)
//
// cluster-read for the Schedule, BackupStorageLocation, Backup and Restore it
// watches; cluster-write for the Backup and Restore it creates and the canary
// namespace (a ConfigMap and a Secret) it writes, deletes and requires back;
// secret-custody because that canary IS a Secret it places, and the temporary
// bucket key it mints is credential material it holds; cloud-mutate to mint and
// revoke that key, which is how the drill reads the bucket from the consumer side
// rather than taking Velero's word for what it wrote.
//
// Velero itself is not here: it is the upstream chart, and its server is Velero's
// code, not llz's.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `velero` declaration.
func Extension() extension.Extension {
	return extension.Extension{
		Name:  "velero",
		Short: "prove a Velero backup of this cluster lands encrypted and restores",
		// FOLLOWS `velero`: with the component off there is no Velero to back up
		// with and nothing to drill.
		Component: "velero",
		Bindings: []extension.Binding{
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
