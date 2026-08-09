package objenc

// extension.go — `obj-encryption` declares itself, and is the first extension to
// bind `seeded`.
//
// TENTH EXTRACTION, AND THE GROUP THE OLD CEILING BANNED OUTRIGHT. PR #15's
// `kind: check|tool` menu had no seeder skeleton, so the whole `→ seeded` group —
// 6,874 lines of credential provisioning — was inexpressible *by omission*. That
// omission is the single defect the declaration model was built to fix. This is
// the first extension to actually occupy the state, which makes it the test of
// whether the fix was real or only argued.
//
// It was real. No ceiling change was needed.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `obj-encryption` declaration.
//
//	transition:seeded    [secret-custody]              mint the SSE-C key into OpenBao
//	invariant:operating  [secret-custody, cluster-read] the proxy that holds and uses it
//	assertion:verified   [cluster-read, cloud-read]     prove objects are actually encrypted
//
// WHY CUSTODY IS LITERAL HERE. Linode Object Storage implements exactly ONE
// server-side encryption mode — SSE-C, where the *client* supplies the key on every
// request. SSE-S3 answers 400 and PutBucketEncryption answers 501. So there is no
// arrangement where the provider holds the key: the platform mints a 32-byte key,
// keeps it in OpenBao at `secret/obj/ssec`, and hands it to the one process that
// injects it into every write. `secret-custody` is not a label on this extension,
// it is the thing the extension is.
//
// THREE BINDINGS AT THREE MOMENTS, and the grants differ because the risk does:
//
//   - The SEED runs once, at `seeded`, and touches nothing but OpenBao. It holds
//     no cluster grant because it does not read the cluster — and the refusal that
//     protects everything is inside it: an INDETERMINATE read (sealed pod, rejected
//     token) must never be treated as "no key here", because minting a second key
//     makes every object written under the first unreadable. `Deps.KVGet` returns
//     three values, not a bool, for exactly that.
//   - The PROXY holds the key continuously at `operating` and also reads the
//     cluster for its own config. An invariant, because "objects landing in object
//     storage are encrypted" has to keep being true, not merely have been true.
//   - The ASSERTION holds NO custody. It proves encryption by observing — a HEAD
//     with no SSE-C header returns 400 for an encrypted object and 200 for a
//     plaintext one, so the check needs no key at all. That is the read-only
//     ceiling paying for itself: the gate that proves the key is working cannot
//     itself hold the key.
//
// NOT DECLARED, and deliberately: the `sync`/drain verbs. See Incomplete.
func Extension() extension.Extension {
	return extension.Extension{
		Name:      "obj-encryption",
		Short:     "mint, hold and prove the SSE-C key that encrypts Linode Object Storage",
		Always:    false,
		Component: "objProxy", // obj-encryption exists to protect what the OBJ proxy writes; its own declaration
		// already said "once, when spec.components.objProxy is enabled", and this is that
		// sentence made load-bearing.
		Bindings: []extension.Binding{
			{
				// `llz ci seed-ssec-key` — generate the key and write it to
				// OpenBao, once, when spec.components.objProxy is enabled.
				Kind:   extension.Transition,
				State:  extension.Seeded,
				Grants: []extension.Grant{extension.SecretCustody},
			},
			{
				// `llz obj-proxy` — the in-cluster S3 gateway that injects SSE-C
				// headers for writers that cannot ask for encryption themselves.
				Kind:  extension.Invariant,
				Name:  "obj-proxy",
				State: extension.Operating,
				Grants: []extension.Grant{
					extension.SecretCustody, extension.ClusterRead,
				},
			},
			{
				// `llz ci assert-obj-encryption` — the runtime gate. Read-only by
				// construction; see above.
				Kind:  extension.Assertion,
				State: extension.Verified,
				Grants: []extension.Grant{
					extension.ClusterRead, extension.CloudRead,
				},
			},
		},
		Incomplete: []string{
			"ci_drain_obj_buckets.go — the destroy-time bucket drain. It belongs to the " +
				"teardown path by state and to this one by subject, and it reads objkey " +
				"credentials that are credential-objkey's territory; nothing settles which " +
				"extension owns a verb that two of them have a claim on",
		},
	}
}

// seedBinding returns the `transition:seeded[secret-custody]` binding — the one
// that mints and places the SSE-C key, and therefore the one whose grants decide
// what ObjencDeps may do.
//
// LOOKED UP BY KIND AND STATE, NOT BY INDEX. `Bindings[0]` would be correct today
// and silently wrong the moment someone reorders the slice or adds a binding above
// it — and what it would be wrong ABOUT is which grants the custody handle is built
// from. This campaign has a positional test that survived a reorder on record;
// this is the same hazard pointed at production wiring instead.
//
// It panics rather than returning a zero Binding, because a zero Binding declares
// no grants: capability.For would hand back refusing handles and the seeder would
// fail at the write with a permission message, sending the reader after a grant
// bug that does not exist. TestSeedBindingIsFound pins it so the panic is
// unreachable in a built binary.
func seedBinding() extension.Binding {
	for _, b := range Extension().Bindings {
		if b.Kind == extension.Transition && b.State == extension.Seeded {
			return b
		}
	}
	panic("obj-encryption: no transition:seeded binding — ObjencDeps builds its OpenBao " +
		"custody handle from it, so its absence is a wiring bug, not a missing feature")
}
