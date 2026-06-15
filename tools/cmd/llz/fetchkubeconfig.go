package main

// fetchkubeconfig.go implements `llz ci fetch-kubeconfig` — fetch an LKE cluster's
// admin kubeconfig straight from the Linode API and write it to a file.
//
// This is the API-sourced alternative to reading kubeconfig_raw out of the
// cluster Terraform state: no `terraform init`, no S3 backend creds, no git auth
// to clone module sources — and it sidesteps the "terraform output -raw returned
// empty" failure class entirely. The kubeconfig the API returns is the same
// Linode-issued admin kubeconfig the cluster module exposes as kubeconfig_raw.

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/linode"
	"github.com/spf13/cobra"
)

// kubeconfigClient is the slice of the Linode client fetch-kubeconfig needs,
// injected for testing. *linode.Client satisfies it.
type kubeconfigClient interface {
	clusterLister
	GetKubeconfig(ctx context.Context, clusterID uint64) (string, error)
}

var newKubeconfigClient = func(token string) kubeconfigClient { return linode.NewClient(token, 30*time.Second) }

type fetchKubeconfigOpts struct {
	ref          clusterRef
	output       string
	allowMissing bool
}

func ciFetchKubeconfigCmd() *cobra.Command {
	var o fetchKubeconfigOpts
	c := &cobra.Command{
		Use:   "fetch-kubeconfig",
		Short: "write an LKE cluster's kubeconfig (from the Linode API) to a file",
		Long: "Fetch the cluster's admin kubeconfig directly from the Linode API and write\n" +
			"it to --output (mode 0600). The API-sourced alternative to reading\n" +
			"kubeconfig_raw out of Terraform state — no terraform init / S3 backend / git\n" +
			"auth, and no empty-output failure class. The cluster is resolved from\n" +
			"--cluster-id, else --cluster-label (+ --linode-region), else cluster_label /\n" +
			"region in <tfvars-dir>/<region>.tfvars. Reads LINODE_API_TOKEN (or\n" +
			"LINODE_TOKEN). With --allow-missing, an absent/not-ready kubeconfig sets\n" +
			"available=false on GITHUB_OUTPUT instead of failing.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIFetchKubeconfig(o) },
	}
	f := c.Flags()
	f.StringVar(&o.output, "output", "", "absolute path to write the kubeconfig to (required)")
	f.StringVar(&o.ref.region, "region", "", "deployment/env key (finds <region>.tfvars)")
	f.StringVar(&o.ref.clusterID, "cluster-id", "", "explicit LKE cluster numeric ID (skips label resolution)")
	f.StringVar(&o.ref.clusterLabel, "cluster-label", "", "LKE cluster label to resolve by")
	f.StringVar(&o.ref.linodeRegion, "linode-region", "", "Linode datacenter region (e.g. us-ord) to disambiguate")
	f.StringVar(&o.ref.tfvarsDir, "tfvars-dir", "terraform-iac-bootstrap/cluster", "dir holding <region>.tfvars")
	f.BoolVar(&o.allowMissing, "allow-missing", false, "set available=false instead of failing when the kubeconfig is absent")
	return c
}

func runCIFetchKubeconfig(o fetchKubeconfigOpts) error {
	if o.output == "" {
		return fmt.Errorf("--output is required")
	}
	token := firstNonEmpty(os.Getenv("LINODE_API_TOKEN"), os.Getenv("LINODE_TOKEN"))
	if token == "" {
		return fmt.Errorf("set LINODE_API_TOKEN (or LINODE_TOKEN) to a Linode PAT")
	}
	client := newKubeconfigClient(token)
	ctx := context.Background()

	cid, err := resolveClusterID(ctx, client, o.ref)
	if err != nil {
		return err
	}

	// The API returns the kubeconfig base64-encoded; a not-yet-ready cluster
	// yields an empty string (GetKubeconfig maps a non-2xx to "").
	encoded, err := client.GetKubeconfig(ctx, cid)
	if err != nil {
		return err
	}
	decoded, derr := base64.StdEncoding.DecodeString(encoded)
	if encoded == "" || derr != nil || len(decoded) == 0 {
		if o.allowMissing {
			fmt.Fprintf(os.Stderr, "::warning::fetch-kubeconfig: cluster %d has no kubeconfig yet (allow-missing) — available=false.\n", cid)
			setGHAOutput("available", "false")
			return nil
		}
		if derr != nil {
			return fmt.Errorf("cluster %d kubeconfig is not valid base64: %w", cid, derr)
		}
		return fmt.Errorf("cluster %d returned an empty kubeconfig (cluster not provisioned yet, or imported without a refresh)", cid)
	}

	if dir := filepath.Dir(o.output); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
	}
	if err := os.WriteFile(o.output, decoded, 0o600); err != nil {
		return fmt.Errorf("writing kubeconfig to %s: %w", o.output, err)
	}
	fmt.Printf("fetch-kubeconfig: wrote cluster %d kubeconfig to %s (%d bytes).\n", cid, o.output, len(decoded))
	setGHAOutput("available", "true")
	return nil
}

// setGHAOutput appends key=value to $GITHUB_OUTPUT when set (a no-op otherwise),
// so the composite action can expose `available` without any jq glue.
func setGHAOutput(key, value string) {
	path := os.Getenv("GITHUB_OUTPUT")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s=%s\n", key, value)
}
