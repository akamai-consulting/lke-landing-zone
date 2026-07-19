package main

// ci_crd_unwedge.go hardens the cluster against the 256KB metadata.annotations
// limit. A CRD applied CLIENT-SIDE carries a kubectl.kubernetes.io/last-applied-
// configuration annotation holding the entire object; for a CRD with a large
// embedded OpenAPI schema (Kyverno's policy CRDs, Gateway-API's httproutes, the
// otel collector CRD, …) that annotation runs 150-250KB — a schema bump or a
// reused-cluster stale copy then tips it over 262144 bytes, after which EVERY
// later write to the CRD (including apl-core's own ServerSideApply sync) fails
// with "metadata.annotations: Too long" and the Argo Application wedges.
//
// The annotation is dead weight once a resource is ServerSideApply-managed (SSA
// never writes it), so stripping oversized copies is a safe unwedge that also
// buys headroom on client-side-managed CRDs. Two callers share this: the
// bootstrap runs it proactively before the apl-core deploy, and the converge
// gate runs it reactively when a sync fails on the annotation limit.

import (
	"encoding/json"
	"fmt"
	"os"
)

const staleApplyAnnotation = "kubectl.kubernetes.io/last-applied-configuration"

// crdUnwedgeThreshold is the last-applied-configuration size at which we strip:
// well under the 262144-byte metadata.annotations cap so a subsequent write (or a
// schema bump) can't trip the limit, but high enough that only genuinely-large
// CRDs are touched. The largest observed live annotation was ~246KB (httproutes),
// ~16KB under the cap; 180KB leaves ~82KB of headroom.
const crdUnwedgeThreshold = 180 * 1024

// The seam both callers adapt to is kubectlRunner (ci_shared.go): run one
// kubectl invocation, returning its output and whether it exited 0.
// bootstrapDeps.kubectl already matches; the converge gate uses
// kubectlBoolViaExec. This file used to declare a structurally identical
// `kubectlBool` of its own — the same duplicate-seam-type shape ci_shared.go's
// header records having already collapsed once for the kyverno gate.
//
// One difference the shared type does NOT erase: kubectlBoolViaExec returns
// STDOUT ONLY (via execOutput), while aplGateKubectl returns COMBINED output.
// The type unifies; the constructors are not interchangeable.

// kubectlBoolViaExec adapts the package-wide execOutput seam to kubectlRunner for
// callers (the converge gate) that don't carry a bootstrapDeps.
func kubectlBoolViaExec(args ...string) (string, bool) {
	out, err := execOutput("kubectl", args...)
	return string(out), err == nil
}

// stripOversizedCRDLastApplied lists CRDs and removes the last-applied-
// configuration annotation from any whose copy exceeds crdUnwedgeThreshold.
// Best-effort and idempotent: a fresh cluster has no such CRD, a clean CRD has no
// such annotation, and removing an oversized annotation shrinks the object so the
// patch is admitted even when the current object is already over the cap. Returns
// the CRDs it stripped (for logging/testing). Never fatal — read/parse failures
// and per-CRD annotate failures are logged and skipped.
func stripOversizedCRDLastApplied(kubectl kubectlRunner) []string {
	out, ok := kubectl("get", "crd", "-o", "json")
	if !ok || out == "" {
		return nil // no CRDs yet (fresh cluster) or a transient read failure
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name        string            `json:"name"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if json.Unmarshal([]byte(out), &list) != nil {
		return nil
	}
	var stripped []string
	for _, it := range list.Items {
		if len(it.Metadata.Annotations[staleApplyAnnotation]) < crdUnwedgeThreshold {
			continue
		}
		name := it.Metadata.Name
		if _, ok := kubectl("annotate", "crd", name, staleApplyAnnotation+"-", "--overwrite"); ok {
			stripped = append(stripped, name)
			fmt.Fprintf(os.Stderr, "::notice::stripped oversized %s from CRD %s (annotation-limit unwedge)\n", staleApplyAnnotation, name)
		} else {
			fmt.Fprintf(os.Stderr, "::warning::could not strip oversized %s from CRD %s — a sync to it may wedge on the 256KB annotation limit\n", staleApplyAnnotation, name)
		}
	}
	return stripped
}
