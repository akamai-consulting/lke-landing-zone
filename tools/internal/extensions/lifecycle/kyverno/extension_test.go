package kyverno

import (
	"os"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

func TestExtensionDeclarationValidates(t *testing.T) {
	e := Extension()
	if errs := e.Validate(); len(errs) != 0 {
		t.Fatalf("declaration does not validate: %v", errs)
	}
	if e.Name != "kyverno-policies" || !e.Always {
		t.Errorf("identity drifted: name=%q always=%v", e.Name, e.Always)
	}
}

// It applies AND waits. Dropping either grant would misdescribe it: an applied
// policy that never becomes Ready is not enforcing, which is the whole reason the
// readiness poll exists.
func TestDeclaresBothHalves(t *testing.T) {
	e := Extension()
	for _, g := range []extension.Grant{extension.ClusterWrite, extension.ClusterRead} {
		if !e.HasGrant(g) {
			t.Errorf("%q dropped — this server-side applies a ClusterPolicy and then polls it Ready", g)
		}
	}
	if e.HasGrant(extension.CloudMutate) || e.HasGrant(extension.SecretCustody) {
		t.Error("this touches a cluster and nothing else")
	}
}

// THE MANIFESTS CANNOT LIVE HERE, and the test that reads them says why. If this
// path ever resolves, someone moved the policy assets and both the //go:embed in
// ci_bootstrap_cluster.go and manifestDir need revisiting together.
func TestManifestsStayWithTheEmbeddingPackage(t *testing.T) {
	if _, err := os.Stat("manifests"); err == nil {
		t.Error("a manifests/ directory appeared in this package — ci_bootstrap_cluster.go " +
			"//go:embed-s three files from tools/internal/extensions/bootstrapcluster/manifests, and Go's embed cannot reach " +
			"outside its own package. Two copies of these policies is the drift the shipped set exists to prevent")
	}
	if _, err := os.Stat(manifestDir); err != nil {
		t.Errorf("manifestDir (%s) no longer resolves: %v — the policy assets moved and this const did not", manifestDir, err)
	}
}
