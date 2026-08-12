package cli

import (
	"os"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// THE DEFECT WAS AN ORDERING, NOT A MISSING CHECK. llz-release.yml's `image-tag`
// job already refused to retag a missing image and said so loudly — but it
// declared no `needs:`, so on v0.0.44 it failed while "Attach binaries to the
// release" had already succeeded. The release went out installable, advertising a
// `:vX.Y.Z` image that did not exist.
//
// No Go test could see that: the bug lived entirely in the job graph. This reads
// the graph.
func TestReleasePublishingJobsWaitOnTheImagePreflight(t *testing.T) {
	b, err := os.ReadFile("../../../.github/workflows/llz-release.yml")
	if err != nil {
		t.Fatalf("read llz-release.yml: %v", err)
	}
	var wf struct {
		Jobs map[string]struct {
			Name  string      `json:"name"`
			Needs interface{} `json:"needs"`
		} `json:"jobs"`
	}
	if err := yaml.Unmarshal(b, &wf); err != nil {
		t.Fatalf("parse llz-release.yml: %v", err)
	}
	if _, ok := wf.Jobs["preflight"]; !ok {
		t.Fatal("no `preflight` job — nothing asserts the commit's image exists before the release publishes")
	}
	// `release` attaches the binaries (makes it installable); `image-tag` creates
	// the version tag. Both must be downstream of the preflight.
	for _, job := range []string{"release", "image-tag"} {
		j, ok := wf.Jobs[job]
		if !ok {
			t.Errorf("job %q disappeared — this test is pinning a graph that no longer exists", job)
			continue
		}
		if !dependsOn(j.Needs, "preflight") {
			t.Errorf("job %q does not `needs: preflight` (needs: %v) — it can run while the image it "+
				"advertises is still missing, which is exactly how v0.0.44 shipped a `:vX.Y.Z` tag "+
				"nothing could pull", job, j.Needs)
		}
	}
}

// needs: accepts a string or a list; both spellings must satisfy the check, or the
// gate passes on a formatting choice.
func dependsOn(needs interface{}, want string) bool {
	switch v := needs.(type) {
	case string:
		return v == want
	case []interface{}:
		for _, n := range v {
			if s, ok := n.(string); ok && s == want {
				return true
			}
		}
	}
	return false
}

// The preflight must actually run the verb. A job that exists and checks nothing
// satisfies the graph test above while proving nothing at all.
func TestReleasePreflightRunsTheImageAssertion(t *testing.T) {
	b, err := os.ReadFile("../../../.github/workflows/llz-release.yml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "assert-release-image") {
		t.Error("llz-release.yml never invokes `llz ci assert-release-image` — the preflight job " +
			"would be a name with no check behind it")
	}
}
