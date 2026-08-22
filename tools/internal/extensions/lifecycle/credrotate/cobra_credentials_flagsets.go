package credrotate

// cobra_credentials_flagsets.go — the `llz credentials pat` and `llz credentials
// lke-admin` flag sets.
//
// The rotators are tools/internal/extensions/lifecycle/credrotate, which also owns the shared
// framework they run on. The wiring travels with the capability rather than
// sitting in main.

import (
	"context"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cli"
)

func CredentialsPATCmd(o *Opts) *cobra.Command {
	c := &cobra.Command{
		Use:   "pat",
		Short: "rotate the shared Linode PAT (90-day policy): create + revoke-old",
	}
	c.AddCommand(CredentialsPATCreateCmd(o), CredentialsPATRevokeOldCmd(o))
	return c
}

func CredentialsLKEAdminCmd(o *Opts) *cobra.Command {
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
			return RunLKEAdminRotate(o, firstNonEmpty(clusterID, os.Getenv("LKE_CLUSTER_ID")))
		},
	}
	rotate.Flags().StringVar(&clusterID, "lke-cluster-id", "", "numeric LKE cluster id (default: env LKE_CLUSTER_ID)")
	c.AddCommand(rotate)
	return c
}

// isEnterprise reports whether a k8s_version is LKE-Enterprise — those carry a
// "+lke" suffix, e.g. v1.31.9+lke7.

func CredentialsPATCreateCmd(o *Opts) *cobra.Command {
	var label, scopes, ghaSecretName, ghaDeployments string
	var validityDays int64
	c := &cobra.Command{
		Use:   "create",
		Short: "mint a new PAT with the configured label/scopes/validity (JSON record on stdout)",
		Long: "Issues a new Linode PAT with the configured label, scopes, and validity,\n" +
			"printing the new id + token as one JSON record on stdout for the calling\n" +
			"composite action to propagate (GHA secret + each region's OpenBao). With\n" +
			"--gha-secret-name it ALSO writes the new token into that GitHub secret for\n" +
			"every infra-<deployment> environment (--gha-deployments) — the env-scoped\n" +
			"copies the workflows actually read. Refuses validity-days > 90. Dry-run unless --apply.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			token, apply, err := o.Resolve()
			if err != nil {
				return err
			}
			// RESOLVED HERE, NOT DEFAULTED IN THE FLAG. See the note on
			// revoke-old below: the delivered workflow stopped passing `label:`
			// on the strength of this call, and the call did not exist.
			label, err := resolveRotationLabel(label, rotationKindPAT, "`llz credentials pat create`")
			if err != nil {
				return err
			}
			return RunPATCreate(context.Background(), NewPATClient(token), apply, label, scopes, validityDays, ghaSecretName, strings.Fields(ghaDeployments))
		},
	}
	f := c.Flags()
	f.StringVar(&label, "label", os.Getenv("PAT_LABEL"), "label for the new PAT — also the revoke-old drain target (env PAT_LABEL)")
	f.StringVar(&scopes, "scopes", os.Getenv("PAT_SCOPES"), "Linode-API scopes string for the new PAT (env PAT_SCOPES)")
	f.Int64Var(&validityDays, "validity-days", cli.EnvInt("PAT_VALIDITY_DAYS", 90), "validity window in days; the 90-day policy caps this at 90 (env PAT_VALIDITY_DAYS)")
	f.StringVar(&ghaSecretName, "gha-secret-name", os.Getenv("GHA_SECRET_NAME"), "GitHub secret to update with the new token, in every infra-<deployment> env (env GHA_SECRET_NAME; empty = don't write)")
	f.StringVar(&ghaDeployments, "gha-deployments", os.Getenv("GHA_SECRET_DEPLOYMENTS"), "space-separated deployment names whose infra-<name> env gets the new secret (env GHA_SECRET_DEPLOYMENTS; empty = repo-level)")
	return c
}

