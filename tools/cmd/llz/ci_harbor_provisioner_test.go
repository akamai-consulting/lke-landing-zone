package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// jsonDecode is a tiny helper shared with harborStub.
func jsonDecode(r *http.Request, v any) error { return json.NewDecoder(r.Body).Decode(v) }

// fakeBaoStore implements baoStore in memory.
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
	newProvisionerBaoStore = func(context.Context) (baoStore, error) { return store, nil }
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
	newProvisionerBaoStore = func(context.Context) (baoStore, error) {
		t.Error("bao login must not happen before Harbor is deployed")
		return store, nil
	}
	t.Cleanup(func() { newProvisionerBaoStore = origStore })

	var err error
	out := captureStdout(t, func() { err = runCIHarborProvisioner() })
	if err != nil {
		t.Fatalf("missing admin password must be a clean no-op, got %v", err)
	}
	if !strings.Contains(out, "Harbor not deployed yet") {
		t.Errorf("stdout %q missing the not-deployed note", out)
	}
}

func TestHarborProvisionerSteadyStateNoop(t *testing.T) {
	srv, payloads := harborStub(t, http.StatusCreated, nil)
	store := &fakeBaoStore{data: map[string]map[string]string{
		"secret/harbor/robot":      {"username": "robot$ci-firewall-controller", "password": "sec"},
		"secret/harbor/pull-robot": {"username": "robot$pull-platform", "password": "psec"},
	}}
	gh := setProvisionerEnv(t, "adminpass", store)
	t.Setenv("HARBOR_API_URL", srv.URL)

	var err error
	out := captureStdout(t, func() { err = runCIHarborProvisioner() })
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
	out := captureStdout(t, func() { err = runCIHarborProvisioner() })
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

	err := runCIHarborProvisioner()
	if err == nil || !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "robot$stale") {
		t.Errorf("err = %v, want a loud 401 naming the stale robot", err)
	}
}

func TestHarborProvisionerUnreachableHarborRetriesNextTick(t *testing.T) {
	store := &fakeBaoStore{}
	setProvisionerEnv(t, "adminpass", store)
	t.Setenv("HARBOR_API_URL", "http://127.0.0.1:1") // nothing listens

	var err error
	out := captureStdout(t, func() { err = runCIHarborProvisioner() })
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

	if err := runCIHarborProvisioner(); err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("err = %v, want fatal project-create failure", err)
	}
}

func TestHarborProvisionerCreatesBothSeedsAndPublishes(t *testing.T) {
	srv, payloads := harborStub(t, http.StatusCreated, []int{http.StatusCreated, http.StatusCreated})
	store := &fakeBaoStore{}
	gh := setProvisionerEnv(t, "adminpass", store)
	t.Setenv("HARBOR_API_URL", srv.URL)

	if err := runCIHarborProvisioner(); err != nil {
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

func TestHarborProvisionerExistingUnseededRobotWarnsAndContinues(t *testing.T) {
	srv, _ := harborStub(t, http.StatusConflict, []int{http.StatusConflict, http.StatusCreated})
	store := &fakeBaoStore{}
	gh := setProvisionerEnv(t, "adminpass", store)
	t.Setenv("HARBOR_API_URL", srv.URL)

	if err := runCIHarborProvisioner(); err != nil {
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

	if err := runCIHarborProvisioner(); err != nil {
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

	if err := runCIHarborProvisioner(); err == nil || !strings.Contains(err.Error(), "permission denied") {
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

	if err := runCIHarborProvisioner(); err != nil {
		t.Fatal(err)
	}
	if len(*payloads) != 0 {
		t.Errorf("republish must not touch Harbor: %v", *payloads)
	}
	if strings.Join(*gh, ",") != "HARBOR_PASSWORD=sec" {
		t.Errorf("gh publications = %v, want only HARBOR_PASSWORD re-published from OpenBao", *gh)
	}
}

func TestCIHarborProvisionerCmd(t *testing.T) {
	c := ciHarborProvisionerCmd()
	if c.Use != "harbor-provisioner" {
		t.Errorf("Use = %q", c.Use)
	}
	if !strings.Contains(c.Long, "Kubernetes-auth") {
		t.Error("Long help must describe the k8s-auth OpenBao write path")
	}
}

// httptestNewSmoke401 serves 401 to the smoke's project list.
func httptestNewSmoke401(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2.0/projects") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	return srv
}
