package bootstrapcluster

import (
	"strings"
	"testing"
	"time"
)

// The annotation is a verbatim quote from apl-core's 6.1.0 release notes:
//
//	kubectl annotate deployment apl-operator \
//	  argocd.argoproj.io/sync-options=Force=true,Replace=true -n apl-operator
//
// A typo here is invisible until a managed upgrade fails to sync months later, so
// pin the literal rather than re-deriving it from the constants under test.
func TestAplSyncOptionsMatchUpstream(t *testing.T) {
	if got := AplSyncOptionsKey + "=" + AplSyncOptionsValue; got != "argocd.argoproj.io/sync-options=Force=true,Replace=true" {
		t.Errorf("annotation = %q, want apl-core 6.1.0's documented prerequisite", got)
	}
	if AplOperatorNamespace != "apl-operator" || AplOperatorDeployment != "apl-operator" {
		t.Errorf("target = %s/%s, want apl-operator/apl-operator", AplOperatorNamespace, AplOperatorDeployment)
	}
}

// The happy path: the Deployment exists, so it gets annotated — with --overwrite,
// because a second bootstrap of the same cluster re-asserts an annotation that is
// already there and kubectl refuses that without the flag.
func TestPrepareAplUpgrade_AnnotatesWithOverwrite(t *testing.T) {
	var cmds []string
	d := bootstrapDeps{kubectl: func(args ...string) (string, bool) {
		line := strings.Join(args, " ")
		cmds = append(cmds, line)
		return "deployment.apps/apl-operator", true
	}}
	applied, err := PrepareAplUpgrade(d)
	if err != nil || !applied {
		t.Fatalf("PrepareAplUpgrade() = %v, %v; want applied, nil", applied, err)
	}
	joined := strings.Join(cmds, "\n")
	for _, want := range []string{
		"-n apl-operator annotate --overwrite deployment apl-operator",
		"argocd.argoproj.io/sync-options=Force=true,Replace=true",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("kubectl calls missing %q; got:\n%s", want, joined)
		}
	}
}

// A cluster with no apl-operator Deployment (managed apl-core not installed yet) is
// NOT a failure — it means there is nothing to prepare. It must also not attempt the
// annotate, or the error would be reported as a real one.
func TestPrepareAplUpgrade_AbsentDeploymentIsNotAnError(t *testing.T) {
	var annotated bool
	d := bootstrapDeps{kubectl: func(args ...string) (string, bool) {
		line := strings.Join(args, " ")
		if strings.Contains(line, "annotate") {
			annotated = true
		}
		return `Error from server (NotFound): deployments.apps "apl-operator" not found`, false
	}}
	applied, err := PrepareAplUpgrade(d)
	if err != nil {
		t.Fatalf("an absent Deployment must not be an error, got: %v", err)
	}
	if applied {
		t.Error("applied must be false when there is no Deployment to annotate")
	}
	if annotated {
		t.Error("must not attempt to annotate a Deployment that does not exist")
	}
}

// A get that succeeds followed by an annotate that fails IS a real error — the
// caller decides whether to warn or abort, but it must not be silently swallowed
// into the same "nothing to do" result as an absent Deployment.
func TestPrepareAplUpgrade_AnnotateFailureIsAnError(t *testing.T) {
	d := bootstrapDeps{kubectl: func(args ...string) (string, bool) {
		if strings.Contains(strings.Join(args, " "), "annotate") {
			return "Error from server (Forbidden): deployments.apps is forbidden", false
		}
		return "deployment.apps/apl-operator", true
	}}
	if _, err := PrepareAplUpgrade(d); err == nil {
		t.Fatal("a failed annotate must surface as an error")
	} else if !strings.Contains(err.Error(), "Forbidden") {
		t.Errorf("error should carry kubectl's reason, got: %v", err)
	}
}

// bootstrap-cluster must assert the prerequisite on every apply (Linode owns when
// the managed upgrade rolls, so there is no upgrade hook to hang it on), and must
// NOT fail the bridge when it can't: a missing annotation degrades a FUTURE upgrade,
// it does not affect the bridge being placed now.
func TestBootstrapCluster_PreparesAplUpgradeBestEffort(t *testing.T) {
	run := func(annotateOK bool) (annotated bool, err error) {
		d := bootstrapDeps{
			kubectl: func(args ...string) (string, bool) {
				line := strings.Join(args, " ")
				switch {
				case strings.Contains(line, "annotate") && strings.Contains(line, "apl-operator"):
					annotated = true
					return "", annotateOK
				case strings.Contains(line, "crd applications.argoproj.io"):
					return "applications.argoproj.io", true
				case strings.Contains(line, "deploy argocd-server"):
					return "1", true
				}
				return "", true
			},
			apply: func(string, string, bool) (string, bool) { return "", true },
			now:   time.Now, sleep: func(time.Duration) {},
		}
		o := bootstrapClusterOpts{
			env: "primary", clusterID: "393244", instanceRepo: "acme/instance",
			upstreamOrg: "akamai-consulting", templateRef: "ref", appsRepoRevision: "main",
		}
		return annotated, bootstrapCluster(o, d)
	}
	if annotated, err := run(true); err != nil || !annotated {
		t.Errorf("bootstrap must annotate apl-operator; annotated=%v err=%v", annotated, err)
	}
	if _, err := run(false); err != nil {
		t.Errorf("a failed annotate must not fail the bridge apply, got: %v", err)
	}
}
