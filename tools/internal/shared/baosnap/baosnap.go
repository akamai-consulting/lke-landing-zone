// Package baosnap is the OpenBao backup substrate: what a Raft snapshot is named,
// where it is stored, how one is proven intact, and how it is taken and restored
// over OpenBao's HTTP API.
//
// IT IS SHARED SUBSTRATE, NOT A CAPABILITY, because three callers in three places
// must agree on every one of those facts:
//
//   - the producer, `llz ci openbao-backup`, which runs in-cluster as the
//     openbaoBackup CronJob and writes snapshots;
//   - the operator verbs, `llz openbao backups` and `llz openbao restore`, which
//     list them and put one back;
//   - the gate, `llz ci assert-openbao-restore`, which proves the two above still
//     meet.
//
// A snapshot written under one naming rule and listed under another is the
// split-contract archetype in docs/e2e-gates.md: both sides pass their own tests
// and the restore lists nothing. So the rule lives once, here, and the gate calls
// the producer's real functions rather than a copy of them.
//
// WHAT IS DELIBERATELY NOT HERE: a Linode client, a kubectl, a binding. Minting a
// key and reaching a pod are things an extension does under its own declared
// grants; this package takes the results as arguments.
package baosnap

import "os"

const (
	// KeyPath is the OpenBao KV path of the object-storage key scoped to the
	// backup bucket. Written by the rotation table (bootstrap mint + in-cluster
	// rotation), read by the CronJob under the openbao-backup role. The fields are
	// AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, BUCKET and ENDPOINT.
	KeyPath = "secret/obj/openbao-backup"

	// Namespace, ServiceAccount, CronJob and Role are the in-cluster identity of
	// the producer. The k8s-auth role is bound to exactly this SA in exactly this
	// namespace; the manifest coupling test holds the CronJob to them.
	Namespace      = "llz-openbao-backup"
	ServiceAccount = "openbao-backup"
	CronJob        = "openbao-backup"
	Role           = "openbao-backup"

	// AuditEvent is the `event` field of the JSON record the producer prints as
	// its last line. The drill reads the key it uploaded from that record, so the
	// restore it performs is of the object THIS run wrote — not merely the newest
	// one in the bucket, which a stale earlier run could also satisfy.
	AuditEvent = "openbao-backup"
)

func isActions() bool { return os.Getenv("GITHUB_ACTIONS") != "" }
