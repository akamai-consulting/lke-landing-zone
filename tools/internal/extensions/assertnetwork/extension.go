package assertnetwork

// extension.go — `assert-network` declares itself.
//
// TWENTY-SECOND EXTENSION, and the best ratio of the lot: closure 6 across 1,267
// lines. Four lanes that assert the platform's ENFORCEMENT actually enforces —
// NetworkPolicies deny what they claim to, the admission webhooks are wired, the
// wave-health ValidatingAdmissionPolicy rejects what its Go twin rejects, and a
// probe pod really cannot reach what it should not.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `assert-network` declaration.
//
//	assertion:verified "network-enforcement"   [cluster-read]
//	assertion:verified "admission-enforcement" [cluster-read]
//	assertion:verified "net-probe"             [read-repo]
//	assertion:verified "wave-health-vap"       [cluster-read]
//
// THE THIRD LANE IS NOT WHAT ITS NAME SUGGESTS, AND I DECLARED IT WRONG FIRST.
// I wrote `net-probe` as a `transition` holding cluster-write, reasoning that you
// cannot assert a NetworkPolicy denies a connection without a pod to attempt it
// from. Validate() rejected it — a transition cannot attach to `verified`, which
// is an observation state — and the rejection was right for a better reason than
// the one it gave: THE LANE DOES NOT CREATE THE POD. It is the code that runs
// INSIDE it. Its entire body is a net.DialTimeout and an exit code; the pod is
// created by the workflow, and "exit codes are the interface" is the first thing
// the file says.
//
// So it holds no cluster grant at all. It dials a TCP address and reports what
// happened. The lesson is one this branch keeps re-learning from the other
// direction: a name is not evidence, and `ci_net_probe.go` sounds like a prober
// of the cluster rather than a thing the cluster runs.
//
// WHY ExecCombined IS A CAPABILITY AND NOT A CONVENIENCE. The probe asserts a
// connection is REFUSED, which means reading the failure TEXT to tell "connection
// refused" (the policy works) from "no route to host" or a timeout (it may not).
// An error-gated, stdout-only read discards exactly the evidence the assertion is
// made of — the same distinction internal/kubectlprobe draws between "absent" and
// "we never got an answer".
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "assert-network",
		Short:  "the platform's enforcement actually enforces: policies deny, admission admits, probes are refused",
		Always: true,
		Bindings: []extension.Binding{
			{
				Kind:   extension.Assertion,
				Name:   "network-enforcement",
				State:  extension.Verified,
				Grants: []extension.Grant{extension.ClusterRead},
			},
			{
				Kind:   extension.Assertion,
				Name:   "admission-enforcement",
				State:  extension.Verified,
				Grants: []extension.Grant{extension.ClusterRead},
			},
			{
				// Runs INSIDE the probe pod: a net.DialTimeout and an exit code. It
				// touches no cluster API, so read-repo is the honest floor — every
				// binding must declare at least one grant, and this one genuinely
				// reaches nothing else.
				Kind:   extension.Assertion,
				Name:   "net-probe",
				State:  extension.Verified,
				Grants: []extension.Grant{extension.ReadRepo},
			},
			{
				Kind:   extension.Assertion,
				Name:   "wave-health-vap",
				State:  extension.Verified,
				Grants: []extension.Grant{extension.ClusterRead},
			},
		},
	}
}
