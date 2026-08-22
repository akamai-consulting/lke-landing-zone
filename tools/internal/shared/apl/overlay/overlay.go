// Package overlay is the apl-overlay git-to-git config-sync reconciler, lifted out
// of package main per ADR 0013 Phase 1 (docs/adr/0013-llz-as-apl-cli.md) — the
// values front door the ADR names as its flagship. It reads LLZ's apl-overlay from
// the human-owned source branch, fills the obj credential, merges the _shared +
// <env> layers, and overlays ONLY the owned files onto the machine-owned apl-<env>
// branch with a non-force fast-forward (ff-retry) — the config role a force-push
// used to play, letting this reconciler and apl-operator share the branch without
// clobbering each other.
//
// Transport is ABOVE this package: the caller supplies a Repo (cmd/llz's GitHub
// git-data-API client) and an ObjCreds reader (OpenBao). overlay itself is
// transport-agnostic — no net/http, no git binary, no OpenBao — so it is
// unit-testable against an in-memory fake Repo. Repo is to overlay what
// provider.ClusterProvider is to the cluster: the substrate seam it depends on,
// declared here at the consumer. See docs/designs/apl-overlay-obj-native.md for
// the owned-file mapping.
package overlay

import (
	"context"
	"errors"
	"fmt"
	"sort"

	yaml "gopkg.in/yaml.v3"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/metrics"
)

// ErrRefNotFound signals that the target (apl-<env>) branch does not exist yet —
// apl-operator has not bootstrapped it. The Repo implementation returns it (the
// cmd/llz adapter maps GitHub's ref-404 onto it); Reconcile treats it as an
// expected first-boot no-op, not an error.
var ErrRefNotFound = errors.New("apl-overlay: target branch not found")

// Repo is the git-data substrate overlay needs: read a file at a branch, and
// overlay a set of files onto a branch as one fast-forward commit. cmd/llz backs
// it with the GitHub git-data REST API; tests back it with an in-memory fake.
type Repo interface {
	ReadFile(ctx context.Context, branch, path string) (content string, found bool, err error)
	OverlayCommit(ctx context.Context, branch string, files map[string]string, message string, maxAttempts int) (sha string, changed bool, err error)
}

// ObjCreds reads the obj access-key ID (a non-secret identifier the overlay
// carries in git) from the credential store. ok=false (not an error) means it is
// not seeded yet — Reconcile then skips obj.yaml rather than pushing a placeholder.
// The paired secret_access_key is never read here — ESO delivers it directly into
// the obj-secrets Secret.
type ObjCreds func(ctx context.Context) (accessKeyID string, ok bool, err error)

// Config is the per-env wiring Reconcile needs (the branches + env name). cmd/llz
// builds it from the reconciler's env contract.
type Config struct {
	Env          string // deployment/env name (overlay path + branch suffix)
	SourceBranch string // human-owned branch the overlay source lives on (main)
	TargetBranch string // machine-owned branch to overlay onto (apl-<env>)
}

// commitMessage is the reconciler's overlay-push subject — distinct from
// apl-operator's "otomi commit" so the two writers' history stays legible.
// maxAttempts bounds the fast-forward retry loop against concurrent pushes.
const (
	commitMessage = "chore(llz): sync apl-overlay (obj storage + app toggles + teams) [ci skip]"
	maxAttempts   = 4
)

// aplOverlayTargets maps each rendered overlay file to the path in the apl-<env>
// values tree apl-core reads as config input.
//
// LAB-GATED: the exact env-tree paths — and whether a target needs a key-level
// (not file-level) merge because apl-operator co-writes the same file — are
// apl-core-internal and provable only on a live cluster. Isolated here so a lab
// finding is a one-line correction. Only obj.yaml maps to a single fixed file (the
// AplObjectStorage settings CR LLZ owns outright). App toggles are NOT a single
// apps.yaml — apl-core keeps per-app AplApp CRs at env/apps/<name>.yaml, so the
// reconciler fans the desired toggles out there (aplAppTarget) with a key-level
// merge instead.
var aplOverlayTargets = map[string]string{
	clusterspec.OverlayObjFile: "env/settings/obj.yaml",
}

// aplAppTarget is apl-core's per-app AplApp CR path on the machine branch.
func aplAppTarget(app string) string { return "env/apps/" + app + ".yaml" }

// aplTeamTarget is a team CR's path under apl-core's env/teams/<name>/ on the
// machine branch; envTeamPath is the same CR in LLZ's committed per-env overlay.
func aplTeamTarget(name, file string) string { return "env/teams/" + name + "/" + file }
func envTeamPath(env, name, file string) string {
	return "apl-values/" + env + "/apl-overlay/teams/" + name + "/" + file
}

func sharedOverlayPath(base string) string { return "apl-values/_shared/apl-overlay/" + base }
func envOverlayPath(env, base string) string {
	return "apl-values/" + env + "/apl-overlay/" + base
}

