package harbor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/openbao"
)

// jsonDecode is a tiny helper shared with harborStub.
func jsonDecode(r *http.Request, v any) error { return json.NewDecoder(r.Body).Decode(v) }

// fakeBaoStore implements openbao.BaoStore in memory.
type fakeBaoStore struct {
	data     map[string]map[string]string
	writes   []string
	getErr   error
	writeErr error
}

func (f *fakeBaoStore) Get(_ context.Context, path, key string) (string, bool, error) {
	if f.getErr != nil {
		return "", false, f.getErr
	}
	kv, ok := f.data[path]
	if !ok {
		return "", false, nil
	}
	v, ok := kv[key]
	return v, ok, nil
}

func (f *fakeBaoStore) Write(_ context.Context, path string, data map[string]string) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.writes = append(f.writes, fmt.Sprintf("%s username=%s password=%s registry_host=%s",
		path, data["username"], data["password"], data["registry_host"]))
	return nil
}

// setProvisionerEnv pins the provisioner's env contract and seams: a mounted
// admin-password file, a fake bao store, and a recording gh publisher.
func setProvisionerEnv(t *testing.T, adminPass string, store *fakeBaoStore) (gh *[]string) {
	t.Helper()
	dir := t.TempDir()
	passFile := filepath.Join(dir, "HARBOR_ADMIN_PASSWORD")
	if adminPass != "" {
		if err := os.WriteFile(passFile, []byte(adminPass+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HARBOR_ADMIN_PASSWORD_FILE", passFile)
	t.Setenv("HARBOR_HOST", "harbor.env.internal")
	t.Setenv("GH_TOKEN", "ghp_test")
	t.Setenv("GH_REPO", "acme/platform")
	t.Setenv("GITHUB_ACTIONS", "") // in-cluster: no masking, no summaries
	t.Setenv("GITHUB_STEP_SUMMARY", "")

	origStore, origGH, origExists := newProvisionerBaoStore, ghPublishRepoSecret, ghRepoSecretExists
	gh = new([]string)
	newProvisionerBaoStore = func(context.Context) (openbao.BaoStore, error) { return store, nil }
	ghPublishRepoSecret = func(name, value string) error {
		*gh = append(*gh, name+"="+value)
		return nil
	}
	ghRepoSecretExists = func(string) (bool, error) { return true, nil } // steady state: published
	t.Cleanup(func() {
		newProvisionerBaoStore, ghPublishRepoSecret, ghRepoSecretExists = origStore, origGH, origExists
	})
	return gh
}

func TestHarborProvisionerNoAdminPasswordIsCleanNoop(t *testing.T) {
	store := &fakeBaoStore{}
	setProvisionerEnv(t, "", store) // file never written → read fails
	origStore := newProvisionerBaoStore
	newProvisionerBaoStore = func(context.Context) (openbao.BaoStore, error) {
		t.Error("bao login must not happen before Harbor is deployed")
		return store, nil
	}
	t.Cleanup(func() { newProvisionerBaoStore = origStore })

	var err error
	out := captureStdout(t, func() { err = RunProvisioner() })
	if err != nil {
		t.Fatalf("missing admin password must be a clean no-op, got %v", err)
	}
	if !strings.Contains(out, "Harbor not deployed yet") {
		t.Errorf("stdout %q missing the not-deployed note", out)
	}
}

func TestHarborProvisionerSteadyStateNoop(t *testing.T) {
	srv, payloads := harborStub(t, http.StatusCreated, nil)
	// registry_host is part of a seeded path — every writer (this command and
	// seed-standby-harbor-robots) writes all three keys together. It is spelled out
	// here because the steady state is now "seeded AND the stored registry_host is
	// usable": a path missing it is a real defect (cert-automation's
	// harborDockerConfig ExternalSecret reads that property, and cannot sync
	// without it), so the loop repairs it rather than calling it converged.
	store := &fakeBaoStore{data: map[string]map[string]string{
		"secret/harbor/robot":      {"username": "robot$ci-firewall-controller", "password": "sec", "registry_host": "harbor.env.internal"},
		"secret/harbor/pull-robot": {"username": "robot$pull-platform", "password": "psec", "registry_host": "harbor.env.internal"},
	}}
	gh := setProvisionerEnv(t, "adminpass", store)
	t.Setenv("HARBOR_API_URL", srv.URL)

	var err error
	out := captureStdout(t, func() { err = RunProvisioner() })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "nothing to do") {
		t.Errorf("stdout %q missing steady-state note", out)
	}
	if len(*payloads) != 0 || len(store.writes) != 0 || len(*gh) != 0 {
		t.Errorf("steady state must not create/write/publish: robots=%v bao=%v gh=%v",
			*payloads, store.writes, *gh)
	}
}

// An OpenBao read failure is not an unseeded path. robotsSeeded used to fold
// the error into false, and the 409 branch then told the operator to delete a
// robot whose credentials were intact in OpenBao — turning a recoverable blip
// into a destroyed credential. The read failure must stop the tick instead.
func TestHarborProvisionerUnreadableBaoDoesNotAdviseDeletingTheRobot(t *testing.T) {
	srv, payloads := harborStub(t, http.StatusConflict, nil) // the robot already exists
	store := &fakeBaoStore{
		data:   map[string]map[string]string{},
		getErr: errors.New("Error making API request: 503 Service Unavailable"),
	}
	gh := setProvisionerEnv(t, "adminpass", store)
	t.Setenv("HARBOR_API_URL", srv.URL)

	var err error
	out := captureStdout(t, func() { err = RunProvisioner() })
	if err == nil {
		t.Fatal("an unreadable OpenBao must fail the tick, not fall through to the create path")
	}
	if !strings.Contains(err.Error(), "cannot tell") {
		t.Errorf("the error should name the ambiguity it refuses to resolve: %v", err)
	}
	if strings.Contains(out, "delete the robot") {
		t.Error("advised deleting a live robot on the strength of a failed read")
	}
	if len(*payloads) != 0 || len(store.writes) != 0 || len(*gh) != 0 {
		t.Errorf("nothing should have been created/written/published: robots=%v bao=%v gh=%v",
			*payloads, store.writes, *gh)
	}
}

func TestHarborProvisionerSteadySmoke401IsFatal(t *testing.T) {
	srv := httptestNewSmoke401(t)
	store := &fakeBaoStore{data: map[string]map[string]string{
		"secret/harbor/robot":      {"username": "robot$stale", "password": "old"},
		"secret/harbor/pull-robot": {"username": "robot$pull", "password": "p"},
	}}
	setProvisionerEnv(t, "adminpass", store)
	t.Setenv("HARBOR_API_URL", srv.URL)

	err := RunProvisioner()
	if err == nil || !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "robot$stale") {
		t.Errorf("err = %v, want a loud 401 naming the stale robot", err)
	}
}

func TestHarborProvisionerUnreachableHarborRetriesNextTick(t *testing.T) {
	store := &fakeBaoStore{}
	setProvisionerEnv(t, "adminpass", store)
	t.Setenv("HARBOR_API_URL", "http://127.0.0.1:1") // nothing listens

	var err error
	out := captureStdout(t, func() { err = RunProvisioner() })
	if err != nil {
		t.Fatalf("unreachable Harbor must defer to the next tick, got %v", err)
	}
	if !strings.Contains(out, "retrying next tick") {
		t.Errorf("stdout %q missing the retry note", out)
	}
}

func TestHarborProvisionerProjectCreateFatal(t *testing.T) {
	srv, _ := harborStub(t, http.StatusInternalServerError, nil)
	store := &fakeBaoStore{}
	setProvisionerEnv(t, "adminpass", store)
	t.Setenv("HARBOR_API_URL", srv.URL)

	if err := RunProvisioner(); err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("err = %v, want fatal project-create failure", err)
	}
}

