package assertobjstore

// extension.go — `assert-objstore` declares itself.
//
// THIRTY-THIRD EXTENSION. SIX FOR SIX ON HIDDEN MUTATIONS — except this one is not
// hidden, and that is the interesting part. Every previous `assert-` lane buried
// its write somewhere a reader would not look: a health check that patches Argo
// Applications, a file called "readiness" that runs `kubectl rollout restart`, a
// rotation drill that applies a Job, a "smoke test" that creates a Keycloak client
// and a user. This one announces it in step 3 of its own header — "PUTs a small
// object, GETs it back and compares the bytes, then deletes it".
//
// THE WRITE IS THE CHECK, and it cannot be otherwise. Linode's object-storage
// generations are disjoint namespaces on different hosts, so `verify-object-storage`
// could list the account's buckets through the Linode API and confirm all four
// exist by label while Loki and Harbor both returned NoSuchBucket in production.
// Two views that agree with themselves and not with each other. The only check that
// can tell them apart speaks S3 at the endpoint the CONSUMER uses, with the
// credential the CONSUMER holds — which means writing.

import (
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

// Extension is the `assert-objstore` declaration.
//
//	transition:converged[cluster-read, secret-read, cloud-mutate]
//
// A TRANSITION, WHICH IS THE FIFTH TIME THE MODEL HAS FORCED THAT SPELLING ON A
// CHECK, and the pattern is now worth naming rather than repeating. `converge`,
// `assert-observability`, `assert-secrets`, `assert-identity` and this one all
// VERIFY something by mutating it, and each ends up declared
// `transition:converged` for the same two mechanical reasons: an assertion may
// hold read grants only, and `bindableStates` gives Transition no way to reach
// `verified`.
//
// So the declaration is true — it does hold those grants, and it does run at
// `converged` — but it is true the way a forced move is true. What a reader wants
// to know is "this is a check, and checking it requires writing", and the
// vocabulary can say the second half only by dropping the first. That is the same
// family of gap as the missing diagnostic kind (`argocd-diagnostics`), from the
// opposite side: one is an observation that contributes no verdict, this is a
// verdict that cannot be reached by observing.
//
// NOTHING IS INVENTED HERE. Five instances is well past the two-case bar, but the
// bar is two cases AND a shape, and the shape is not settled: these five differ in
// whether the mutation is incidental (converge patches to make progress) or
// constitutive (this one writes because writing IS the question). A kind that
// merged them would hide exactly the distinction a reviewer needs. Recorded for
// whoever takes the fifth-kind question up, alongside the diagnostic case.
//
// GRANTS, each earned:
//
//   - cluster-read — reads each consumer's live ConfigMap/Secret for its endpoint
//     and bucket. It REFUSES to derive an endpoint from the Linode API or the
//     spec, because that derived view is the one that was already wrong.
//   - secret-read — reads the consumer's OWN mounted credential, not one minted
//     for the occasion. That is the point: a credential minted here would test a
//     path the consumer never takes.
//   - cloud-mutate — the PUT and the DELETE against object storage.
//
// It does NOT hold cloud-read: it never asks the Linode API anything, and the
// whole extension exists because the Linode API's answer was the misleading one.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "assert-objstore",
		Short:  "each consumer can actually write, read back and delete in its own bucket, at its own endpoint",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Transition,
			State:  extension.Converged,
			Grants: []extension.Grant{extension.ClusterRead, extension.SecretRead, extension.CloudMutate},
		}},
	}
}

// objstoreCluster is this package's cluster handle, built from the declaration
// rather than from a package var.
//
// EVERY CALL THROUGH IT IS A `get`. The binding declares cluster-read and no
// cluster-write, so the handle permits reads and refuses mutations — which is the
// grant made real rather than described. The lane's WRITE is to object storage,
// over S3, which is what `cloud-mutate` on the same binding covers; nothing it
// does to the cluster is more than looking.
//
// Reached through a function rather than stored, so the seam is read at call time
// — the capture bug this campaign has on record.
func objstoreCluster() capability.Cluster { return capability.For(roundtripBinding()).Cluster }

// roundtripBinding returns the transition whose grants scope the reads above. By
// kind and state, not by index — database-provisioner's namedBinding records why.
func roundtripBinding() extension.Binding {
	for _, b := range Extension().Bindings {
		if b.Kind == extension.Transition && b.State == extension.Converged {
			return b
		}
	}
	panic("assert-objstore: no transition:converged binding — the cluster handle is built from it")
}

// cloudBinding is the binding this package reaches Linode through — the one
// carrying a cloud grant. Looked up rather than reconstructed, following objenc's
// seedBinding: handles belong to a BINDING, and an extension with several must
// not hand back the union.
func cloudBinding() extension.Binding {
	for _, b := range Extension().Bindings {
		for _, g := range b.Grants {
			if g == extension.CloudRead || g == extension.CloudMutate {
				return b
			}
		}
	}
	panic("assert-objstore: no binding carries a cloud grant — the Linode client is built from " +
		"one, so its absence is a wiring bug")
}
