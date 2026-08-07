package assertobjstore

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/objenc"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cli"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/linode"
	"github.com/spf13/cobra"
)

// cobra_verify_objstore.go — the verify-object-storage flag set and body.
//
// This extension already names verify-object-storage in its declaration; only the
// cobra wiring was still in package main.

func VerifyObjectStorageCmd() *cobra.Command {
	var region string
	c := &cobra.Command{
		Use:   "verify-object-storage",
		Short: "verify a region's Loki/Harbor object-storage BUCKETS exist before the key mint + seeds",
		Long: "Lists Linode object-storage buckets and checks the region's four exist\n" +
			"(<prefix>-loki-{chunks,ruler,admin}-<region>, <prefix>-harbor-registry-\n" +
			"<region>, <prefix> being spec.instance.objLabelPrefix) — i.e.\n" +
			"terraform.yml's apply-object-storage ran. Buckets only:\n" +
			"the scoped KEYS are no longer Terraform-minted — `llz ci\n" +
			"mint-bootstrap-objkeys` mints them AFTER this preflight and the in-cluster\n" +
			"rotator owns them after first boot, so key absence here is normal on a\n" +
			"fresh bootstrap. Non-fatal on a Linode API hiccup (warn + succeed); fails\n" +
			"only when the API responds and a bucket is genuinely absent. Reads\n" +
			"LINODE_API_TOKEN.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIVerifyObjectStorage(region) },
	}
	c.Flags().StringVar(&region, "region", "", "region whose buckets to verify (required)")
	return c
}
func runCIVerifyObjectStorage(region string) error {
	if region == "" {
		return fmt.Errorf("--region is required")
	}
	// From the spec, never a constant: these have to be THIS instance's buckets. A
	// hardcoded prefix would let the gate pass on another adopter's identically
	// named buckets in the same region (clusterspec/objlabels.go).
	prefix, err := objenc.LabelPrefixFor("verify-object-storage")
	if err != nil {
		return err
	}
	token, err := linode.TokenFromEnv()
	if err != nil {
		return err
	}
	buckets, err := linode.NewClient(token, 30*time.Second).ListObjectStorageBuckets(context.Background())
	if err != nil {
		// Transient API hiccup / auth page / non-JSON body — don't block the
		// bootstrap on a parsing edge case; the mint + seed steps below still run.
		fmt.Fprintf(os.Stderr, "::warning::could not verify object-storage buckets for %s (%v) — skipping this preflight (non-fatal).\n", region, err)
		return nil
	}
	have := map[string]bool{}
	for _, b := range buckets {
		have[cli.AsString(b["label"])] = true
	}
	want := []string{
		prefix + "-loki-chunks-" + region,
		prefix + "-loki-ruler-" + region,
		prefix + "-loki-admin-" + region,
		prefix + "-harbor-registry-" + region,
	}
	var missing []string
	for _, label := range want {
		if !have[label] {
			missing = append(missing, label)
		}
	}
	if len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "::error::object-storage bucket(s) not found on Linode for %s: %s\n", region, strings.Join(missing, " "))
		fmt.Fprintln(os.Stderr, "The Loki/Harbor buckets are absent — the key mint below would fail and Loki/Harbor would have no object store. Remediate:")
		fmt.Fprintf(os.Stderr, "  gh workflow run terraform.yml -f action=apply -f module=object-storage -f region=%s\n", region)
		fmt.Fprintf(os.Stderr, "  gh workflow run bootstrap-openbao.yml -f region=%s\n", region)
		return fmt.Errorf("object-storage preflight failed: %d bucket(s) missing for %s", len(missing), region)
	}
	fmt.Printf("object-storage preflight OK for %s: all four buckets exist on Linode.\n", region)
	return nil
}