func CredentialsPATRevokeOldCmd(o *Opts) *cobra.Command {
	var label string
	var graceDays int64
	c := &cobra.Command{
		Use:   "revoke-old",
		Short: "daily reaper: keep the newest same-labeled PAT, revoke older siblings past the grace window",
		Long: "Lists every PAT matching the label, keeps the newest (the live one), and\n" +
			"revokes any older sibling whose `created` time is past the grace window.\n" +
			"Stateless: the label IS the record of which PAT is current. Dry-run unless\n" +
			"--apply.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			token, apply, err := o.Resolve()
			if err != nil {
				return err
			}
			// ─────────────────────────────────────────────────────────────────
			// THE RESOLVER EXISTED, WITH TESTS, AND NOTHING CALLED IT. All four
			// {pat,obj-key} {create,revoke-old} verbs took `--label` straight
			// from the flag, whose default is $PAT_LABEL / $OBJ_LABEL — and
			// llz-secret-rotation.yml had already been changed to STOP passing a
			// label, on comments reading "omitted on purpose — llz derives the
			// instance-scoped label". Neither side set it, so every verb ran with
			// label "".
			//
			// For create that means PATs minted unlabeled. For this verb it means
			// the daily reaper matches nothing — a permanent no-op, so superseded
			// PATs accumulate against the 100-token account cap while the job
			// reports success. Neither shows up as a failure anywhere.
			//
			// It is resolved in RunE rather than in the flag default because the
			// derivation READS THE SPEC (clusterspec.LabelPrefixFor), which is a
			// filesystem access that must not happen while the command tree is
			// being built — and because a flag default evaluated at construction
			// time is the same trap converge/deps.go documents for --dry-run.
			// ─────────────────────────────────────────────────────────────────
			label, err := resolveRotationLabel(label, rotationKindPAT, "`llz credentials pat revoke-old`")
			if err != nil {
				return err
			}
			return RunPATRevokeOld(context.Background(), NewPATClient(token), apply, label, graceDays)
		},
	}
	f := c.Flags()
	f.StringVar(&label, "label", os.Getenv("PAT_LABEL"), "label to drain — same label `pat create` uses (env PAT_LABEL)")
	f.Int64Var(&graceDays, "grace-days", cli.EnvInt("PAT_GRACE_DAYS", 7), "only revoke same-labeled siblings older than this many days (env PAT_GRACE_DAYS)")
	return c
}

func CredentialsObjKeyCmd(o *Opts) *cobra.Command {
	c := &cobra.Command{
		Use:   "obj-key",
		Short: "rotate the TF-state Object Storage key pair (120-day SLA): create + revoke-old",
	}
	c.AddCommand(CredentialsObjKeyCreateCmd(o), CredentialsObjKeyRevokeOldCmd(o))
	return c
}

