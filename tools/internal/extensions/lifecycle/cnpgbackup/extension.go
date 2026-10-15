package cnpgbackup

// extension.go — `cnpg-backup` declares itself.
//
// The cnpgBackup component switches on apl-core's own CloudNativePG backups of
// the Harbor and Keycloak databases. Nothing in the cluster says whether they
// work: the settings CR reads enabled, the Cluster carries a plugin stanza, the
// ScheduledBackup exists — and a backup that cannot reach its bucket, or one that
// lands somewhere the restore will not look, looks the same as one that works
// until the day it is needed. This extension is the gate.
//
// ONE BINDING, A TRANSITION, for the reason velero's and openbao-backup's are:
// proving a backup restores means taking one and restoring it.
//
//	transition:seeded/restore-drill  [cluster-read, cluster-write, secret-custody, cloud-mutate]
//	                                 (requires operating)
//
// cluster-read for the Cluster, ObjectStore and Backup it reads back and the
// verify Job's log; cluster-write for the Backup it takes and, in the drill, the
// canary Database, the scratch namespace, recovery Cluster and verify Job;
// secret-custody because the drill places a Secret holding the temporary bucket
// key it mints; cloud-mutate to mint and revoke that key, which is how the check
// reads the bucket from the consumer side instead of taking barman's word for
// what it wrote.
//
// The backups themselves are not here: they are apl-core's chart and the
// barman-cloud plugin's code, not llz's.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `cnpg-backup` declaration.
func Extension() extension.Extension {
	return extension.Extension{
		Name:  "cnpg-backup",
		Short: "prove apl-core's CNPG database backups land in the cnpg bucket and restore",
		// FOLLOWS `cnpgBackup`: with the component off LLZ asked apl-core for no
		// backups, so there is nothing to check.
		Component: "cnpgBackup",
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
