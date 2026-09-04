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
	"strings"

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
	commitMessage = "chore(llz): sync apl-overlay (obj storage + app toggles + app values + teams) [ci skip]"
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

// aplOtomiTarget is apl-core's platform settings CR on the machine branch — the
// file that carries `spec.version`, and therefore the deployed platform version.
//
// NOT IN aplOverlayTargets, because that map is for files LLZ owns OUTRIGHT and
// writes whole. apl-core co-writes this one and LLZ owns a single key of it, so it
// takes the key-level path (otomiOverlayFiles) exactly as the per-app CRs do.
const aplOtomiTarget = "env/settings/otomi.yaml"

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
	// Published unconditionally below, so it goes to 0 on the pass that fixes the
	// overlay rather than silently ceasing to exist — an alert on an absent series
	// never evaluates, which is the shape this tree keeps finding.
	objSourceEmpty := 0.0

	// obj.yaml — LLZ owns the whole AplObjectStorage settings CR: merge the _shared +
	// per-env source, fill accessKeyId from the credential store, and write
	// env/settings/obj.yaml.
	objMerged, objFound, objEnvFound, err := readMergedOverlay(ctx, repo, cfg, clusterspec.OverlayObjFile)
	if err != nil {
		return err
	}
	switch {
	case !objFound:
		// No obj overlay source on the source branch. LLZ has nothing to say, so
		// it says nothing — apl-core's own AplObjectStorage CR stands.
		fmt.Println("apl-overlay: no obj.yaml overlay source for this env — leaving apl-core's obj settings alone")
	case !objOverlayIsWritable(objMerged):
		// Present but empty. Writing it would blank the CR exactly as an absent
		// source used to, so it skips — and it has to be VISIBLE, because unlike
		// the case above this one means the committed overlay is wrong.
		//
		// A GAUGE, NOT AN ANNOTATION. The first cut printed `::warning::`, which
		// is a GitHub Actions instruction — and this code runs in a pod, on a
		// reconcile loop, where nothing reads stdout for annotations and no job
		// summary exists. The condition was invisible to the only thing that
		// watches this lane. Prometheus sees it now, alongside the synced gauge
		// converge's own message points readers at.
		fmt.Println("apl-overlay: the obj.yaml overlay source has no region and/or no buckets — " +
			"refusing to write it over apl-core's AplObjectStorage CR, which would blank both. " +
			"The per-env layer supplies them; re-run `llz render` and commit the result")
		objSourceEmpty = 1
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

	// PUBLISHED HERE, NOT AT THE END, because the end is not always reached. A pass
	// whose only obj source is the empty one writes no files at all and returns
	// early at `len(files) == 0` — so the gauge announcing that exact condition
	// was skipped by the exact condition. The fact is known now; it is recorded
	// now.
	reg.SetGauge("llz_apl_overlay_obj_source_empty",
		"1 when the obj.yaml overlay source exists but merged to an empty document, so it was NOT written",
		map[string]string{"branch": cfg.TargetBranch}, objSourceEmpty)

	// A PASS THAT FOUND NOTHING MUST NOT LOOK LIKE A HEALTHY ONE. The skips above
	// are correct — no source means no opinion — but "correctly nothing to do"
	// and "looking at the wrong branch" produce the identical outcome: no error,
	// no files, and llz_apl_overlay_synced never published, because it has no 0
	// arm and the pass returns before it. A misspelled Env or SourceBranch, or a
	// token GitHub answers 404 for on a private repo, lands exactly there.
	//
	// This is the series that tells them apart, and it is published on EVERY pass
	// so it can be alerted on — a gauge that only exists when things are fine
	// cannot be.

	// apps — fan LLZ's desired {app: enabled} out to apl-core's per-app AplApp CRs at
	// env/apps/<name>.yaml (apl-core has no env/settings/apps.yaml). Key-level merge.
	appsEnvFound, err := appOverlayFiles(ctx, repo, cfg, files)
	if err != nil {
		return err
	}

	// FROM THE ENV LAYER, NOT `objFound`, and keying it on objFound made this the
	// guard-that-cannot-fire the alert beside it is named for. objFound is the OR
	// of the two layers, and the _shared path has NO env in it — so a misspelled
	// REGION, the exact case LLZAplOverlayNoSource describes, still reads the
	// shared file, still sets 1, and the alert can never fire. Same for an env
	// whose per-env tree was never rendered.
	//
	// The apps layer is the reliable witness: `llz render` emits
	// apl-values/<env>/apl-overlay/apps.yaml unconditionally, while the obj env
	// layer is conditional on the deployment declaring object storage. So an
	// absent apps env layer means the env is not in this tree; an absent obj one
	// does not. ORed anyway, so a hand-written obj-only overlay still counts.
	sourcePresent := 0.0
	if objEnvFound || appsEnvFound {
		sourcePresent = 1
	}
	reg.SetGauge("llz_apl_overlay_source_present",
		"1 when this ENV's overlay layer was found on the source branch; 0 means the reconciler is reading a tree that does not carry this env",
		map[string]string{"branch": cfg.SourceBranch, "env": cfg.Env}, sourcePresent)

	// teams — CREATE each declared team's apl-core CRs at env/teams/<name>/ when
	// absent, so apl-operator provisions the namespace + Keycloak group + realm role
	// team-<name>. Never clobbers a team apl-core / the App Platform Console owns.
	if err := teamOverlayFiles(ctx, repo, cfg, files); err != nil {
		return err
	}

	if err := otomiOverlayFiles(ctx, repo, cfg, files); err != nil {
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
func readMergedOverlay(ctx context.Context, repo Repo, cfg Config, base string) (merged []byte, found, envFound bool, err error) {
	shared, sharedFound, err := repo.ReadFile(ctx, cfg.SourceBranch, sharedOverlayPath(base))
	if err != nil {
		return nil, false, false, fmt.Errorf("read _shared %s overlay: %w", base, err)
	}
	envLayer, envFound, err := repo.ReadFile(ctx, cfg.SourceBranch, envOverlayPath(cfg.Env, base))
	if err != nil {
		return nil, false, false, fmt.Errorf("read %s %s overlay: %w", cfg.Env, base, err)
	}
	if !sharedFound && !envFound {
		return nil, false, false, nil
	}
	merged, err = clusterspec.MergeOverlay([]byte(shared), []byte(envLayer))
	if err != nil {
		return nil, false, false, fmt.Errorf("merge %s overlay: %w", base, err)
	}
	return merged, true, envFound, nil
}

// objOverlayIsWritable reports whether a merged obj overlay carries enough to
// REPLACE apl-core's AplObjectStorage CR, which is what writing it does.
//
// NON-EMPTY WAS NOT THE RIGHT BAR, and the first cut used it. The _shared layer
// alone is a valid, non-empty document — kind, metadata, showWizard, the
// accessKeyId placeholder — and carries NO region and NO buckets, because those
// are the per-env layer's whole contribution. Writing it wholesale blanks both
// on the live CR, which is the same loki/harbor outage as the `{}` case by a
// narrower route — and the gate that existed for it asserted the bug as intended
// behaviour, which is why it took a second reviewer to see. It is
// TestASharedOnlyObjOverlayIsRefusedBecauseItCarriesNoRegionOrBuckets now, with
// TestACompleteObjOverlayInOneLayerIsWritten holding the other side.
//
// The bar is CONTENT, not layer count: an operator who hand-writes a complete
// obj.yaml into _shared is fine, and an instance whose per-env layer was never
// rendered is not. region and buckets are what apl-core resolves storage with,
// so they are what must be present.
func objOverlayIsWritable(merged []byte) bool {
	var doc struct {
		Spec struct {
			Provider struct {
				Linode struct {
					Region  string            `yaml:"region"`
					Buckets map[string]string `yaml:"buckets"`
				} `yaml:"linode"`
			} `yaml:"provider"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(merged, &doc); err != nil {
		// Undecodable is NOT writable, and not silently either — the writers
		// below surface it.
		return false
	}
	l := doc.Spec.Provider.Linode
	return strings.TrimSpace(l.Region) != "" && len(l.Buckets) > 0
}

// appOverlayFiles reads LLZ's TWO per-app sources — the desired {app: enabled}
// map (apps.yaml) and the per-app chart values LLZ asserts (appvalues.yaml) — and,
// for each app whose current env/apps/<name>.yaml on the target branch differs
// from either, adds the key-level-merged AplApp CR to files. Apps whose target
// file does not exist yet (apl-operator has not seeded them) are skipped until it
// has — and an app already at the desired state is skipped (SetAppSpec's semantic
// no-op), so the reconciler never churns against apl-operator's
// re-populated/re-formatted file.
//
// BOTH SOURCES WRITE THE SAME FILE, which is why they are composed into one
// desired state (clusterspec.AppOverlays) before a single SetAppSpec call rather
// than handled by two passes. Two passes would each read the branch copy, each
// merge their own half onto it, and the second `files[target] = …` would silently
// discard the first — an app with both a toggle and values would get whichever
// half ran last, intermittently, depending on map order.
func appOverlayFiles(ctx context.Context, repo Repo, cfg Config, files map[string]string) (envFound bool, err error) {
	merged, found, envFound, err := readMergedOverlay(ctx, repo, cfg, clusterspec.OverlayAppsFile)
	if err != nil {
		return false, err
	}
	// appvalues.yaml is _shared-only (no per-env layer), so its envFound is not
	// part of this function's answer — envFound reports on the per-env apps layer,
	// which is what the caller's "is this env's overlay source present" gauge means.
	mergedValues, valuesFound, _, err := readMergedOverlay(ctx, repo, cfg, clusterspec.OverlayAppValuesFile)
	if err != nil {
		return envFound, err
	}
	if !found && !valuesFound {
		// Same rule as obj: no source means no opinion. An empty desired map would
		// write no files either, so this changes no behaviour today — it is here
		// so the two passes cannot drift into answering the question differently.
		fmt.Println("apl-overlay: no apps.yaml/appvalues.yaml overlay source for this env — leaving apl-core's app config alone")
		return envFound, nil
	}
	if !valuesFound {
		// SAID OUT LOUD, because this is the state the whole appvalues channel
		// exists to make impossible. An instance rendered before appvalues.yaml
		// existed carries apps.yaml and not this one, so its Argo CD health
		// customizations and Loki's WAL-replay headroom are asserted by nothing —
		// which is indistinguishable, from the cluster, from the pre-fix world.
		// `llz upgrade` re-renders it; until then this line is the only evidence.
		fmt.Println("apl-overlay: no appvalues.yaml overlay source on the source branch — " +
			"apl-core's own app values stand, INCLUDING the argocd health customizations " +
			"and Loki's ingester resources LLZ normally asserts. Re-run `llz render` and commit " +
			"apl-values/_shared/apl-overlay/appvalues.yaml")
	}
	desired, err := clusterspec.AppOverlays(merged, mergedValues)
	if err != nil {
		return envFound, fmt.Errorf("parse app overlays: %w", err)
	}
	apps := make([]string, 0, len(desired))
	for a := range desired {
		apps = append(apps, a)
	}
	sort.Strings(apps) // deterministic push order
	for _, app := range apps {
		target := aplAppTarget(app)
		current, found, err := repo.ReadFile(ctx, cfg.TargetBranch, target)
		if err != nil {
			return envFound, fmt.Errorf("read target %s: %w", target, err)
		}
		if !found {
			// SILENT FOR A TOGGLE, LOUD FOR VALUES, and the asymmetry is the point.
			// A missing CR is the ordinary first-boot state: apl-operator seeds
			// env/apps/<name>.yaml on its own schedule and the next pass picks it
			// up, so saying so every pass for every app would be noise.
			//
			// But an app LLZ has VALUES for is different. argocd is the case:
			// nothing else in this repo delivers its health customizations, and if
			// apl-operator never seeds that CR they are asserted by nothing — while
			// wave-health-guard, which checks the SOURCE, stays green. That is the
			// same shape as the wedge this whole channel was built for, so it gets a
			// line naming the app and the file it is waiting on.
			if len(desired[app].RawValues) > 0 {
				fmt.Printf("apl-overlay: %s has values to assert but apl-operator has not created %s "+
					"on %s yet — they are asserted by NOTHING until it does. Expected on a fresh "+
					"cluster; if it persists, apl-core is not managing this app and the values need "+
					"another home.\n", app, target, cfg.TargetBranch)
			}
			continue
		}
		updated, changed, err := clusterspec.SetAppSpec([]byte(current), desired[app])
		if err != nil {
			return envFound, fmt.Errorf("set spec on %s: %w", target, err)
		}
		if changed {
			files[target] = string(updated)
		}
	}
	return envFound, nil
}

// otomiOverlayFiles asserts the platform VERSION onto apl-core's own settings CR
// when the instance has opted into owning it.
//
// THIS IS THE HALF THAT WAS MISSING, and its absence is why the render alone did
// nothing: `llz render` wrote apl-values/<env>/apl-overlay/otomi.yaml, and no
// target consumed it. aplOverlayTargets maps only obj.yaml; apps and teams have
// their own target functions. So the file was committed, reviewed, and never
// reached the cluster — the rendered-into-the-void shape this tree has shipped
// before.
//
// No source means the instance has not set spec.cluster.bootstrap.manageAplVersion,
// which is the default: Linode versions apl-core on managed and taking that over is
// a decision. LLZ then has no opinion and says nothing.
func otomiOverlayFiles(ctx context.Context, repo Repo, cfg Config, files map[string]string) error {
	srcPath := envOverlayPath(cfg.Env, clusterspec.OverlayOtomiFile)
	src, found, err := repo.ReadFile(ctx, cfg.SourceBranch, srcPath)
	if err != nil {
		return fmt.Errorf("read source %s: %w", srcPath, err)
	}
	if !found {
		return nil // not opted in — Linode owns the version
	}
	want, err := clusterspec.OtomiOverlayVersion([]byte(src))
	if err != nil {
		return fmt.Errorf("parse %s: %w", srcPath, err)
	}
	if want == "" {
		return nil
	}
	current, exists, err := repo.ReadFile(ctx, cfg.TargetBranch, aplOtomiTarget)
	if err != nil {
		return fmt.Errorf("read target %s: %w", aplOtomiTarget, err)
	}
	if !exists {
		// LOUD, not silent, and unlike a per-app CR this one does not get a pass.
		// An instance that asked to own the platform version and is asserting it
		// against nothing is the exact state this channel exists to prevent, and it
		// is invisible from the source side — the overlay file is present and
		// correct on the instance branch either way.
		fmt.Printf("apl-overlay: manageAplVersion is set (want apl-core %s) but %s does not exist on %s yet — "+
			"the platform version is asserted by NOTHING until apl-operator creates it. Expected on a fresh "+
			"cluster; if it persists, apl-core is not keeping its settings there and the version needs another home.\n",
			want, aplOtomiTarget, cfg.TargetBranch)
		return nil
	}
	updated, changed, err := clusterspec.SetOtomiVersion([]byte(current), want)
	if err != nil {
		return fmt.Errorf("set version on %s: %w", aplOtomiTarget, err)
	}
	if changed {
		files[aplOtomiTarget] = string(updated)
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
