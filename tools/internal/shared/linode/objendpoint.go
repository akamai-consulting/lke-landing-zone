package linode

// objendpoint.go — reading the object-storage cluster out of an S3 endpoint.
//
// ONE COPY, BECAUSE THERE WERE TWO AND BOTH WERE WRONG IN DIFFERENT PLACES.
// `llz tokens` writes TF_STATE_ENDPOINT and two callers read the cluster back
// out of it — the onboarding wizard, to label the state bucket, and the
// credential rotation, to scope a new OBJ key. They had separate
// implementations, each with its own idea of what an endpoint looks like, and
// the cluster is the thing that decides whether a minted key can read the state
// at all: a bucket is reachable ONLY at the endpoint it was created against.
//
// BOTH SPELLINGS ARE LEGAL AND THEY DISAGREE ABOUT WHICH LABEL IS THE CLUSTER:
//
//	path-style          https://us-ord-1.linodeobjects.com
//	virtual-host style  https://<bucket>.us-ord-1.linodeobjects.com
//
// Taking the FIRST label — what the rotation's copy did — reads the bucket name
// as the cluster on the second form, and a key scoped to a cluster that does not
// exist fails at mint or, worse, succeeds against the wrong namespace. Anchoring
// on `.linodeobjects.com` and taking everything before it — what onboarding's
// copy did — returns "<bucket>.us-ord-1" on the same input.
//
// The cluster is the LAST label before the domain, in both. Bucket names may
// themselves contain dots, and that rule survives them.

import "strings"

// objEndpointDomain is the suffix every Linode Object Storage endpoint carries.
// Anchoring on it is what makes this refuse rather than guess: an endpoint that
// is not Linode's has no cluster to read, and a confidently wrong answer is the
// one outcome worse than no answer.
const objEndpointDomain = ".linodeobjects.com"

// ObjClusterFromEndpoint extracts the object-storage cluster id from an S3
// endpoint URL, in either the path-style or virtual-host spelling. It returns ""
// for anything it cannot read with certainty — a bare host, another provider's
// endpoint, or the apex domain with no cluster label at all.
//
// Tolerates a missing scheme, which is how an operator who set the variable by
// hand is most likely to have written it.
func ObjClusterFromEndpoint(endpoint string) string {
	s := strings.TrimSpace(endpoint)
	if s == "" {
		return ""
	}
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	// Drop anything after the host: a path, a query, a fragment, a port.
	for _, sep := range []string{"/", "?", "#"} {
		if i := strings.Index(s, sep); i >= 0 {
			s = s[:i]
		}
	}
	if i := strings.Index(s, ":"); i >= 0 {
		s = s[:i]
	}
	s = strings.ToLower(strings.TrimSuffix(s, "."))

	if !strings.HasSuffix(s, objEndpointDomain) {
		return ""
	}
	host := strings.TrimSuffix(s, objEndpointDomain)
	if host == "" {
		return "" // the apex: no cluster label to read
	}
	// The LAST label before the domain. Path-style has exactly one; virtual-host
	// style has the bucket in front of it, and a bucket name may contain dots.
	labels := strings.Split(host, ".")
	return labels[len(labels)-1]
}
