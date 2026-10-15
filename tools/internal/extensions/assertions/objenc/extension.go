package objenc

// extension.go — `obj-encryption` declares itself.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `obj-encryption` declaration.
//
//	assertion:verified   [cluster-read, cloud-read]     prove objects are actually encrypted
//
// ONE BINDING, AND IT HOLDS NO CUSTODY. Linode Object Storage encrypts objects with
// SSE-S3 by default on E2/E3 endpoints, with keys Linode manages. There is nothing
// for the platform to seed or to inject, so all that is left is the part that was
// always the point: observe real objects and fail when one is stored in the clear.
//
// ALWAYS ON. Every deployment writes Loki chunks and Harbor layers to Object
// Storage, so every deployment is subject to the question. It used to follow the
// objProxy component, which made "encryption at rest" something an instance had to
// opt into — and a deployment that never did was told nothing.
//
// It previously also declared a `seeded` transition (mint the SSE-C key into
// OpenBao) and an `operating` invariant (the obj-proxy gateway that injected it).
// Both were retired with the gateway; see docs/designs/obj-sse-c-gateway.md.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "obj-encryption",
		Short:  "prove objects in Linode Object Storage are encrypted at rest",
		Always: true,
		Bindings: []extension.Binding{
			{
				// `llz ci assert-obj-encryption` — the runtime gate. Read-only by
				// construction: it HEADs objects with the consumer's own credential.
				Kind:  extension.Assertion,
				State: extension.Verified,
				Grants: []extension.Grant{
					extension.ClusterRead, extension.CloudRead,
				},
			},
		},
		Incomplete: []string{
			"drain-obj-buckets — the destroy-time bucket drain. It lives in the teardown " +
				"PACKAGE (lifecycle/teardown/drain_obj_buckets.go), which settles it by state, " +
				"but no extension DECLARES it: teardown's bindings do not name it and this one " +
				"cannot, since it belongs here only by subject and it reads objkey credentials " +
				"that are credential-objkey's territory",
		},
	}
}
