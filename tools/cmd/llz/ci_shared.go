package main

// ci_shared.go — what did NOT go to internal/cigate.
//
// readRegionTFVars knows the repo's terraform directory layout; ciClient is the
// CI PAT reader. Neither is a gate primitive, and a shared package that knew
// either would be a shared package that knows this repo's shape.

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/linode"
	tf "github.com/akamai-consulting/lke-landing-zone/tools/internal/terraform"
)

// readRegionTFVars resolves the tfvars for a region — preferring
// <tfDir>/<region>.tfvars and falling back to the committed .example — parses
// it, and asserts a cluster_label is present. tfDir == "" resolves relative to
// the current working directory (the tf-root the verb already chdir'd into).
//
// Four verbs (tf-import, firewall discovery, teardown, destroy-unwedge) each
// open-coded this same stat-then-fallback + "read %s" wrap + "%s has no
// cluster_label" guard; two of them carried comments saying they mirrored
// runCITFImport, which is how you know it was copied. The guard is on
// ClusterLabel rather than DeriveLabels().Cluster because those are the same
// field (Labels.Cluster is assigned straight from v.ClusterLabel) — callers that
// want labels derive them from the returned vars.
func readRegionTFVars(tfDir, region string) (tf.TFVars, string, error) {
	prefix := ""
	if tfDir != "" {
		prefix = tfDir + "/"
	}
	varFile := prefix + region + ".tfvars"
	if _, err := os.Stat(varFile); err != nil {
		varFile = prefix + region + ".tfvars.example"
	}
	content, err := os.ReadFile(varFile)
	if err != nil {
		return tf.TFVars{}, varFile, fmt.Errorf("read %s: %w", varFile, err)
	}
	vars := tf.ParseTFVars(string(content))
	if vars.ClusterLabel == "" {
		return tf.TFVars{}, varFile, fmt.Errorf("%s has no cluster_label", varFile)
	}
	return vars, varFile, nil
}

// ciClient builds the Linode API client the ci verbs share, with the 60s
// per-request timeout they all used, plus the background context they all then
// created. Four verbs (tf-import, the two reap gates, teardown) open-coded this
// exact ciToken → err-check → NewClient(token, 60*time.Second) →
// context.Background() sequence.
//
// This is deliberately NOT applied to the narrow per-command interfaces
// (teardownClient, firewallDiscoverer, credLister, …) — those are intentional
// test seams, not duplication.
func ciClient() (*linode.Client, context.Context, error) {
	token, err := ciToken()
	if err != nil {
		return nil, nil, err
	}
	return linode.NewClient(token, 60*time.Second), context.Background(), nil
}
