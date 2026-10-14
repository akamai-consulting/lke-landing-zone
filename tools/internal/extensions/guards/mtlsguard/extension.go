package mtlsguard

// extension.go — `mtls-wiring` declares itself.
//
// FIFTY-THIRD EXTENSION, CLOSURE 1 — `main`, and nothing else. 282 lines that
// needed exactly nothing from package main and were never measured until the
// extraction ran out of hard candidates and started sweeping.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `mtls-wiring` declaration.
//
//	gate:scaffolded[read-repo]
//
// A GATE, AND A REMARKABLE ONE. Gates read files alone, and this reads manifests
// — but the thing it checks is a fact about RUNTIME: that a pod declaring
// OPENBAO_ADDR mounts the three TLS files openbao.InClusterHTTPClient() will read.
// It is a static check standing in for a cluster test that nobody can run at
// scaffold time.
//
// Its own header records why that matters: deleting the reconciler's
// client-certificate volumeMount used to pass EVERY gate in this repo — kustomize
// rendered, kubeconform was satisfied, `make lint-k8s` was clean — and the pod
// could not reach OpenBao at all. Verified by mutation, not assumed.
//
// `read-repo` AND NOTHING ELSE, which is what makes the gate kind legal here.
// Validate() refuses a gate holding any other grant, and this one genuinely
// qualifies: it opens YAML and compares strings, with no cluster and no cloud.
// The invariant is INFERRED rather than registered — a new OpenBao consumer
// inherits the requirement automatically — which is the property an allowlist
// version would have lost.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "mtls-wiring",
		Short:  "every pod that talks to OpenBao must mount the client TLS material its code path reads",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Gate,
			State:  extension.Scaffolded,
			Grants: []extension.Grant{extension.ReadRepo},
		}},
	}
}
