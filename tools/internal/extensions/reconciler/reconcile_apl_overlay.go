// reconcile_apl_overlay.go — cmd/llz wiring for the apl-overlay reconciler. The
// sync ORCHESTRATION (read → fill → merge → overlay onto apl-<env>) lives in
// internal/apl/overlay (ADR 0013 Phase 1, the values front door); this file is the
// transport + config that feeds it: it reads the reconciler's env contract, gates
// on the ESO-synced repo token, adapts the GitHub git-data REST API
// (gh_gitdata_native.go) to overlay.Repo, and reads the obj credential from
// OpenBao. It runs on the slim distroless image (no git binary), so every git
// operation is the git-data API, never exec("git"). Leader-gated by the caller.
package reconciler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/apl/overlay"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cigate"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghgitdata"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/metrics"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/openbao"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/reconcilelanes"
)

// objPlatformPath is the consolidated platform object-storage credential (one key
// spanning all buckets — apl-core's native one-cred model). The linode-creds
// reconciler rotates it; this pass propagates the access-key ID into apl-core's
// values. objCredAccessField is the only field read here — a non-secret identifier
// the overlay carries in git; the secret_access_key never transits this path (ESO
// writes it into obj-secrets directly).
const objPlatformPath = "secret/obj/platform"
const objCredAccessField = "AWS_ACCESS_KEY_ID"

// aplOverlayConfig is the per-env wiring the reconciler reads from its env.
type aplOverlayConfig struct {
	repo         string // GH_REPO — owner/name of the primary (values) repo
	token        string // APL_VALUES_REPO_TOKEN — Contents:write fine-grained PAT
	env          string // REGION — the deployment/env name (overlay path + branch)
	sourceBranch string // where the overlay source lives (human-owned; default main)
	targetBranch string // the machine-owned branch to overlay onto (default apl-<env>)
	openbaoAddr  string
}

// openbaoGetClientFn builds an OpenBao client that can READ secret data (Get),
// distinct from reconcile_openbao.go's metadata-only probe. A seam for tests.
//
// Pod-network traffic, so it uses the shared mTLS transport. Returns the
// transport error rather than a client that fails every call — the same shape
// reconcile_openbao.go's reconcilelanes.OpenBaoClientFn uses, so a missing client certificate
// surfaces once at construction instead of once per Get.
var openbaoGetClientFn = func(addr, token string) (interface {
	Get(ctx context.Context, path, key string) (string, bool, error)
}, error) {
	hc, err := openbao.InClusterHTTPClient()
	if err != nil {
		return nil, err
	}
	return openbao.NewWithClient(addr, token, "", hc), nil
}

// aplOverlayConfigFromEnv reads + validates the reconciler's env contract. REGION
// is the env name (same value the linode-creds reconciler reads); the branches
// default to the render-time conventions (apl-<env> machine branch, main source).
func aplOverlayConfigFromEnv() (aplOverlayConfig, error) {
	c := aplOverlayConfig{
		repo: os.Getenv("GH_REPO"),
		// Lazy, env-first then optional Secret volume — NOT a hard env ref (the pod
		// starts at wave 0, before the OpenBao store serves the token). Empty is a
		// transient not-yet-synced state the caller no-ops on, not a misconfig.
		token:        ghgitdata.InClusterAplValuesToken(),
		env:          os.Getenv("REGION"),
		sourceBranch: cigate.EnvOr("APL_VALUES_SOURCE_BRANCH", "main"),
		targetBranch: os.Getenv("APL_VALUES_BRANCH"),
		openbaoAddr:  cigate.EnvOr("OPENBAO_ADDR", reconcilelanes.DefaultOpenBaoAddr),
	}
	// GH_REPO/REGION are render-time static — their absence is a genuine misconfig.
	// The token is ESO-synced and handled as a no-op below, not here.
	if c.repo == "" || c.env == "" {
		return aplOverlayConfig{}, fmt.Errorf(
			"apl-overlay reconciler needs GH_REPO and REGION (env) — got repo=%q env=%q",
			c.repo, c.env)
	}
	if c.targetBranch == "" {
		c.targetBranch = "apl-" + c.env
	}
	return c, nil
}

// reconcileAplOverlay wires the env contract + transport and runs one
// overlay.Reconcile pass. Returns nil (a no-op) for the ESO-synced token not being
// present yet (the pod starts at wave 0, before the OpenBao store serves); genuine
// transient states inside the sync (missing branch / unseeded cred) are no-ops
// handled by overlay.Reconcile itself.
func reconcileAplOverlay(ctx context.Context, reg *metrics.Registry) error {
	cfg, err := aplOverlayConfigFromEnv()
	if err != nil {
		return err
	}
	if cfg.token == "" {
		fmt.Println("apl-overlay: APL_VALUES_REPO_TOKEN not synced yet — skipping this pass")
		return nil
	}
	client := &http.Client{Timeout: 30 * time.Second}
	repo := ghOverlayRepo{client: client, token: cfg.token, repo: cfg.repo}
	objCreds := func(ctx context.Context) (string, bool, error) {
		return readObjPlatformCreds(ctx, cfg.openbaoAddr)
	}
	return overlay.Reconcile(ctx, overlay.Config{
		Env:          cfg.env,
		SourceBranch: cfg.sourceBranch,
		TargetBranch: cfg.targetBranch,
	}, repo, objCreds, reg)
}

// ghOverlayRepo adapts the GitHub git-data REST API (gh_gitdata_native.go) to
// overlay.Repo, mapping the branch-ref 404 onto overlay.ErrRefNotFound so the
// orchestration can treat a not-yet-bootstrapped apl-<env> as an expected no-op.
type ghOverlayRepo struct {
	client      *http.Client
	token, repo string
}

func (r ghOverlayRepo) ReadFile(ctx context.Context, branch, path string) (string, bool, error) {
	return ghgitdata.ReadFile(ctx, r.client, r.token, r.repo, branch, path)
}

func (r ghOverlayRepo) OverlayCommit(ctx context.Context, branch string, files map[string]string, message string, maxAttempts int) (string, bool, error) {
	sha, changed, err := ghgitdata.OverlayCommit(ctx, r.client, r.token, r.repo, branch, files, message, maxAttempts)
	if errors.Is(err, ghgitdata.ErrRefNotFound) {
		return sha, changed, overlay.ErrRefNotFound
	}
	return sha, changed, err
}

// readObjPlatformCreds reads the obj access-key ID (a non-secret identifier) from
// OpenBao secret/obj/platform via the reconciler's Kubernetes-auth role. Returns
// ok=false (not an error) when the path/field is not seeded yet. The paired
// secret_access_key is NOT read here — it never transits the git overlay; ESO
// delivers it into the obj-secrets Secret from the same OpenBao path.
func readObjPlatformCreds(ctx context.Context, addr string) (accessKeyID string, ok bool, err error) {
	jwt, err := reconcilelanes.OpenBaoJWTFn()
	if err != nil {
		return "", false, err
	}
	tok, err := reconcilelanes.OpenBaoLoginFn(ctx, addr, jwt)
	if err != nil {
		return "", false, err
	}
	c, err := openbaoGetClientFn(addr, tok)
	if err != nil {
		return "", false, err
	}
	ak, ok, err := c.Get(ctx, objPlatformPath, objCredAccessField)
	if err != nil || !ok {
		return "", false, err
	}
	if ak == "" {
		return "", false, nil
	}
	return ak, true, nil
}