func TestHarborProvisionerCreatesBothSeedsAndPublishes(t *testing.T) {
	srv, payloads := harborStub(t, http.StatusCreated, []int{http.StatusCreated, http.StatusCreated})
	store := &fakeBaoStore{}
	gh := setProvisionerEnv(t, "adminpass", store)
	t.Setenv("HARBOR_API_URL", srv.URL)

	if err := RunProvisioner(); err != nil {
		t.Fatal(err)
	}

	// Payload contract preserved from the CI provisioner: never-expiring system
	// robots scoped to project platform.
	if len(*payloads) != 2 {
		t.Fatalf("robot creates = %d, want 2", len(*payloads))
	}
	for i, wantActions := range [][]string{{"push", "pull", "delete"}, {"pull"}} {
		p := (*payloads)[i]
		if p.Duration != -1 || p.Level != "system" {
			t.Errorf("robot %s: duration=%d level=%s, want -1/system", p.Name, p.Duration, p.Level)
		}
		var actions []string
		for _, a := range p.Permissions[0].Access {
			actions = append(actions, a.Action)
		}
		if strings.Join(actions, ",") != strings.Join(wantActions, ",") {
			t.Errorf("robot %s actions = %v, want %v", p.Name, actions, wantActions)
		}
	}

	wantBao := []string{
		"secret/harbor/robot username=robot$ci-firewall-controller password=sec-ci-firewall-controller registry_host=harbor.env.internal",
		"secret/harbor/pull-robot username=robot$pull-platform password=sec-pull-platform registry_host=harbor.env.internal",
	}
	if strings.Join(store.writes, " | ") != strings.Join(wantBao, " | ") {
		t.Errorf("bao writes = %v, want %v", store.writes, wantBao)
	}
	wantGH := []string{
		"HARBOR_ROBOT_NAME=robot$ci-firewall-controller",
		"HARBOR_PASSWORD=sec-ci-firewall-controller",
		"HARBOR_PULL_ROBOT_NAME=robot$pull-platform",
		"HARBOR_PULL_PASSWORD=sec-pull-platform",
	}
	if strings.Join(*gh, " | ") != strings.Join(wantGH, " | ") {
		t.Errorf("gh publications = %v, want %v", *gh, wantGH)
	}
}