// Reconcile runs one sync pass: read → fill → merge → overlay. It is leader-gated
// by the caller (it writes). It returns nil (a no-op) — never an error — for the
// two expected transient states: the apl-<env> branch not yet created by
// apl-operator (ErrRefNotFound), and the obj credential not yet seeded (ObjCreds
// ok=false). A genuine API/merge failure IS returned (the manager records up=0).
func Reconcile(ctx context.Context, cfg Config, repo Repo, objCreds ObjCreds, reg *metrics.Registry) error {
	files := map[string]string{}

	// obj.yaml — LLZ owns the whole AplObjectStorage settings CR: merge the _shared +
	// per-env source, fill accessKeyId from the credential store, and write
	// env/settings/obj.yaml.
	objMerged, objFound, err := readMergedOverlay(ctx, repo, cfg, clusterspec.OverlayObjFile)
	if err != nil {
		return err
	}
	switch {
	case !objFound:
		// No obj overlay source on the source branch. LLZ has nothing to say, so
		// it says nothing — apl-core's own AplObjectStorage CR stands.
		fmt.Println("apl-overlay: no obj.yaml overlay source for this env — leaving apl-core's obj settings alone")
	case overlayIsEmpty(objMerged):
		// Present but empty. Writing it would blank the CR exactly as an absent
		// source used to, so it skips — loudly, because unlike the case above
		// this one means the committed overlay is wrong.
		fmt.Println("::warning::apl-overlay: the obj.yaml overlay source merged to an empty document — " +
			"refusing to write it over apl-core's AplObjectStorage CR. Re-run `llz render` and commit the result")
	default:
		ak, ok, err := objCreds(ctx)
		if err != nil {
			return fmt.Errorf("read obj platform credential: %w", err)
		}
		if !ok {
			// accessKeyId not seeded yet — NEVER push the literal placeholder onto the
			// machine branch (a broken obj cred). Skip obj this pass; apps still sync.
			fmt.Println("apl-overlay: obj platform credential not seeded yet — skipping obj.yaml this pass")
		} else {
			files[aplOverlayTargets[clusterspec.OverlayObjFile]] = string(clusterspec.FillObjPlaceholders(objMerged, ak))
		}
	}

	// apps — fan LLZ's desired {app: enabled} out to apl-core's per-app AplApp CRs at
	// env/apps/<name>.yaml (apl-core has no env/settings/apps.yaml). Key-level merge.
	if err := appOverlayFiles(ctx, repo, cfg, files); err != nil {
		return err
	}

	// teams — CREATE each declared team's apl-core CRs at env/teams/<name>/ when
	// absent, so apl-operator provisions the namespace + Keycloak group + realm role
	// team-<name>. Never clobbers a team apl-core / the App Platform Console owns.
	if err := teamOverlayFiles(ctx, repo, cfg, files); err != nil {
		return err
	}

	if len(files) == 0 {
		return nil // nothing to sync
	}

	sha, changed, err := repo.OverlayCommit(ctx, cfg.TargetBranch, files, commitMessage, maxAttempts)
	if errors.Is(err, ErrRefNotFound) {
		// apl-operator has not created/bootstrapped apl-<env> yet — there is no tree
		// to overlay onto. Expected during first boot; not an error.
		fmt.Printf("apl-overlay: target branch %q absent (apl-operator not bootstrapped yet) — will retry next pass\n", cfg.TargetBranch)
		return nil
	}
	if err != nil {
		return err
	}
	reg.SetGauge("llz_apl_overlay_synced",
		"1 when the apl-overlay reconciler's last git sync succeeded",
		map[string]string{"branch": cfg.TargetBranch}, 1)
	if changed {
		fmt.Printf("apl-overlay: synced %d file(s) onto %s at %s\n", len(files), cfg.TargetBranch, sha)
	}
	return nil
}

