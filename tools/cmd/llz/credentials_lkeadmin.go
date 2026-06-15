package main

// credentials_lkeadmin.go implements `llz credentials lke-admin rotate` — the
// former standalone secret-rotation binary, folded into llz the same way the
// PAT/OBJ-key rotators were (one binary in TF_IMAGE instead of three).
//
// Per the Akamai "LKE Secrets Rotation Guidelines", the ONLY sanctioned
// rotation today on LKE-Enterprise is lke-admin-token, performed by calling
// the Linode delete-kubeconfig API (which invalidates and regenerates it). The
// guidelines recommend a Kubernetes-scoped PAT; we deliberately reuse the broad
// shared Terraform LINODE_API_TOKEN instead — see
// docs/runbooks/lke-admin-rotation.md "PAT scope — accepted deviation". Every
// other token is "being worked upon" upstream and must NOT be rotated yet, so
// nothing else is implemented.
//
// Hard guardrails (encoded, not operator discipline):
//   - The cluster MUST be LKE-Enterprise (k8s_version contains "+lke"). On
//     standard LKE this command refuses to run — the standard-LKE batch +
//     regenerate path is intentionally NOT implemented, so it cannot be
//     misapplied to an Enterprise cluster.
//   - The lke-admin-token Secret is NEVER deleted directly (the guidelines
//     state it will not be regenerated if so). Rotation goes exclusively
//     through the delete-kubeconfig API; the Kubernetes API is never touched.
//
// Prints one JSON record on stdout (the rotation audit record, also appended
// to the step summary); logs go to stderr. Dry-run is the default; --apply
// (env ROTATION_APPLY=true) arms it.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/cli"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/linode"
	"github.com/spf13/cobra"
)

// lkeAdminAPI is the slice of the Linode client the rotation uses, seamed for
// tests like patAPI/objKeyAPI.
type lkeAdminAPI interface {
	ClusterK8sVersion(ctx context.Context, clusterID uint64) (string, error)
	DeleteKubeconfig(ctx context.Context, clusterID uint64) error
}

var newLKEAdminClient = func(token string) lkeAdminAPI { return linode.NewClient(token, 30*time.Second) }

func credentialsLKEAdminCmd(o *rotatorOpts) *cobra.Command {
	c := &cobra.Command{
		Use:   "lke-admin",
		Short: "rotate lke-admin-token on LKE-Enterprise via the delete-kubeconfig API",
	}
	var clusterID string
	rotate := &cobra.Command{
		Use:   "rotate",
		Short: "invalidate + regenerate lke-admin-token (refuses non-Enterprise clusters)",
		Long: "The former standalone secret-rotation binary. Rotates lke-admin-token by\n" +
			"calling the Linode delete-kubeconfig API — the only sanctioned LKE-Enterprise\n" +
			"rotation. Hard-refuses a cluster whose k8s_version lacks the \"+lke\" suffix\n" +
			"and never deletes the Secret directly (it would not be regenerated). Prints\n" +
			"one JSON rotation record on stdout. Dry-run unless --apply\n" +
			"(ROTATION_APPLY=true). Reads LINODE_TOKEN, LKE_CLUSTER_ID, REGION.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runCredentialsLKEAdminRotate(o, firstNonEmpty(clusterID, os.Getenv("LKE_CLUSTER_ID")))
		},
	}
	rotate.Flags().StringVar(&clusterID, "lke-cluster-id", "", "numeric LKE cluster id (default: env LKE_CLUSTER_ID)")
	c.AddCommand(rotate)
	return c
}

// isEnterprise reports whether a k8s_version is LKE-Enterprise — those carry a
// "+lke" suffix, e.g. v1.31.9+lke7.
func isEnterprise(k8sVersion string) bool {
	return strings.Contains(k8sVersion, "+lke")
}

func runCredentialsLKEAdminRotate(o *rotatorOpts, clusterIDArg string) error {
	token, apply, err := o.resolve()
	if err != nil {
		return err
	}
	if clusterIDArg == "" {
		return fmt.Errorf("an LKE cluster ID is required (--lke-cluster-id or env LKE_CLUSTER_ID)")
	}
	clusterID := cli.MustUint(clusterIDArg)
	client := newLKEAdminClient(token)
	ctx := context.Background()

	// ── Hard guardrail: must be LKE-Enterprise ──
	k8sVersion, err := client.ClusterK8sVersion(ctx, clusterID)
	if err != nil {
		return err
	}
	if !isEnterprise(k8sVersion) {
		return fmt.Errorf("cluster %d is k8s_version %q — not LKE-Enterprise (no \"+lke\" suffix). "+
			"Only the Enterprise-sanctioned lke-admin rotation is implemented; refusing to run",
			clusterID, k8sVersion)
	}
	slog.Info("confirmed LKE-Enterprise", "cluster_id", clusterID, "k8s_version", k8sVersion)

	// ── Rotate lke-admin-token via delete-kubeconfig ──
	var action string
	if !apply {
		slog.Warn("DRY-RUN: would DELETE kubeconfig (rotates lke-admin-token)")
		action = "DRY-RUN delete-kubeconfig"
	} else {
		if err := client.DeleteKubeconfig(ctx, clusterID); err != nil {
			return err
		}
		slog.Info("rotated lke-admin-token via delete-kubeconfig", "cluster_id", clusterID)
		action = "delete-kubeconfig (lke-admin)"
	}

	record := map[string]any{
		"event":          "lke-admin-rotation",
		"timestamp_unix": time.Now().Unix(),
		"dry_run":        !apply,
		"lke_cluster_id": clusterID,
		"k8s_version":    k8sVersion,
		"tier":           "enterprise",
		"api_action":     action,
	}
	if err := cli.PrintRecord(record); err != nil {
		return err
	}
	// The step summary block the workflow used to tee from stdout.
	recordJSON, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return appendGHAFile("GITHUB_STEP_SUMMARY",
		fmt.Sprintf("### Rotation record — %s", os.Getenv("REGION")),
		"```json",
		string(recordJSON),
		"```")
}
