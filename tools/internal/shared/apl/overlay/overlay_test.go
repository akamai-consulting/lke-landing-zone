package overlay

import (
	"context"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/metrics"
)

func testCfg() Config {
	return Config{Env: "primary", SourceBranch: "main", TargetBranch: "apl-primary"}
}

// fakeRepo backs overlay.Repo from an in-memory path→content map. Reads are
// branch-agnostic: a file's role (source vs target) is encoded in its path
// (sharedOverlayPath / envOverlayPath / aplAppTarget / aplTeamTarget are all
// distinct), so keying on path alone reproduces the real branch layout.
type fakeRepo struct {
	files     map[string]string
	commitErr error

	gotFiles  map[string]string
	gotBranch string
}

func (f *fakeRepo) ReadFile(_ context.Context, _, path string) (string, bool, error) {
	c := f.files[path]
	return c, c != "", nil
}

func (f *fakeRepo) OverlayCommit(_ context.Context, branch string, files map[string]string, _ string, _ int) (string, bool, error) {
	f.gotFiles, f.gotBranch = files, branch
	if f.commitErr != nil {
		return "", false, f.commitErr
	}
	return "newsha", true, nil
}

func credsOK(context.Context) (string, bool, error)      { return "AKID", true, nil }
func credsMissing(context.Context) (string, bool, error) { return "", false, nil }

