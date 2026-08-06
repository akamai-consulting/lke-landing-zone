package clusteraccess

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
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/linode"
)

// kubeconfigClient is the slice of the Linode client fetch-kubeconfig needs,
// injected for testing. *linode.Client satisfies it.
type kubeconfigClient interface {
	clusterLister
	GetKubeconfig(ctx context.Context, clusterID uint64) (string, error)
}

var newKubeconfigClient = func(token string) kubeconfigClient { return linode.NewClient(token, 30*time.Second) }

type FetchOpts struct {
	Ref          ClusterRef
	Output       string
	AllowMissing bool
}

func RunFetch(o FetchOpts) error {
	if o.Output == "" {
		return fmt.Errorf("--output is required")
	}
	token := firstNonEmpty(os.Getenv("LINODE_API_TOKEN"), os.Getenv("LINODE_TOKEN"))
	if token == "" {
		return fmt.Errorf("set LINODE_API_TOKEN (or LINODE_TOKEN) to a Linode PAT")
	}
	client := newKubeconfigClient(token)
	ctx := context.Background()

	cid, err := resolveClusterID(ctx, client, o.Ref)
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
		if o.AllowMissing {
			fmt.Fprintf(os.Stderr, "::warning::fetch-kubeconfig: cluster %d has no kubeconfig yet (allow-missing) — available=false.\n", cid)
			setGHAOutput("available", "false")
			return nil
		}
		if derr != nil {
			return fmt.Errorf("cluster %d kubeconfig is not valid base64: %w", cid, derr)
		}
		return fmt.Errorf("cluster %d returned an empty kubeconfig (cluster not provisioned yet, or imported without a refresh)", cid)
	}

	if dir := filepath.Dir(o.Output); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
	}
	if err := os.WriteFile(o.Output, decoded, 0o600); err != nil {
		return fmt.Errorf("writing kubeconfig to %s: %w", o.Output, err)
	}
	fmt.Printf("fetch-kubeconfig: wrote cluster %d kubeconfig to %s (%d bytes).\n", cid, o.Output, len(decoded))
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

// firstNonEmpty and firstLine are local copies of package main's glue — a few
// lines each, with nothing to drift.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 140 {
		s = s[:140] + "…"
	}
	return strings.TrimSpace(s)
}
