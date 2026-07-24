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
	"sort"
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

// Only the access-key ID is read here — it is a non-secret identifier the overlay
// carries in git (apl-core treats it as a settings field). The secret_access_key
// deliberately never transits this path: ESO writes it into obj-secrets directly.
const objCredAccessField = "AWS_ACCESS_KEY_ID"

// aplOverlayTargets maps each rendered overlay file to the path in the apl-<env>
// values tree apl-core reads as config input.
//
// LAB-GATED: the exact env-tree paths — and whether a target needs a key-level
// (not file-level) merge because apl-operator co-writes the same file — are
// apl-core-internal and provable only on a live cluster (the same disposition
// every apl-core-facing assumption in this repo carries). Isolated here so a lab
// finding is a one-line correction. See the design doc's owned-file mapping.
// Only obj.yaml maps to a single fixed file (the AplObjectStorage settings CR LLZ
// owns outright). App toggles are NOT a single env/settings/apps.yaml — apl-core
// keeps per-app AplApp CRs at env/apps/<name>.yaml, so the reconciler fans the
// desired toggles out there (aplAppTarget) with a key-level merge instead.
var aplOverlayTargets = map[string]string{
	clusterspec.OverlayObjFile: "env/settings/obj.yaml",
}

// aplAppTarget is apl-core's per-app AplApp CR path on the machine branch.
func aplAppTarget(app string) string { return "env/apps/" + app + ".yaml" }

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
		repo: os.Getenv("GH_REPO"),
		// Lazy, env-first then optional Secret volume — NOT a hard env ref (the pod
		// starts at wave 0, before the OpenBao store serves the token). Empty is a
		// transient not-yet-synced state the caller no-ops on, not a misconfig.
		token:        inclusterAplValuesRepoToken(),
		env:          os.Getenv("REGION"),
		sourceBranch: orEnvDefault("APL_VALUES_SOURCE_BRANCH", "main"),
		targetBranch: os.Getenv("APL_VALUES_BRANCH"),
		openbaoAddr:  orEnvDefault("OPENBAO_ADDR", defaultOpenBaoAddr),
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
	if cfg.token == "" {
		// ESO hasn't synced the apl-values-repo-token yet (the reconciler starts at
		// wave 0, before the OpenBao store serves — same secrets-before-apps posture
		// as the linode token). No-op this pass; kubelet surfaces the mounted token
		// file within ~1m, and the next pass proceeds.
		fmt.Println("apl-overlay: APL_VALUES_REPO_TOKEN not synced yet — skipping this pass")
		return nil
	}
	client := &http.Client{Timeout: 30 * time.Second}

	files := map[string]string{}

	// obj.yaml — LLZ owns the whole AplObjectStorage settings CR: merge the _shared +
	// per-env source, fill accessKeyId from OpenBao, and write env/settings/obj.yaml.
	objShared, _, err := aplOverlayReadFileFn(ctx, client, cfg.token, cfg.repo, cfg.sourceBranch, sharedOverlayPath(clusterspec.OverlayObjFile))
	if err != nil {
		return fmt.Errorf("read _shared obj overlay: %w", err)
	}
	objEnv, _, err := aplOverlayReadFileFn(ctx, client, cfg.token, cfg.repo, cfg.sourceBranch, envOverlayPath(cfg.env, clusterspec.OverlayObjFile))
	if err != nil {
		return fmt.Errorf("read %s obj overlay: %w", cfg.env, err)
	}
	objMerged, err := clusterspec.MergeOverlay([]byte(objShared), []byte(objEnv))
	if err != nil {
		return fmt.Errorf("merge obj overlay: %w", err)
	}
	if len(bytes.TrimSpace(objMerged)) > 0 {
		ak, ok, err := aplOverlayObjCredsFn(ctx, cfg.openbaoAddr)
		if err != nil {
			return fmt.Errorf("read %s from OpenBao: %w", objPlatformPath, err)
		}
		if !ok {
			// accessKeyId not seeded yet — NEVER push the literal placeholder onto the
			// machine branch (a broken obj cred). Skip obj this pass; apps still sync.
			fmt.Printf("apl-overlay: %s not seeded in OpenBao yet — skipping obj.yaml this pass\n", objPlatformPath)
		} else {
			files[aplOverlayTargets[clusterspec.OverlayObjFile]] = string(clusterspec.FillObjPlaceholders(objMerged, ak))
		}
	}

	// apps — fan LLZ's desired {app: enabled} out to apl-core's per-app AplApp CRs at
	// env/apps/<name>.yaml (apl-core has no env/settings/apps.yaml). Key-level merge.
	if err := appOverlayFiles(ctx, client, cfg, files); err != nil {
		return err
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

// readObjPlatformCreds reads the obj access-key ID (a non-secret identifier) from
// OpenBao secret/obj/platform via the reconciler's Kubernetes-auth role. Returns
// ok=false (not an error) when the path/field is not seeded yet. The paired
// secret_access_key is NOT read here — it never transits the git overlay; ESO
// delivers it into the obj-secrets Secret from the same OpenBao path.
func readObjPlatformCreds(ctx context.Context, addr string) (accessKeyID string, ok bool, err error) {
	jwt, err := openbaoJWTFn()
	if err != nil {
		return "", false, err
	}
	tok, err := openbaoLoginFn(ctx, addr, jwt)
	if err != nil {
		return "", false, err
	}
	c := openbaoGetClientFn(addr, tok)
	ak, ok, err := c.Get(ctx, objPlatformPath, objCredAccessField)
	if err != nil || !ok {
		return "", false, err
	}
	if ak == "" {
		return "", false, nil
	}
	return ak, true, nil
}

func sharedOverlayPath(base string) string { return "apl-values/_shared/apl-overlay/" + base }
func envOverlayPath(env, base string) string {
	return "apl-values/" + env + "/apl-overlay/" + base
}

// appOverlayFiles reads LLZ's merged apps source (the desired {app: enabled} map) and,
// for each app whose enabled differs from apl-operator's CURRENT env/apps/<name>.yaml on
// the target branch, adds the key-level-merged AplApp CR to files. Apps whose target file
// does not exist yet (apl-operator has not seeded them) are skipped until it has — and an
// app already at the desired enabled is skipped (SetAppEnabled's semantic no-op), so the
// reconciler never churns against apl-operator's re-populated/re-formatted file.
func appOverlayFiles(ctx context.Context, client *http.Client, cfg aplOverlayConfig, files map[string]string) error {
	shared, _, err := aplOverlayReadFileFn(ctx, client, cfg.token, cfg.repo, cfg.sourceBranch, sharedOverlayPath(clusterspec.OverlayAppsFile))
	if err != nil {
		return fmt.Errorf("read _shared apps overlay: %w", err)
	}
	envLayer, _, err := aplOverlayReadFileFn(ctx, client, cfg.token, cfg.repo, cfg.sourceBranch, envOverlayPath(cfg.env, clusterspec.OverlayAppsFile))
	if err != nil {
		return fmt.Errorf("read %s apps overlay: %w", cfg.env, err)
	}
	merged, err := clusterspec.MergeOverlay([]byte(shared), []byte(envLayer))
	if err != nil {
		return fmt.Errorf("merge apps overlay: %w", err)
	}
	toggles, err := clusterspec.AppToggles(merged)
	if err != nil {
		return fmt.Errorf("parse apps toggles: %w", err)
	}
	apps := make([]string, 0, len(toggles))
	for a := range toggles {
		apps = append(apps, a)
	}
	sort.Strings(apps) // deterministic push order
	for _, app := range apps {
		target := aplAppTarget(app)
		current, found, err := aplOverlayReadFileFn(ctx, client, cfg.token, cfg.repo, cfg.targetBranch, target)
		if err != nil {
			return fmt.Errorf("read target %s: %w", target, err)
		}
		if !found {
			continue // apl-operator has not created this app's CR yet — next pass
		}
		updated, changed, err := clusterspec.SetAppEnabled([]byte(current), toggles[app])
		if err != nil {
			return fmt.Errorf("set enabled on %s: %w", target, err)
		}
		if changed {
			files[target] = string(updated)
		}
	}
	return nil
}
