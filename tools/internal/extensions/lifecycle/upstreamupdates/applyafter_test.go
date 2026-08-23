package upstreamupdates

// applyafter_test.go — the pull request must say that merging applies nothing,
// and must name what to dispatch.
//
// The failure being guarded is not a crash. It is the section quietly reverting
// to absent, or to a claim it cannot support ("these deployments are out of
// date") — both of which read as a normal green run.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A minimal but REAL spec, loaded by the real clusterspec.LoadInstance. Feeding a
// hand-built []string into applySection would test the formatter and nothing
// about whether this repo can find an instance's deployments at all.
const twoDeploymentSpec = `apiVersion: llz.akamai-consulting.io/v1alpha1
kind: LandingZone
metadata:
  name: probe-instance
spec:
  instance:
    upstreamOrg: probe-org
    repo: probe-org/probe-instance
    forge: github
`

const envYAML = `apiVersion: llz.akamai-consulting.io/v1alpha1
kind: ClusterDefinition
metadata:
  name: %s
spec:
  cluster:
    clusterLabel: probe-%s
    region: us-ord
    k8sVersion: v1.34.6+lke2
    nodePool: { type: g8-dedicated-8-4, count: 5 }
    bootstrap:
      name: probe
`

func instanceWithDeployments(t *testing.T, names ...string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "landingzone.yaml"), []byte(twoDeploymentSpec), 0o644); err != nil {
		t.Fatal(err)
	}
	envDir := filepath.Join(root, "environments")
	if err := os.MkdirAll(envDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		body := strings.ReplaceAll(envYAML, "%s", n)
		if err := os.WriteFile(filepath.Join(envDir, n+".yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// THE BEHAVIOR: an instance's deployments are discoverable, sorted, through the
// same loader the rest of the CLI uses.
func TestDeploymentsToApplyReadsTheSpec(t *testing.T) {
	root := instanceWithDeployments(t, "staging", "primary")
	got := DeploymentsToApply(root)
	if len(got) != 2 || got[0] != "primary" || got[1] != "staging" {
		t.Fatalf("DeploymentsToApply = %v, want [primary staging] (sorted, so the body is stable "+
			"across runs and a retry cannot reorder it)", got)
	}
}

// Three states where there is nothing useful to say, and none of them may be an
// error: `llz upgrade` has to keep working outside an instance, before the first
// `llz env add`, and on a spec that a bad merge left unparseable — which is the
// likeliest moment for one, since this runs right after `copier update`.
func TestDeploymentsToApplyIsQuietWhenItCannotAnswer(t *testing.T) {
	t.Run("no instance", func(t *testing.T) {
		if got := DeploymentsToApply(t.TempDir()); got != nil {
			t.Errorf("DeploymentsToApply = %v outside an instance, want nil", got)
		}
	})
	t.Run("spec with no deployment", func(t *testing.T) {
		if got := DeploymentsToApply(instanceWithDeployments(t)); len(got) != 0 {
			t.Errorf("DeploymentsToApply = %v before the first env add, want none", got)
		}
	})
	t.Run("unparseable spec", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "landingzone.yaml"), []byte("{{{ not yaml"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := DeploymentsToApply(root); got != nil {
			t.Errorf("DeploymentsToApply = %v on an unparseable spec, want nil", got)
		}
	})
}

// The section has to carry the three facts a reviewer does not otherwise have:
// that dispatch is what applies, that the absent .tf diff is expected rather than
// evidence of a no-op, and which deployments to run.
func TestApplySectionCarriesTheDispatchAndTheDeployments(t *testing.T) {
	body := applySection([]string{"primary", "staging"})
	for _, want := range []string{
		"workflow_dispatch", // what actually applies
		"gitignored",        // why there is no .tf diff to review
		"llz build primary", // the command, per deployment
		"llz build staging",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("apply section never mentions %q:\n%s", want, body)
		}
	}
	// Commands must be in a fenced block, never backticked — see the file header
	// and TestPRBodyCarriesNoBacktickedCLICommand.
	if strings.Contains(body, "`llz ") {
		t.Errorf("apply section backticks a CLI command:\n%s", body)
	}
	if !strings.Contains(body, "```") {
		t.Errorf("apply section has no fenced block, so the commands are not copy-pasteable:\n%s", body)
	}
}

// IT MUST NOT CLAIM MORE THAN IT KNOWS. Nothing records which deployments have
// been applied since, so the section is a list of deployments, not a list of
// stale ones. A reminder that overclaims is one people learn to skip — and the
// wrong version of this sentence would tell an operator their freshly-applied
// production cluster is behind.
func TestApplySectionDoesNotClaimTheDeploymentsAreStale(t *testing.T) {
	body := applySection([]string{"primary"})
	for _, overclaim := range []string{"out of date", "stale deployment", "are behind", "running the old"} {
		if strings.Contains(strings.ToLower(body), overclaim) {
			t.Errorf("apply section claims %q, which nothing here can know:\n%s", overclaim, body)
		}
	}
	if !strings.Contains(body, "not a list of stale ones") {
		t.Errorf("apply section does not disclaim what it cannot know:\n%s", body)
	}
}

// An instance with no deployment gets a sentence, not an empty fenced block. A
// dangling "After merging, apply each deployment:" with nothing under it reads as
// a bug in the bot.
func TestApplySectionHandlesAnInstanceWithNoDeployment(t *testing.T) {
	body := applySection(nil)
	if strings.Contains(body, "```") {
		t.Errorf("empty deployment list produced a fenced block with nothing in it:\n%s", body)
	}
	if !strings.Contains(body, "no deployment yet") {
		t.Errorf("empty deployment list must say so:\n%s", body)
	}
}

// The section has to actually reach the body — the whole change is inert if
// prBody composes without it.
func TestPRBodyCarriesTheApplySection(t *testing.T) {
	for _, draft := range []bool{true, false} {
		body := prBody(draft, []string{"primary"})
		if !strings.Contains(body, "llz build primary") {
			t.Errorf("prBody(%t) does not tell the merger what to apply:\n%s", draft, body)
		}
	}
}
