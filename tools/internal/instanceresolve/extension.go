package instanceresolve

// extension.go — `instance-resolve` declares itself: the four questions every
// scaffold and render path has to answer before it can do anything.
//
// SIXTY-THIRD EXTENSION, and a second sweep find in one iteration. 546 lines at
// closure 2, both edges noise.
//
// WHAT IT ANSWERS, and why the four belong together:
//
//	instance_root       am I inside an instance, and where does it start?
//	custom_layout       is this tree's layout one we can render into?
//	region_resolve      is this a real Linode region?
//	objcluster_resolve  which object-storage cluster serves it?
//
// Each is a precondition for the next, and every caller in cmd/llz asks them in
// that order. Splitting them would put four halves of one "can I proceed?"
// question in four packages.
//
// region_resolve AND objcluster_resolve TOGETHER IS THE LOAD-BEARING PART. This
// repo has been bitten by the fact that REGION IDS AND OBJ-CLUSTER IDS OVERLAP as
// strings — `isOBJClusterID` exists precisely because a region check that accepts
// an obj-cluster id passes and then fails much later, somewhere unrelated. The two
// resolvers have to agree about which namespace a string is in, and the only way
// two of them can disagree is if they are apart.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/extension"

// Extension is the `instance-resolve` declaration.
//
//	assertion:scaffolded[read-repo, cloud-read]
//
// AN ASSERTION, AND I TRIED TO MAKE IT A GATE FIRST. Validate() rejected it, and
// the rejection was right in a way worth recording: I had written that the gate
// rule bars only MUTATING grants. It does not. It permits `read-repo` and nothing
// else, and the error message says why — "it runs in the fast pre-commit path over
// files alone".
//
// That is the whole distinction. A gate is not defined by being harmless; it is
// defined by being CHEAP AND OFFLINE. `checkRegion` and `resolveOBJCluster` call
// the Linode API — they cannot answer from files, which is the point of them (a
// hardcoded region list is what they replaced, and it went stale). Something that
// needs the network does not belong in the pre-commit path no matter how read-only
// it is.
//
// So: an assertion at `scaffolded`, contributing evidence that this instance's
// inputs resolve, which is exactly what the four checks establish.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "instance-resolve",
		Short:  "resolve the instance root, layout, region and object-storage cluster before anything is rendered",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Assertion,
			State:  extension.Scaffolded,
			Grants: []extension.Grant{extension.ReadRepo, extension.CloudRead},
		}},
	}
}
