// reconcile_apl_overlay.go — the git-to-git config-sync reconciler. It reads the
// apl-overlay from the primary repo (the human-owned `main`), fills the obj
// credential from OpenBao secret/obj/platform, merges the _shared + <env> layers,
// and overlays ONLY the owned files onto the machine-owned apl-<env> branch with a
// non-force fast-forward push (ff-retry). This replaces the config role a
// force-push previously played: two writers (this reconciler + apl-operator) share
// apl-<env> without either clobbering the other.
//
// It runs on the slim distroless image (no git binary, no shell), so every git
// operation is the GitHub REST git-data API (gh_gitdata_native.go), never
// exec("git") — consistent with the reconciler's no-client-go stance. It is a
// DRIVING reconciler (it writes), so it is leader-gated. See
// docs/designs/apl-overlay-obj-native.md.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/metrics"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/openbao"
)

// objPlatformPath is the consolidated platform object-storage credential (one key
// spanning all buckets — apl-core's native one-cred model). The linode-creds
// reconciler rotates it; this pass propagates it into apl-core's values. Fields
// mirror the harbor-registry-s3 field naming.
const objPlatformPath = "secret/obj/platform"

const (
	objCredAccessField = "access_key_id"
	objCredSecretField = "secret_access_key"
)

// aplOverlayTargets maps each rendered overlay file to the path in the apl-<env>
// values tree apl-core reads as config input.
//
// LAB-GATED: the exact env-tree paths — and whether a target needs a key-level
// (not file-level) merge because apl-operator co-writes the same file — are
// apl-core-internal and provable only on a live cluster (the same disposition
// every apl-core-facing assumption in this repo carries). Isolated here so a lab
// finding is a one-line correction. See the design doc's owned-file mapping.
var aplOverlayTargets = map[string]string{
	clusterspec.OverlayObjFile:  "env/settings/obj.yaml",
	clusterspec.OverlayAppsFile: "env/settings/apps.yaml",
}

// aplOverlayCommitMessage is the commit subject for the reconciler's overlay
// pushes onto apl-<env> — distinct from apl-operator's "otomi commit" so the two
// writers' history is legible.
const aplOverlayCommitMessage = "chore(llz): sync apl-overlay (obj storage + app toggles) [ci skip]"

// aplOverlayMaxAttempts bounds the fast-forward retry loop against apl-operator's
// concurrent reconcile pushes.
const aplOverlayMaxAttempts = 4

// aplOverlayConfig is the per-env wiring the reconciler reads from its env.
type aplOverlayConfig struct {
	repo         string // GH_REPO — owner/name of the primary (values) repo
	token        string // APL_VALUES_REPO_TOKEN — Contents:write fine-grained PAT
	env          string // REGION — the deployment/env name (overlay path + branch)
	sourceBranch string // where the overlay source lives (human-owned; default main)
	targetBranch string // the machine-owned branch to overlay onto (default apl-<env>)
	openbaoAddr  string
}

// Seams for tests — the git-data primitives, the overlay-commit orchestration,
// and the OpenBao credential read are swapped for fakes so the reconciler's
// composition is unit-tested without a network.
var (
	aplOverlayReadFileFn = ghReadFileNative
	aplOverlayCommitFn   = ghOverlayCommitNative
	aplOverlayObjCredsFn = readObjPlatformCreds
)

// openbaoGetClientFn builds an OpenBao client that can READ secret data (Get),
// distinct from reconcile_openbao.go's metadata-only probe. A seam for tests.
var openbaoGetClientFn = func(addr, token string) interface {
	Get(ctx context.Context, path, key string) (string, bool, error)
} {
	return openbao.NewWithClient(addr, token, "", openbao.HTTPClientInsecure(30*time.Second))
}

// aplOverlayConfigFromEnv reads + validates the reconciler's env contract. REGION
// is the env name (same value the linode-creds reconciler reads); the branches
// default to the render-time conventions (apl-<env> machine branch, main source).
func aplOverlayConfigFromEnv() (aplOverlayConfig, error) {
	c := aplOverlayConfig{
		repo:         os.Getenv("GH_REPO"),
		token:        os.Getenv("APL_VALUES_REPO_TOKEN"),
		env:          os.Getenv("REGION"),
		sourceBranch: orEnvDefault("APL_VALUES_SOURCE_BRANCH", "main"),
		targetBranch: os.Getenv("APL_VALUES_BRANCH"),
		openbaoAddr:  orEnvDefault("OPENBAO_ADDR", defaultOpenBaoAddr),
	}
	if c.repo == "" || c.token == "" || c.env == "" {
		return aplOverlayConfig{}, fmt.Errorf(
			"apl-overlay reconciler needs GH_REPO, APL_VALUES_REPO_TOKEN, and REGION (env) — got repo=%q env=%q token-set=%v",
			c.repo, c.env, c.token != "")
	}
	if c.targetBranch == "" {
		c.targetBranch = "apl-" + c.env
	}
	return c, nil
}

func orEnvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// reconcileAplOverlay runs one sync pass: read → fill → merge → overlay. It is
// leader-gated by the caller (it writes). Returns nil (a no-op) — never an error —
// for the two expected transient states: the apl-<env> branch not yet created by
// apl-operator, and the obj credential not yet seeded in OpenBao. A genuine
// API/merge failure IS returned (the manager records up=0).
func reconcileAplOverlay(ctx context.Context, reg *metrics.Registry) error {
	cfg, err := aplOverlayConfigFromEnv()
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 30 * time.Second}

	files := map[string]string{}
	// obj.yaml carries the secret, so process it with the credential fill; apps.yaml
	// is secret-free and always syncs.
	for _, base := range []string{clusterspec.OverlayAppsFile, clusterspec.OverlayObjFile} {
		shared, _, err := aplOverlayReadFileFn(ctx, client, cfg.token, cfg.repo, cfg.sourceBranch, sharedOverlayPath(base))
		if err != nil {
			return fmt.Errorf("read _shared overlay %s: %w", base, err)
		}
		envLayer, _, err := aplOverlayReadFileFn(ctx, client, cfg.token, cfg.repo, cfg.sourceBranch, envOverlayPath(cfg.env, base))
		if err != nil {
			return fmt.Errorf("read %s overlay %s: %w", cfg.env, base, err)
		}
		merged, err := clusterspec.MergeOverlay([]byte(shared), []byte(envLayer))
		if err != nil {
			return fmt.Errorf("merge overlay %s: %w", base, err)
		}
		if len(bytes.TrimSpace(merged)) == 0 {
			continue // nothing authored for this file
		}
		if base == clusterspec.OverlayObjFile {
			ak, sk, ok, err := aplOverlayObjCredsFn(ctx, cfg.openbaoAddr)
			if err != nil {
				return fmt.Errorf("read %s from OpenBao: %w", objPlatformPath, err)
			}
			if !ok {
				// Credential not seeded yet — NEVER overlay the literal placeholder
				// onto the machine branch (it would push a broken obj cred to
				// apl-core). Skip obj.yaml this pass; apps.yaml still syncs.
				fmt.Printf("apl-overlay: %s not seeded in OpenBao yet — skipping obj.yaml this pass\n", objPlatformPath)
				continue
			}
			merged = clusterspec.FillObjPlaceholders(merged, ak, sk)
		}
		files[aplOverlayTargets[base]] = string(merged)
	}
	if len(files) == 0 {
		return nil // nothing to sync
	}

	sha, changed, err := aplOverlayCommitFn(ctx, client, cfg.token, cfg.repo, cfg.targetBranch, files, aplOverlayCommitMessage, aplOverlayMaxAttempts)
	if errors.Is(err, errGHRefNotFound) {
		// apl-operator has not created/bootstrapped apl-<env> yet — there is no tree
		// to overlay onto. Expected during first boot; not an error.
		fmt.Printf("apl-overlay: target branch %q absent (apl-operator not bootstrapped yet) — will retry next pass\n", cfg.targetBranch)
		return nil
	}
	if err != nil {
		return err
	}
	reg.SetGauge("llz_apl_overlay_synced",
		"1 when the apl-overlay reconciler's last git sync succeeded",
		map[string]string{"branch": cfg.targetBranch}, 1)
	if changed {
		fmt.Printf("apl-overlay: synced %d file(s) onto %s at %s\n", len(files), cfg.targetBranch, sha)
	}
	return nil
}

// readObjPlatformCreds reads the consolidated obj credential (access + secret key)
// from OpenBao secret/obj/platform via the reconciler's Kubernetes-auth role.
// Returns ok=false (not an error) when the path/fields are not seeded yet.
func readObjPlatformCreds(ctx context.Context, addr string) (accessKeyID, secretAccessKey string, ok bool, err error) {
	jwt, err := openbaoJWTFn()
	if err != nil {
		return "", "", false, err
	}
	tok, err := openbaoLoginFn(ctx, addr, jwt)
	if err != nil {
		return "", "", false, err
	}
	c := openbaoGetClientFn(addr, tok)
	ak, ok, err := c.Get(ctx, objPlatformPath, objCredAccessField)
	if err != nil || !ok {
		return "", "", false, err
	}
	sk, ok, err := c.Get(ctx, objPlatformPath, objCredSecretField)
	if err != nil || !ok {
		return "", "", false, err
	}
	if ak == "" || sk == "" {
		return "", "", false, nil
	}
	return ak, sk, true, nil
}

func sharedOverlayPath(base string) string { return "apl-values/_shared/apl-overlay/" + base }
func envOverlayPath(env, base string) string {
	return "apl-values/" + env + "/apl-overlay/" + base
}
