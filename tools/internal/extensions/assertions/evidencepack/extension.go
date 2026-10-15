package evidencepack

// extension.go — `cis-evidence` declares itself.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `cis-evidence` declaration.
//
//	assertion:operating "cis-evidence" [cluster-read, read-repo]
//
// OPERATING, NOT VERIFIED. The pack is a scheduled report over a cluster that has
// been running for months — the evidence an auditor signs is about the cluster as
// it is today, not as release-e2e built it. It is also not a battery lane: its
// OUTPUT is the deliverable, and the gates whose outcomes it records already have
// their own lanes.
//
// cluster-read: it lists ClusterComplianceReports, NetworkPolicies, Namespaces
// and StorageClasses, and reads trivy-operator's config ConfigMap for the
// compliance-scanner switch. It never reads a Secret — the gate outcomes it
// records arrive as flags from the steps that ran those gates.
//
// read-repo: the answered checklist is instance DATA (infosec/questionnaire.yaml
// by default), read through the fenced repo handle rather than compiled in.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "cis-evidence",
		Short:  "harvest compliance evidence and the instance's answered checklist into an auditor evidence pack",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Assertion,
			Name:   "cis-evidence",
			State:  extension.Operating,
			Grants: []extension.Grant{extension.ClusterRead, extension.ReadRepo},
		}},
	}
}