// TestHarborProvisionerIgnoresUnusableHarborHost is the regression for the bug that
// shipped to every Managed App Platform instance: `llz render` baked
// harbor.<domainSuffix> with an EMPTY domainSuffix, so HARBOR_HOST arrived as the
// bare prefix "harbor." — non-empty, so it sailed past the discovery fallback and
// was seeded as registry_host. Every docker credential then authenticated for a
// registry literally named "harbor.", matched nothing, and pushes 401'd with an
// error that reads like bad credentials.
//
// The render side now emits "" (clusterspec.HarborHost), but instances rendered
// before that fix carry "harbor." in a COMMITTED artifact, so this guard is what
// heals them without a re-render. Same for the un-patched base's REPLACE_ME.
func TestHarborProvisionerIgnoresUnusableHarborHost(t *testing.T) {
	for _, bad := range []string{"harbor.", "REPLACE_ME", ""} {
		t.Run("host="+bad, func(t *testing.T) {
			srv, _ := harborStub(t, http.StatusCreated, []int{http.StatusCreated, http.StatusCreated})
			store := &fakeBaoStore{}
			setProvisionerEnv(t, "adminpass", store)
			t.Setenv("HARBOR_API_URL", srv.URL)
			t.Setenv("HARBOR_HOST", bad)

			if err := RunProvisioner(); err != nil {
				t.Fatal(err)
			}
			for _, w := range store.writes {
				if !strings.Contains(w, "registry_host=harbor.lke635371.akamai-apl.net") {
					t.Errorf("seeded %q — an unusable HARBOR_HOST %q must be discarded in favour of Harbor's own systeminfo host", w, bad)
				}
			}
			if len(store.writes) != 2 {
				t.Errorf("bao writes = %d, want 2", len(store.writes))
			}
		})
	}
}

