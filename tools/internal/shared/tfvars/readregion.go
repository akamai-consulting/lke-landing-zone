package tfvars

import (
	"fmt"
	"os"

	tf "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/terraform"
)

// readregion.go — locate and parse a region's committed <region>.tfvars.
//
// It joins the reader/writer/existence trio already here: same file format, same
// line-oriented assumptions, and it was the last piece of tfvars handling still
// living in package main.

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
func ReadRegion(tfDir, region string) (tf.TFVars, string, error) {
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