// readMergedOverlay reads the _shared + <env> layers of one overlay file (base,
// e.g. OverlayObjFile) from the source branch and deep-merges them (env wins) —
// the read→read→merge sequence the obj and apps passes share.
// It returns found=false when NEITHER layer exists on the source branch, which
// is the difference between "LLZ has nothing to say about this file" and "LLZ
// says this file is empty" — and both `found` flags used to be discarded.
//
// THE DISCARDED FLAGS TURNED AN ABSENT SOURCE INTO AN INSTRUCTION TO BLANK A CR.
// repo.ReadFile answers a missing path with ("", false, nil), so two absent
// layers merged as MergeOverlay([]byte(""), []byte("")) — and that does not come
// back empty. It comes back "{}\n", the canonical YAML for an empty map. The
// caller's `len(bytes.TrimSpace(objMerged)) > 0` guard therefore PASSED, and the
// reconciler wrote `{}` to env/settings/obj.yaml on the machine branch, over
// apl-core's live AplObjectStorage CR. Object storage for loki and harbor stops
// resolving, and nothing about it looks like a failure: the commit succeeds, the
// lane reports synced, and the file it wrote is valid YAML.
//
// A guard that cannot fire is worse than no guard, because it is the reason
// nobody looked again.
func readMergedOverlay(ctx context.Context, repo Repo, cfg Config, base string) (merged []byte, found bool, err error) {
	shared, sharedFound, err := repo.ReadFile(ctx, cfg.SourceBranch, sharedOverlayPath(base))
	if err != nil {
		return nil, false, fmt.Errorf("read _shared %s overlay: %w", base, err)
	}
	envLayer, envFound, err := repo.ReadFile(ctx, cfg.SourceBranch, envOverlayPath(cfg.Env, base))
	if err != nil {
		return nil, false, fmt.Errorf("read %s %s overlay: %w", cfg.Env, base, err)
	}
	if !sharedFound && !envFound {
		return nil, false, nil
	}
	merged, err = clusterspec.MergeOverlay([]byte(shared), []byte(envLayer))
	if err != nil {
		return nil, false, fmt.Errorf("merge %s overlay: %w", base, err)
	}
	return merged, true, nil
}

// overlayIsEmpty reports whether a merged overlay carries no keys at all.
//
// SEPARATE FROM `found` ON PURPOSE. A source that EXISTS and merges to nothing
// is a different fault from a source that is absent — a render bug rather than
// an un-rendered instance — but it has the identical consequence if written, so
// both skip. Checking the parsed document rather than the bytes: "{}", "{}\n",
// "---\n{}\n" and a file of only comments are all the same emptiness, and a
// string comparison catches one of them.
func overlayIsEmpty(merged []byte) bool {
	var m map[string]any
	if err := yaml.Unmarshal(merged, &m); err != nil {
		// Undecodable is NOT empty. It is a genuine fault the caller should see
		// rather than quietly skip, and the writers below will surface it.
		return false
	}
	return len(m) == 0
}

// appOverlayFiles reads LLZ's merged apps source (the desired {app: enabled} map)
// and, for each app whose enabled differs from apl-operator's CURRENT
// env/apps/<name>.yaml on the target branch, adds the key-level-merged AplApp CR to
// files. Apps whose target file does not exist yet (apl-operator has not seeded
// them) are skipped until it has — and an app already at the desired enabled is
// skipped (SetAppEnabled's semantic no-op), so the reconciler never churns against
// apl-operator's re-populated/re-formatted file.
func appOverlayFiles(ctx context.Context, repo Repo, cfg Config, files map[string]string) error {
	merged, found, err := readMergedOverlay(ctx, repo, cfg, clusterspec.OverlayAppsFile)
	if err != nil {
		return err
	}
	if !found {
		// Same rule as obj: no source means no opinion. An empty toggle map would
		// write no files either, so this changes no behaviour today — it is here
		// so the two passes cannot drift into answering the question differently.
		fmt.Println("apl-overlay: no apps.yaml overlay source for this env — leaving apl-core's app toggles alone")
		return nil
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
		current, found, err := repo.ReadFile(ctx, cfg.TargetBranch, target)
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

// teamOverlayFiles reads LLZ's per-env teams manifest and, for each declared team,
// adds its apl-core CRs (settings.yaml + apps.yaml) to files ONLY when they are
// absent on the target branch — a CREATE-if-absent. Once they exist apl-core / the
// App Platform Console owns them (members, quota), so the reconciler never
// overwrites them. A missing manifest (no declared teams) is a clean no-op.
func teamOverlayFiles(ctx context.Context, repo Repo, cfg Config, files map[string]string) error {
	manifest, found, err := repo.ReadFile(ctx, cfg.SourceBranch, envOverlayPath(cfg.Env, clusterspec.OverlayTeamsFile))
	if err != nil {
		return fmt.Errorf("read teams manifest: %w", err)
	}
	if !found {
		return nil
	}
	names, err := clusterspec.TeamNames([]byte(manifest))
	if err != nil {
		return fmt.Errorf("parse teams manifest: %w", err)
	}
	sort.Strings(names) // deterministic push order
	for _, name := range names {
		for _, file := range []string{clusterspec.TeamSettingsFile, clusterspec.TeamAppsFile} {
			target := aplTeamTarget(name, file)
			if _, exists, err := repo.ReadFile(ctx, cfg.TargetBranch, target); err != nil {
				return fmt.Errorf("read target %s: %w", target, err)
			} else if exists {
				continue // apl-core / the console owns the team now — never clobber
			}
			src, srcFound, err := repo.ReadFile(ctx, cfg.SourceBranch, envTeamPath(cfg.Env, name, file))
			if err != nil {
				return fmt.Errorf("read source %s: %w", envTeamPath(cfg.Env, name, file), err)
			}
			if srcFound {
				files[target] = src
			}
		}
	}
	return nil
}
