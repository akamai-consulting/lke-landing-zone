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
				// THE PROBE FIXTURE, AND IT WAS UNDECLARED. This extension CREATES a
				// namespace and a probe pod and then DELETES the namespace — an
				// `assertion:verified[cluster-read]` that stands up and tears down real
				// cluster objects. Nothing could see it: the delete went through a
				// general exec seam, and the apply goes through a raw exec.Command that
				// no seam touches at all.
				//
				// A separate transition rather than cluster-write on an assertion, for
				// the reason Validate enforces: an assertion that changes what it
				// measures is not an assertion.
				Kind:   extension.Transition,
				Name:   "probe-fixture",
				State:  extension.Converged,
				Grants: []extension.Grant{extension.ClusterRead, extension.ClusterWrite},
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

// MutatingBinding is `probe-fixture`, the only binding here that may write. Named
// rather than indexed so adding an assertion cannot shift which grants the writer
// is built from.
func MutatingBinding() extension.Binding {
	for _, b := range Extension().Bindings {
		if b.Name == "probe-fixture" {
			return b
		}
	}
	return extension.Binding{}
}