func CredentialsObjKeyCreateCmd(o *Opts) *cobra.Command {
	var label, cluster, bucket, permissions, ghaAccessName, ghaSecretName, ghaDeployments string
	c := &cobra.Command{
		Use:   "create",
		Short: "mint a new bucket-scoped OBJ key pair (JSON record on stdout)",
		Long: "Issues a new bucket-scoped Linode Object Storage key, printing the new id +\n" +
			"access_key + secret_key as one JSON record on stdout. With --gha-*-secret-name it\n" +
			"ALSO writes both halves into those GitHub secrets for every infra-<deployment>\n" +
			"environment (--gha-deployments) — the env-scoped copies the workflows read. The\n" +
			"secret half is shown exactly once by the Linode API. Dry-run unless --apply.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			token, apply, err := o.Resolve()
			if err != nil {
				return err
			}
			label, err := resolveRotationLabel(label, rotationKindTFStateKey, "`llz credentials obj-key create`")
			if err != nil {
				return err
			}
			// THE OTHER HALF, AND THE ONE THAT DESTROYS SOMETHING. --bucket-cluster
			// was likewise never resolved, so this ran with cluster "". The monthly
			// cron APPLIES (routeRotation sets TFStateApply), and create writes the
			// new pair over TF_STATE_ACCESS_KEY / TF_STATE_SECRET_KEY in every
			// infra-<deployment> environment. A bucket is reachable ONLY at the
			// endpoint it was created against — the disjoint-generation rule behind
			// the Loki/Harbor NoSuchBucket class — so the key that lands cannot read
			// the state bucket. It fires ~30 days after a successful first build,
			// with no operator action involved.
			//
			// The resolver refuses rather than guessing, which is why its error is
			// returned rather than logged: minting against the wrong cluster is the
			// failure, and doing it confidently is worse than not doing it.
			cluster, err := resolveObjBucketCluster(cluster, os.Getenv("TF_STATE_ENDPOINT"))
			if err != nil {
				return err
			}
			return RunObjKeyCreate(context.Background(), NewObjKeyClient(token), apply, label, cluster, bucket, permissions, ghaAccessName, ghaSecretName, strings.Fields(ghaDeployments))
		},
	}
	defaultPermissions := os.Getenv("OBJ_BUCKET_PERMISSIONS")
	if defaultPermissions == "" {
		defaultPermissions = "read_write"
	}
	f := c.Flags()
	f.StringVar(&label, "label", os.Getenv("OBJ_LABEL"), "label for the new OBJ key — also the revoke-old drain target (env OBJ_LABEL)")
	f.StringVar(&cluster, "bucket-cluster", os.Getenv("OBJ_BUCKET_CLUSTER"), "Linode object-storage cluster id, e.g. us-ord-10 (env OBJ_BUCKET_CLUSTER)")
	f.StringVar(&bucket, "bucket-name", os.Getenv("OBJ_BUCKET_NAME"), "bucket name to scope the key to (env OBJ_BUCKET_NAME)")
	f.StringVar(&permissions, "bucket-permissions", defaultPermissions, "read_only, read_write, or none (env OBJ_BUCKET_PERMISSIONS)")
	f.StringVar(&ghaAccessName, "gha-access-key-secret-name", os.Getenv("GHA_ACCESS_KEY_SECRET_NAME"), "GitHub secret for the access-key half, written per infra-<deployment> env (env GHA_ACCESS_KEY_SECRET_NAME)")
	f.StringVar(&ghaSecretName, "gha-secret-key-secret-name", os.Getenv("GHA_SECRET_KEY_SECRET_NAME"), "GitHub secret for the secret-key half, written per infra-<deployment> env (env GHA_SECRET_KEY_SECRET_NAME)")
	f.StringVar(&ghaDeployments, "gha-deployments", os.Getenv("GHA_SECRET_DEPLOYMENTS"), "space-separated deployment names whose infra-<name> env gets the new secrets (env GHA_SECRET_DEPLOYMENTS; empty = repo-level)")
	return c
}

func CredentialsObjKeyRevokeOldCmd(o *Opts) *cobra.Command {
	var label string
	var keepNewest int64
	c := &cobra.Command{
		Use:   "revoke-old",
		Short: "daily reaper: keep the N newest same-labeled OBJ keys by id, revoke the rest",
		Long: "Lists every OBJ key matching the label, keeps the N most recent by id (Linode\n" +
			"IDs are monotonically increasing per account, so highest id == newest), and\n" +
			"revokes the rest. keep-newest=2 gives ~30-day overlap with monthly rotation.\n" +
			"Dry-run unless --apply.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			token, apply, err := o.Resolve()
			if err != nil {
				return err
			}
			label, err := resolveRotationLabel(label, rotationKindTFStateKey, "`llz credentials obj-key revoke-old`")
			if err != nil {
				return err
			}
			return RunObjKeyRevokeOld(context.Background(), NewObjKeyClient(token), apply, label, keepNewest)
		},
	}
	f := c.Flags()
	f.StringVar(&label, "label", os.Getenv("OBJ_LABEL"), "label to drain — same label `obj-key create` uses (env OBJ_LABEL)")
	f.Int64Var(&keepNewest, "keep-newest", cli.EnvInt("OBJ_KEEP_NEWEST", 2), "how many most-recent same-labeled keys to keep (env OBJ_KEEP_NEWEST)")
	return c
}