// TestHarborProvisionerRepairsSeededRegistryHost is the case the sibling test
// above MISSES, and it is the one that matters in the field.
//
// Every managed instance that rendered before the HarborHost fix has already
// seeded registry_host="harbor." — so it takes the STEADY-STATE branch (robots
// present, smoke passes) and returns before the resolution block ever runs. The
// unusable value would be permanent, and the tick would report healthy the whole
// time: the smoke authenticates username/password, which are valid. Only the
// docker config derived from registry_host is wrong.
//
// The seeded credentials must survive the repair untouched — a KV v2 write
// replaces the whole secret.
func TestHarborProvisionerRepairsSeededRegistryHost(t *testing.T) {
	srv, payloads := harborStub(t, http.StatusCreated, nil)
	store := &fakeBaoStore{data: map[string]map[string]string{
		"secret/harbor/robot":      {"username": "robot$ci-firewall-controller", "password": "sec", "registry_host": "harbor."},
		"secret/harbor/pull-robot": {"username": "robot$pull-platform", "password": "psec", "registry_host": "harbor."},
	}}
	setProvisionerEnv(t, "adminpass", store)
	t.Setenv("HARBOR_API_URL", srv.URL)
	t.Setenv("HARBOR_HOST", "harbor.") // the rendered value is equally unusable

	if err := RunProvisioner(); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"secret/harbor/robot username=robot$ci-firewall-controller password=sec registry_host=harbor.lke635371.akamai-apl.net",
		"secret/harbor/pull-robot username=robot$pull-platform password=psec registry_host=harbor.lke635371.akamai-apl.net",
	}
	if strings.Join(store.writes, " | ") != strings.Join(want, " | ") {
		t.Errorf("repair writes = %v,\nwant %v", store.writes, want)
	}
	// Repair must not touch Harbor's robot API — the credentials are fine.
	if len(*payloads) != 0 {
		t.Errorf("repair must not create robots, got %v", *payloads)
	}
}

// A stored registry_host that is merely DIFFERENT from what discovery reports is
// left alone: an operator may have pointed this cluster somewhere on purpose, and
// a convergence loop that overwrites them every tick is worse than the bug it fixes.
func TestHarborProvisionerLeavesADeliberateRegistryHostAlone(t *testing.T) {
	srv, _ := harborStub(t, http.StatusCreated, nil)
	store := &fakeBaoStore{data: map[string]map[string]string{
		"secret/harbor/robot":      {"username": "u", "password": "p", "registry_host": "registry.corp.example"},
		"secret/harbor/pull-robot": {"username": "pu", "password": "pp", "registry_host": "registry.corp.example"},
	}}
	setProvisionerEnv(t, "adminpass", store)
	t.Setenv("HARBOR_API_URL", srv.URL)

	if err := RunProvisioner(); err != nil {
		t.Fatal(err)
	}
	if len(store.writes) != 0 {
		t.Errorf("a usable registry_host must never be rewritten, got %v", store.writes)
	}
}

// Repairing must never cost a credential. A path whose username/password cannot be
// read back would be rewritten with empty strings — a KV v2 write replaces the
// secret — destroying a robot login Harbor will not re-reveal. Skip it instead.
func TestHarborProvisionerRefusesRepairThatWouldDropCredentials(t *testing.T) {
	srv, _ := harborStub(t, http.StatusCreated, nil)
	store := &fakeBaoStore{data: map[string]map[string]string{
		// robotsSeeded only requires pull-robot's USERNAME, so a password-less
		// pull-robot still reaches the steady-state branch.
		"secret/harbor/robot":      {"username": "u", "password": "p", "registry_host": "harbor."},
		"secret/harbor/pull-robot": {"username": "pu", "registry_host": "harbor."},
	}}
	setProvisionerEnv(t, "adminpass", store)
	t.Setenv("HARBOR_API_URL", srv.URL)
	t.Setenv("HARBOR_HOST", "harbor.") // unusable → repair resolves via systeminfo

	if err := RunProvisioner(); err != nil {
		t.Fatal(err)
	}
	for _, w := range store.writes {
		if strings.Contains(w, "pull-robot") {
			t.Errorf("pull-robot has no password — rewriting it drops the credential: %q", w)
		}
		if strings.Contains(w, "password= ") || strings.HasSuffix(w, "password=") {
			t.Errorf("empty credential written: %q", w)
		}
	}
	// The healthy sibling is still repaired — one bad path must not block the other.
	if len(store.writes) != 1 || !strings.Contains(store.writes[0], "secret/harbor/robot username=u password=p registry_host=harbor.lke635371") {
		t.Errorf("the readable path should still be repaired, got %v", store.writes)
	}
}