func TestReconcile_FillsAndOverlays(t *testing.T) {
	repo := &fakeRepo{files: map[string]string{
		sharedOverlayPath(clusterspec.OverlayObjFile):         clusterspec.RenderObjOverlayShared(),
		envOverlayPath("primary", clusterspec.OverlayObjFile): clusterspec.RenderObjOverlayEnv("acme", "primary", "us-ord-1"),
		// apps SOURCE: LLZ wants knative OFF (bare apps: map — LLZ's desired-state input).
		sharedOverlayPath(clusterspec.OverlayAppsFile):         "apps:\n  knative:\n    enabled: false\n",
		envOverlayPath("primary", clusterspec.OverlayAppsFile): "",
		// apl-operator's CURRENT per-app CR on the TARGET branch: enabled + owned config.
		aplAppTarget("knative"): "kind: AplApp\nmetadata:\n  name: knative\nspec:\n  enabled: true\n  resources:\n    foo: bar\n",
	}}

	if err := Reconcile(context.Background(), testCfg(), repo, credsOK, metrics.NewRegistry()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if repo.gotBranch != "apl-primary" {
		t.Errorf("target branch = %q, want apl-primary", repo.gotBranch)
	}
	obj, ok := repo.gotFiles[aplOverlayTargets[clusterspec.OverlayObjFile]]
	if !ok {
		t.Fatalf("obj target not in overlay files: %v", keysOf(repo.gotFiles))
	}
	// apps fanned out to the per-app AplApp CR: enabled flipped to LLZ's desired value,
	// apl-operator's owned config (resources) key-level-preserved.
	knative, ok := repo.gotFiles[aplAppTarget("knative")]
	if !ok {
		t.Fatalf("per-app target %q not in overlay files: %v", aplAppTarget("knative"), keysOf(repo.gotFiles))
	}
	if !strings.Contains(knative, "enabled: false") {
		t.Errorf("knative.yaml must have enabled flipped to false:\n%s", knative)
	}
	if !strings.Contains(knative, "foo: bar") {
		t.Errorf("knative.yaml must preserve apl-operator's owned config (resources):\n%s", knative)
	}
	// The accessKeyId placeholder was filled from the credential store.
	if strings.Contains(obj, clusterspec.ObjAccessKeyIDPlaceholder) {
		t.Errorf("obj.yaml still has the accessKeyId placeholder after fill:\n%s", obj)
	}
	if !strings.Contains(obj, "AKID") {
		t.Errorf("obj.yaml missing filled accessKeyId:\n%s", obj)
	}
	// The secret NEVER transits git: apl-core reads secretAccessKey from obj-secrets
	// via ESO, so it is ABSENT from the settings overlay entirely.
	if strings.Contains(obj, "secretAccessKey") {
		t.Errorf("obj.yaml must NOT carry secretAccessKey (ESO owns it):\n%s", obj)
	}
	if !strings.Contains(obj, "kind: AplObjectStorage") {
		t.Errorf("obj.yaml must be the AplObjectStorage CR:\n%s", obj)
	}
	if !strings.Contains(obj, "us-ord-1") || !strings.Contains(obj, "acme-loki-chunks-primary") {
		t.Errorf("obj.yaml missing merged env region/buckets:\n%s", obj)
	}
}

// When the obj credential is not seeded, obj.yaml is SKIPPED (never push a
// placeholder) but the app toggles still sync (they carry no secret).
func TestReconcile_SkipsObjWhenCredMissing(t *testing.T) {
	repo := &fakeRepo{files: map[string]string{
		sharedOverlayPath(clusterspec.OverlayObjFile):          clusterspec.RenderObjOverlayShared(),
		envOverlayPath("primary", clusterspec.OverlayObjFile):  clusterspec.RenderObjOverlayEnv("acme", "primary", "us-ord-1"),
		sharedOverlayPath(clusterspec.OverlayAppsFile):         "apps:\n  knative:\n    enabled: false\n",
		envOverlayPath("primary", clusterspec.OverlayAppsFile): "",
		aplAppTarget("knative"):                                "kind: AplApp\nmetadata:\n  name: knative\nspec:\n  enabled: true\n",
	}}
	if err := Reconcile(context.Background(), testCfg(), repo, credsMissing, metrics.NewRegistry()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, ok := repo.gotFiles[aplOverlayTargets[clusterspec.OverlayObjFile]]; ok {
		t.Error("obj.yaml must be skipped when the credential is not seeded (no placeholder push)")
	}
	if _, ok := repo.gotFiles[aplAppTarget("knative")]; !ok {
		t.Error("app toggles must still sync when the obj credential is missing")
	}
}

// A missing apl-<env> branch (apl-operator not bootstrapped yet) is a no-op, not
// an error — obj.yaml is populated so the commit is reached and returns ErrRefNotFound.
func TestReconcile_MissingBranchIsNoOp(t *testing.T) {
	repo := &fakeRepo{
		files: map[string]string{
			sharedOverlayPath(clusterspec.OverlayObjFile):          clusterspec.RenderObjOverlayShared(),
			envOverlayPath("primary", clusterspec.OverlayObjFile):  clusterspec.RenderObjOverlayEnv("acme", "primary", "us-ord-1"),
			sharedOverlayPath(clusterspec.OverlayAppsFile):         clusterspec.RenderAppsOverlayShared(),
			envOverlayPath("primary", clusterspec.OverlayAppsFile): "",
		},
		commitErr: ErrRefNotFound,
	}
	if err := Reconcile(context.Background(), testCfg(), repo, credsOK, metrics.NewRegistry()); err != nil {
		t.Errorf("missing target branch must be a no-op, got: %v", err)
	}
	if repo.gotBranch != "apl-primary" {
		t.Errorf("commit must have been attempted on apl-primary, got %q", repo.gotBranch)
	}
}

// A declared team whose apl-core CRs are absent on the target branch gets them
// created, so apl-operator provisions the namespace + Keycloak group + realm role.
func TestReconcile_ProvisionsTeamsWhenAbsent(t *testing.T) {
	repo := &fakeRepo{files: map[string]string{
		envOverlayPath("primary", clusterspec.OverlayTeamsFile):          clusterspec.RenderTeamsManifest([]clusterspec.Team{{Name: "platform"}}),
		envTeamPath("primary", "platform", clusterspec.TeamSettingsFile): clusterspec.RenderTeamSettings(clusterspec.Team{Name: "platform"}),
		envTeamPath("primary", "platform", clusterspec.TeamAppsFile):     clusterspec.RenderTeamApps("platform"),
		// target env/teams/platform/* is ABSENT (not in map → read returns not-found)
	}}
	if err := Reconcile(context.Background(), testCfg(), repo, credsMissing, metrics.NewRegistry()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	for _, f := range []string{clusterspec.TeamSettingsFile, clusterspec.TeamAppsFile} {
		if _, ok := repo.gotFiles[aplTeamTarget("platform", f)]; !ok {
			t.Errorf("team CR %s must be created on the target branch: files=%v", aplTeamTarget("platform", f), keysOf(repo.gotFiles))
		}
	}
	if !strings.Contains(repo.gotFiles[aplTeamTarget("platform", clusterspec.TeamSettingsFile)], "AplTeamSettingSet") {
		t.Error("provisioned settings.yaml must be the AplTeamSettingSet CR")
	}
}

// Once the team CRs exist on the target branch (apl-core / the console owns them —
// members, quota), the reconciler leaves them untouched.
func TestReconcile_NeverClobbersExistingTeam(t *testing.T) {
	repo := &fakeRepo{files: map[string]string{
		envOverlayPath("primary", clusterspec.OverlayTeamsFile):          clusterspec.RenderTeamsManifest([]clusterspec.Team{{Name: "platform"}}),
		envTeamPath("primary", "platform", clusterspec.TeamSettingsFile): clusterspec.RenderTeamSettings(clusterspec.Team{Name: "platform"}),
		envTeamPath("primary", "platform", clusterspec.TeamAppsFile):     clusterspec.RenderTeamApps("platform"),
		// target ALREADY has the team, with console-owned edits (a member):
		aplTeamTarget("platform", clusterspec.TeamSettingsFile): "kind: AplTeamSettingSet\nmetadata:\n  name: platform\nspec:\n  members:\n    - alice\n",
		aplTeamTarget("platform", clusterspec.TeamAppsFile):     "kind: AplTeamTool\nmetadata:\n  name: platform\nspec: {}\n",
	}}
	if err := Reconcile(context.Background(), testCfg(), repo, credsMissing, metrics.NewRegistry()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, ok := repo.gotFiles[aplTeamTarget("platform", clusterspec.TeamSettingsFile)]; ok {
		t.Error("an existing team's settings.yaml must NOT be overwritten (apl-core/console owns it)")
	}
}

func keysOf(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
