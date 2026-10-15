package overlay

// platformbackups_test.go — the database backups the spec turns on must REACH
// apl-core's own settings CR, and must not take apl-core's other settings with
// them. Asserted on the TARGET branch, which is the only thing apl-operator reads.

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/metrics"
)

func cnpgSource(t *testing.T) string {
	t.Helper()
	on := true
	src := clusterspec.RenderPlatformBackupsOverlayEnv(clusterspec.Bootstrap{ManagedApps: clusterspec.DefaultManagedApps},
		map[string]clusterspec.ComponentToggle{clusterspec.CnpgBackupComponent: {Enabled: &on}})
	if src == "" {
		t.Fatal("the renderer produced no platformBackups source with the component on")
	}
	return src
}

// apl-core's CR as the Console and apl-operator keep it: gitea's database entry
// and the gitea application backup are apl-core's and must survive.
const livePlatformBackups = `kind: AplBackupCollection
metadata:
    name: platformBackups
spec:
    database:
        gitea:
            enabled: false
            pathSuffix: gitea
            retentionPolicy: 7d
            schedule: 0 0 * * *
        harbor:
            enabled: false
            pathSuffix: harbor
            retentionPolicy: 7d
            schedule: 0 0 * * *
    gitea:
        enabled: true
        retentionPolicy: 7d
        schedule: 0 0 * * *
`

func gaugeOf(t *testing.T, reg *metrics.Registry) string {
	t.Helper()
	var b bytes.Buffer
	if _, err := reg.WriteTo(&b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestBackupSettingsReachTheMachineBranchKeyLevel(t *testing.T) {
	repo := &branchRepo{
		src: map[string]string{envOverlayPath("primary", clusterspec.OverlayPlatformBackupsFile): cnpgSource(t)},
		tgt: map[string]string{aplPlatformBackupsTarget: livePlatformBackups},
	}
	reg := metrics.NewRegistry()
	if err := Reconcile(context.Background(), testCfg(), repo, credsOK, reg); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got, ok := repo.gotFiles[aplPlatformBackupsTarget]
	if !ok {
		t.Fatalf("%s was never written — the rendered overlay reached nothing: %v", aplPlatformBackupsTarget, keysOf(repo.gotFiles))
	}
	for _, want := range []string{"keycloak-1", "harbor-1", "retentionPolicy: 30d", "7 1 * * *"} {
		if !strings.Contains(got, want) {
			t.Errorf("the asserted backup settings did not land (%s):\n%s", want, got)
		}
	}
	// apl-core's own entries survive the merge.
	if !strings.Contains(got, "pathSuffix: gitea") || !strings.Contains(got, "kind: AplBackupCollection") {
		t.Errorf("the merge dropped apl-core's own settings:\n%s", got)
	}
	if !strings.Contains(gaugeOf(t, reg), `llz_apl_overlay_platform_backups_target_unusable{branch="apl-primary"} 0`) {
		t.Errorf("the unusable gauge is not published at 0 on a healthy pass:\n%s", gaugeOf(t, reg))
	}

	// Second pass over the result: no commit.
	repo2 := &branchRepo{src: repo.src, tgt: map[string]string{aplPlatformBackupsTarget: got}}
	if err := Reconcile(context.Background(), testCfg(), repo2, credsOK, metrics.NewRegistry()); err != nil {
		t.Fatal(err)
	}
	if _, ok := repo2.gotFiles[aplPlatformBackupsTarget]; ok {
		t.Error("an already-correct CR was pushed again — the reconciler would churn against apl-operator")
	}
}

// No CR on the branch yet: created from the source, so the component is not
// wired-and-inert on a branch where apl-core never wrote the file.
func TestAnAbsentBackupCRIsCreated(t *testing.T) {
	src := cnpgSource(t)
	repo := &branchRepo{
		src: map[string]string{envOverlayPath("primary", clusterspec.OverlayPlatformBackupsFile): src},
		tgt: map[string]string{},
	}
	if err := Reconcile(context.Background(), testCfg(), repo, credsOK, metrics.NewRegistry()); err != nil {
		t.Fatal(err)
	}
	if repo.gotFiles[aplPlatformBackupsTarget] != src {
		t.Errorf("absent target: wrote %q, want the source as rendered", repo.gotFiles[aplPlatformBackupsTarget])
	}
}

// Component off → no source → no opinion: apl-core's CR, Console edits included,
// is left exactly as it is.
func TestWithoutTheComponentBackupSettingsAreLeftAlone(t *testing.T) {
	repo := &branchRepo{src: map[string]string{}, tgt: map[string]string{aplPlatformBackupsTarget: livePlatformBackups}}
	if err := Reconcile(context.Background(), testCfg(), repo, credsOK, metrics.NewRegistry()); err != nil {
		t.Fatal(err)
	}
	if _, ok := repo.gotFiles[aplPlatformBackupsTarget]; ok {
		t.Errorf("no source must mean no opinion, but %s was written", aplPlatformBackupsTarget)
	}
}

// A degenerate target (exists, empty — what the GitHub adapter returns for an empty
// file) is NOT a merge base: merging into it would push a kind-less fragment over
// apl-core's CR. Skipped, visibly, and the rest of the pass still syncs.
func TestADegenerateBackupCRIsNotMergedInto(t *testing.T) {
	for name, tgt := range map[string]string{"empty": "", "no kind": "spec: {}\n", "two docs": livePlatformBackups + "---\nkind: X\n"} {
		t.Run(name, func(t *testing.T) {
			repo := &branchRepo{
				src: map[string]string{
					envOverlayPath("primary", clusterspec.OverlayPlatformBackupsFile): cnpgSource(t),
					// something else for the pass to still sync
					sharedOverlayPath(clusterspec.OverlayObjFile):         clusterspec.RenderObjOverlayShared(),
					envOverlayPath("primary", clusterspec.OverlayObjFile): clusterspec.RenderObjOverlayEnv("acme", "primary", "us-ord-1"),
				},
				tgt: map[string]string{aplPlatformBackupsTarget: tgt},
			}
			reg := metrics.NewRegistry()
			if err := Reconcile(context.Background(), testCfg(), repo, credsOK, reg); err != nil {
				t.Fatalf("a bad target on a file LLZ does not own must not fail the pass: %v", err)
			}
			if _, ok := repo.gotFiles[aplPlatformBackupsTarget]; ok {
				t.Errorf("merged into a degenerate target: %q", repo.gotFiles[aplPlatformBackupsTarget])
			}
			if _, ok := repo.gotFiles[aplOverlayTargets[clusterspec.OverlayObjFile]]; !ok {
				t.Error("the rest of the pass (obj.yaml) was dropped")
			}
			if !strings.Contains(gaugeOf(t, reg), `llz_apl_overlay_platform_backups_target_unusable{branch="apl-primary"} 1`) {
				t.Errorf("the skip was silent — no unusable gauge:\n%s", gaugeOf(t, reg))
			}
		})
	}
}