// A usable HARBOR_HOST still wins outright — discovery is the fallback, not the
// default. Guards against the fix above over-reaching into the self-install path,
// where the rendered host is authoritative and Harbor may not even be reachable yet.
func TestHarborProvisionerUsableHarborHostWinsOverDiscovery(t *testing.T) {
	srv, _ := harborStub(t, http.StatusCreated, []int{http.StatusCreated, http.StatusCreated})
	store := &fakeBaoStore{}
	setProvisionerEnv(t, "adminpass", store) // pins HARBOR_HOST=harbor.env.internal
	t.Setenv("HARBOR_API_URL", srv.URL)

	if err := RunProvisioner(); err != nil {
		t.Fatal(err)
	}
	for _, w := range store.writes {
		if !strings.Contains(w, "registry_host=harbor.env.internal") {
			t.Errorf("seeded %q — a usable HARBOR_HOST must not be replaced by discovery", w)
		}
	}
}

func TestHarborProvisionerExistingUnseededRobotWarnsAndContinues(t *testing.T) {
	srv, _ := harborStub(t, http.StatusConflict, []int{http.StatusConflict, http.StatusCreated})
	store := &fakeBaoStore{}
	gh := setProvisionerEnv(t, "adminpass", store)
	t.Setenv("HARBOR_API_URL", srv.URL)

	if err := RunProvisioner(); err != nil {
		t.Fatal(err)
	}
	// Push robot 409-unseeded → warned, skipped; pull robot still provisioned.
	wantBao := []string{
		"secret/harbor/pull-robot username=robot$pull-platform password=sec-pull-platform registry_host=harbor.env.internal",
	}
	if strings.Join(store.writes, " | ") != strings.Join(wantBao, " | ") {
		t.Errorf("bao writes = %v, want only the pull robot: %v", store.writes, wantBao)
	}
	if len(*gh) != 2 || !strings.HasPrefix((*gh)[0], "HARBOR_PULL_ROBOT_NAME=") {
		t.Errorf("gh publications = %v, want only the pull pair", *gh)
	}
}

func TestHarborProvisionerWithoutGHTokenSkipsPublication(t *testing.T) {
	srv, _ := harborStub(t, http.StatusCreated, []int{http.StatusCreated, http.StatusCreated})
	store := &fakeBaoStore{}
	gh := setProvisionerEnv(t, "adminpass", store)
	t.Setenv("HARBOR_API_URL", srv.URL)
	t.Setenv("GH_TOKEN", "")

	if err := RunProvisioner(); err != nil {
		t.Fatal(err)
	}
	if len(store.writes) != 2 {
		t.Errorf("bao writes = %v, want both robots seeded", store.writes)
	}
	if len(*gh) != 0 {
		t.Errorf("gh publications = %v, want none without GH_TOKEN", *gh)
	}
}

func TestHarborProvisionerBaoWriteFailureIsFatal(t *testing.T) {
	srv, _ := harborStub(t, http.StatusCreated, []int{http.StatusCreated})
	store := &fakeBaoStore{writeErr: errors.New("permission denied")}
	setProvisionerEnv(t, "adminpass", store)
	t.Setenv("HARBOR_API_URL", srv.URL)

	if err := RunProvisioner(); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("err = %v, want the OpenBao write failure surfaced", err)
	}
}

func TestHarborProvisionerSteadyStateRepublishesMissingGHSecrets(t *testing.T) {
	srv, payloads := harborStub(t, http.StatusCreated, nil)
	store := &fakeBaoStore{data: map[string]map[string]string{
		"secret/harbor/robot":      {"username": "robot$ci-firewall-controller", "password": "sec"},
		"secret/harbor/pull-robot": {"username": "robot$pull-platform", "password": "psec"},
	}}
	gh := setProvisionerEnv(t, "adminpass", store)
	t.Setenv("HARBOR_API_URL", srv.URL)
	// HARBOR_PASSWORD lost (failed publish after seed, or deleted) — everything
	// else published.
	ghRepoSecretExists = func(name string) (bool, error) { return name != "HARBOR_PASSWORD", nil }

	if err := RunProvisioner(); err != nil {
		t.Fatal(err)
	}
	if len(*payloads) != 0 {
		t.Errorf("republish must not touch Harbor: %v", *payloads)
	}
	if strings.Join(*gh, ",") != "HARBOR_PASSWORD=sec" {
		t.Errorf("gh publications = %v, want only HARBOR_PASSWORD re-published from OpenBao", *gh)
	}
}
