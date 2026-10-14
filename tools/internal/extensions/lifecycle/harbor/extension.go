package harbor

// extension.go — `harbor-provisioner` declares itself.
//
// A SET THAT SHRANK WITHOUT BEING TOUCHED — 17 outbound to 5, with nobody editing
// a Harbor file in between. Three unrelated extractions took its dependencies out
// from under it, in this order:
//
//	17 -> 10   the `gh` CLI secret writers        -> internal/ghsecret
//	10 ->  8   the gh REST secret writers         -> internal/ghsecret
//	 8 ->  6   baoKVPutFn (the last straddle)     -> internal/baoread
//	 6 ->  5   the in-cluster kubernetes login    -> internal/openbao
//
// The five that remain are `main`, `ok`, `pass`, `spec` and `ReadFile` — and the
// middle three are METHOD names the closure scanner cannot distinguish from
// package-level ones. There was never anything to untangle here.
//
// TWO LANES, ONE EXTENSION. `harbor-provisioner` is the in-cluster convergence
// loop that mints robots against a live Harbor; `seed-standby-harbor-robots` is
// the standby half, which has NO Harbor to mint against and replicates what the
// active published. They are one capability seen from the two halves of an HA
// pair — the same reasoning that kept `openbao-peer-ca`'s two verbs together, and
// stronger here, because a standby that seeds from an active that never published
// is the exact failure the second lane's clean-exit-with-a-note design exists for.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `harbor-provisioner` declaration.
//
//	transition:seeded[cluster-read, secret-custody, cloud-mutate]
//
// `seeded` AND NOT `provisioned`, which is the distinction the model keeps
// earning. Harbor itself is provisioned by apl-core long before this runs; what
// these lanes place is CREDENTIALS — two robot accounts, their secrets written to
// OpenBao KV and published as repo-level GitHub secrets. Nothing here creates
// infrastructure.
//
// `secret-custody` is unarguable: the package holds a Harbor admin password on the
// way in and two robot secrets on the way out, and calls ghsecret.Mask on every
// one of them before anything can log it.
//
// `cloud-mutate` FOR THE GITHUB WRITE, on the precedent chart-publish set. Note
// what it is NOT: there is no `cluster-write` here despite this running as an
// in-cluster Job. The package talks to Harbor over its HTTP API and to OpenBao
// through baoread.KVPut; the only thing resembling a cluster write is
// ShutdownIstioSidecar, which POSTs to the pod's OWN Envoy admin endpoint on
// 127.0.0.1 so the Job can exit — a process telling its own sidecar to stop is not
// a mutation of the cluster, and claiming the grant for it would make the word
// mean less everywhere else it appears.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "harbor-provisioner",
		Short:  "mint Harbor robot accounts, seed them into OpenBao, and publish them as repo secrets",
		Always: true,
		// FOLLOWS `harbor`. This mints Harbor robot accounts and seeds them; with no
		// Harbor there is no project to provision into and no robot to mint.
		Component: "harbor",
		Bindings: []extension.Binding{{
			Kind:  extension.Transition,
			Name:  "harbor-robots",
			State: extension.Seeded,
			Grants: []extension.Grant{
				extension.ClusterRead, extension.SecretCustody, extension.CloudMutate,
			},
		}},
	}
}

// seedBinding returns the transition whose secret-custody scopes the OpenBao
// writes below. By kind and state, not by index — obj-encryption's seedBinding
// records why.
func seedBinding() extension.Binding {
	for _, b := range Extension().Bindings {
		if b.Kind == extension.Transition && b.State == extension.Seeded {
			return b
		}
	}
	panic("harbor-provisioner: no transition:seeded binding — the custody handle is built from it")
}
