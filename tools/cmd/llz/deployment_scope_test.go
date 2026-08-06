package main

import (
	"os"
	"path/filepath"
	"testing"
)

// writeClusterTFVars lays down the rendered tfvars both callers read.
func writeClusterTFVars(t *testing.T, deployment, body string) {
	t.Helper()
	dir := filepath.Join("terraform-iac-bootstrap", "cluster")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(dir, deployment+".tfvars"), body)
}

func TestDeploymentScopeResolvesTheLinodeRegion(t *testing.T) {
	// THE bug, at both call sites: the DEPLOYMENT name was passed as
	// --volume-region, which is a Linode-REGION filter. No Volume ever matched, so
	// the apply-side census could not warn and the destroy-side gate could go green
	// on a deployment that leaked every Volume it had.
	chdirTempDir(t)
	writeClusterTFVars(t, "lab", "region = \"us-sea\"\ncluster_label = \"lab-lke\"\n")

	region, volRegion, env := resolveDeploymentScope("lab", "", "", "")
	if region != "us-sea" {
		t.Errorf("region = %q, want the Linode region from the tfvars", region)
	}
	if volRegion != "us-sea" {
		t.Errorf("volumeRegion = %q, want the Linode region — a deployment name matches no Volume", volRegion)
	}
	if env != "lab" {
		t.Errorf("env = %q, want the deployment name (it widens the census to relabeled Volumes)", env)
	}
}

func TestDeploymentScopeNeverOverridesAnExplicitFlag(t *testing.T) {
	// An operator narrowing the sweep by hand must win over the derivation.
	chdirTempDir(t)
	writeClusterTFVars(t, "lab", "region = \"us-sea\"\n")

	region, volRegion, env := resolveDeploymentScope("lab", "us-ord", "de-fra", "other")
	if region != "us-ord" || volRegion != "de-fra" || env != "other" {
		t.Errorf("explicit flags were overwritten: got (%q,%q,%q)", region, volRegion, env)
	}
}

func TestDeploymentScopeIsANoOpWithoutADeployment(t *testing.T) {
	region, volRegion, env := resolveDeploymentScope("", "us-ord", "us-ord", "lab")
	if region != "us-ord" || volRegion != "us-ord" || env != "lab" {
		t.Errorf("unexpected mutation: got (%q,%q,%q)", region, volRegion, env)
	}
}

func TestDeploymentScopeDegradesOnUnreadableTFVars(t *testing.T) {
	// A missing tfvars must not invent a scope — an unscoped Volume census is how
	// another team's Volumes get counted (and, in reap's case, deleted).
	chdirTempDir(t)
	region, volRegion, env := resolveDeploymentScope("lab", "", "", "")
	if region != "" || volRegion != "" {
		t.Errorf("invented a scope from an unreadable tfvars: (%q,%q)", region, volRegion)
	}
	// env still falls back to the deployment name: that is knowable without the file.
	if env != "lab" {
		t.Errorf("env = %q, want lab", env)
	}
}
