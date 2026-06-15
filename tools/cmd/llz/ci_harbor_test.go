package main

import (
	"encoding/base64"
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

// harborEnvVars is the command's full env contract; setHarborEnv pins every
// one (empty unless the test overrides) so ambient CI values can't leak in.
var harborEnvVars = []string{
	"REGION", "HA_ROLE", "HARBOR_URL", "HARBOR_API_URL",
	"EXISTING_ROBOT", "EXISTING_SECRET", "EXISTING_PULL_ROBOT", "EXISTING_PULL_SECRET",
	"OPENBAO_ROOT_TOKEN",
}

func setHarborEnv(t *testing.T, vars map[string]string) (summaryPath string) {
	t.Helper()
	summaryPath = filepath.Join(t.TempDir(), "summary")
	t.Setenv("GITHUB_STEP_SUMMARY", summaryPath)
	t.Setenv("GITHUB_ACTIONS", "1") // maskGHA emits ::add-mask:: like the script's CI runs
	for _, v := range harborEnvVars {
		t.Setenv(v, vars[v])
	}
	return summaryPath
}

func readSummary(t *testing.T, path string) string {
	t.Helper()
	b, _ := os.ReadFile(path)
	return string(b)
}

// withHarborSeams swaps the OpenBao and gh-repo-secret seams, recording calls.
func withHarborSeams(t *testing.T) (bao, gh *[]string) {
	t.Helper()
	origBao, origGH := baoKVPutFn, ghSetRepoSecretFn
	bao, gh = new([]string), new([]string)
	baoKVPutFn = func(path string, fields map[string]string) error {
		*bao = append(*bao, fmt.Sprintf("%s username=%s password=%s registry_host=%s",
			path, fields["username"], fields["password"], fields["registry_host"]))
		return nil
	}
	ghSetRepoSecretFn = func(name, value string) error {
		*gh = append(*gh, name+"="+value)
		return nil
	}
	t.Cleanup(func() { baoKVPutFn, ghSetRepoSecretFn = origBao, origGH })
	return bao, gh
}

// withHarborAdminSecret stubs the kubectl admin-password read.
func withHarborAdminSecret(t *testing.T, pass string, fail bool) {
	t.Helper()
	orig := execOutput
	execOutput = func(name string, args ...string) ([]byte, error) {
		if name != "kubectl" || !strings.Contains(strings.Join(args, " "), "harbor-admin-password") {
			t.Errorf("unexpected exec: %s %v", name, args)
		}
		if fail {
			return nil, errors.New("exit status 1")
		}
		return []byte(base64.StdEncoding.EncodeToString([]byte(pass))), nil
	}
	t.Cleanup(func() { execOutput = orig })
}

// ── secondary branch ──────────────────────────────────────────────────────────

func TestProvisionHarborRobotsSecondarySkipsWithoutPrimarySecrets(t *testing.T) {
	cases := map[string]map[string]string{
		"neither set": {"HA_ROLE": "standby"},
		"robot only":  {"HA_ROLE": "standby", "EXISTING_ROBOT": "robot$x"},
		"secret only": {"HA_ROLE": "standby", "EXISTING_SECRET": "hush"},
	}
	for name, vars := range cases {
		t.Run(name, func(t *testing.T) {
			summary := setHarborEnv(t, vars)
			bao, gh := withHarborSeams(t)
			if err := runCIProvisionHarborRobots(); err != nil {
				t.Fatalf("want graceful skip, got %v", err)
			}
			got := readSummary(t, summary)
			for _, want := range []string{
				"HARBOR_ROBOT_NAME / HARBOR_PASSWORD not yet set — run primary bootstrap first.",
				"Re-run this workflow after primary bootstrap completes.",
			} {
				if !strings.Contains(got, want) {
					t.Errorf("summary %q missing %q", got, want)
				}
			}
			if len(*bao) != 0 || len(*gh) != 0 {
				t.Errorf("skip must not write anywhere: bao=%v gh=%v", *bao, *gh)
			}
		})
	}
}

func TestProvisionHarborRobotsSecondarySeedsRobotThenSkipsPull(t *testing.T) {
	summary := setHarborEnv(t, map[string]string{
		"HA_ROLE":             "standby",
		"HARBOR_URL":          "https://harbor.env.internal",
		"EXISTING_ROBOT":      "robot$ci-firewall-controller",
		"EXISTING_SECRET":     "push-secret",
		"EXISTING_PULL_ROBOT": "robot$pull-platform", // pull SECRET missing
	})
	bao, gh := withHarborSeams(t)
	var err error
	out := captureStdout(t, func() { err = runCIProvisionHarborRobots() })
	if err != nil {
		t.Fatalf("want graceful skip after seeding the push robot, got %v", err)
	}
	want := []string{"secret/harbor/robot username=robot$ci-firewall-controller password=push-secret registry_host=harbor.env.internal"}
	if strings.Join(*bao, " | ") != strings.Join(want, " | ") {
		t.Errorf("bao calls = %v, want %v", *bao, want)
	}
	if !strings.Contains(readSummary(t, summary), "HARBOR_PULL_ROBOT_NAME / HARBOR_PULL_PASSWORD not set — run primary bootstrap first.") {
		t.Error("summary missing the pull-pair skip note")
	}
	if !strings.Contains(out, "::add-mask::push-secret") {
		t.Errorf("stdout %q missing mask of the existing secret", out)
	}
	if len(*gh) != 0 {
		t.Errorf("secondary must not touch gh secrets: %v", *gh)
	}
}

func TestProvisionHarborRobotsSecondarySeedsBoth(t *testing.T) {
	summary := setHarborEnv(t, map[string]string{
		"HA_ROLE":              "standby",
		"HARBOR_URL":           "http://harbor.env.internal", // http:// also stripped
		"EXISTING_ROBOT":       "robot$ci-firewall-controller",
		"EXISTING_SECRET":      "push-secret",
		"EXISTING_PULL_ROBOT":  "robot$pull-platform",
		"EXISTING_PULL_SECRET": "pull-secret",
	})
	bao, _ := withHarborSeams(t)
	var err error
	out := captureStdout(t, func() { err = runCIProvisionHarborRobots() })
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"secret/harbor/robot username=robot$ci-firewall-controller password=push-secret registry_host=harbor.env.internal",
		"secret/harbor/pull-robot username=robot$pull-platform password=pull-secret registry_host=harbor.env.internal",
	}
	if strings.Join(*bao, " | ") != strings.Join(want, " | ") {
		t.Errorf("bao calls = %v, want %v", *bao, want)
	}
	if !strings.Contains(out, "secret/harbor/robot and secret/harbor/pull-robot seeded on the standby peer.") {
		t.Errorf("stdout %q missing the seeded confirmation", out)
	}
	for _, mask := range []string{"::add-mask::push-secret", "::add-mask::pull-secret"} {
		if !strings.Contains(out, mask) {
			t.Errorf("stdout %q missing %q", out, mask)
		}
	}
	if got := readSummary(t, summary); got != "" {
		t.Errorf("full seed must not write summary notes, got %q", got)
	}
}

// ── primary graceful skips ────────────────────────────────────────────────────

func TestProvisionHarborRobotsPrimarySkipsWithoutHarborURL(t *testing.T) {
	summary := setHarborEnv(t, map[string]string{"REGION": "primary"})
	bao, _ := withHarborSeams(t)
	if err := runCIProvisionHarborRobots(); err != nil {
		t.Fatalf("want graceful skip, got %v", err)
	}
	if !strings.Contains(readSummary(t, summary), "HARBOR_URL variable not set — skipping Harbor robot account provisioning.") {
		t.Error("summary missing the HARBOR_URL skip note")
	}
	if len(*bao) != 0 {
		t.Errorf("skip must not seed OpenBao: %v", *bao)
	}
}

func TestProvisionHarborRobotsPrimarySkipsWithoutAdminSecret(t *testing.T) {
	summary := setHarborEnv(t, map[string]string{
		"REGION": "primary", "HARBOR_URL": "https://harbor.env.internal",
	})
	withHarborAdminSecret(t, "", true)
	bao, _ := withHarborSeams(t)
	if err := runCIProvisionHarborRobots(); err != nil {
		t.Fatalf("want graceful skip, got %v", err)
	}
	got := readSummary(t, summary)
	for _, want := range []string{
		"harbor/harbor-admin-password Secret not found — Harbor not yet deployed.",
		"Re-run this workflow after Harbor is up to provision robot accounts.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary %q missing %q", got, want)
		}
	}
	if len(*bao) != 0 {
		t.Errorf("skip must not seed OpenBao: %v", *bao)
	}
}

func TestProvisionHarborRobotsPrimaryUnreachableHarborSkips(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close() // connection refused = the curl-000 case
	summary := setHarborEnv(t, map[string]string{
		"REGION": "primary", "HARBOR_URL": "https://harbor.env.internal", "HARBOR_API_URL": srv.URL,
	})
	withHarborAdminSecret(t, "adminpass", false)
	bao, gh := withHarborSeams(t)
	if err := runCIProvisionHarborRobots(); err != nil {
		t.Fatalf("unreachable Harbor must skip, got %v", err)
	}
	if want := "Harbor not reachable at `" + srv.URL + "` — robot provisioning skipped."; !strings.Contains(readSummary(t, summary), want) {
		t.Errorf("summary missing %q", want)
	}
	if len(*bao) != 0 || len(*gh) != 0 {
		t.Errorf("skip must not write anywhere: bao=%v gh=%v", *bao, *gh)
	}
}

// A non-primary, non-secondary region that hosts Harbor (e.g. e2e) must take the
// provision path, NOT the secondary copy-from-GitHub-secrets path.
func TestProvisionHarborRobotsE2EProvisionsLikePrimary(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close() // connection refused = the curl-000 case → primary "unreachable" skip
	summary := setHarborEnv(t, map[string]string{
		"REGION": "e2e", "HARBOR_URL": "https://harbor.env.internal", "HARBOR_API_URL": srv.URL,
	})
	withHarborAdminSecret(t, "adminpass", false)
	bao, gh := withHarborSeams(t)
	if err := runCIProvisionHarborRobots(); err != nil {
		t.Fatalf("e2e must take the provision path, got %v", err)
	}
	s := readSummary(t, summary)
	if !strings.Contains(s, "Harbor not reachable at `"+srv.URL+"` — robot provisioning skipped.") {
		t.Errorf("e2e took the wrong branch (expected the primary provision path); summary=%q", s)
	}
	if strings.Contains(s, "run primary bootstrap first") {
		t.Error("e2e must not take the secondary copy-from-GitHub-secrets path")
	}
	if len(*bao) != 0 || len(*gh) != 0 {
		t.Errorf("unreachable Harbor must write nothing: bao=%v gh=%v", *bao, *gh)
	}
}

// ── primary fatal paths ───────────────────────────────────────────────────────

func TestProvisionHarborRobotsPrimaryProjectCreateFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"errors":[{"code":"FORBIDDEN"}]}`, http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	setHarborEnv(t, map[string]string{
		"REGION": "primary", "HARBOR_URL": "https://harbor.env.internal", "HARBOR_API_URL": srv.URL,
	})
	withHarborAdminSecret(t, "adminpass", false)
	bao, _ := withHarborSeams(t)
	err := runCIProvisionHarborRobots()
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Errorf("err = %v, want project-create HTTP 403 failure", err)
	}
	if len(*bao) != 0 {
		t.Errorf("fatal project create must not seed OpenBao: %v", *bao)
	}
}

func TestProvisionHarborRobotsPrimaryRobotCreateFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/projects") {
			w.WriteHeader(http.StatusCreated)
			return
		}
		// The HTTP 400 the missing-duration regression produced (see payload comment).
		http.Error(w, `{"errors":[{"code":"BAD_REQUEST","message":"duration must be either -1(Never) or a positive integer"}]}`, http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)
	setHarborEnv(t, map[string]string{
		"REGION": "primary", "HARBOR_URL": "https://harbor.env.internal", "HARBOR_API_URL": srv.URL,
	})
	withHarborAdminSecret(t, "adminpass", false)
	bao, gh := withHarborSeams(t)
	err := runCIProvisionHarborRobots()
	if err == nil || !strings.Contains(err.Error(), "HTTP 400") {
		t.Errorf("err = %v, want robot-create HTTP 400 failure", err)
	}
	if len(*bao) != 0 || len(*gh) != 0 {
		t.Errorf("fatal robot create must not write anywhere: bao=%v gh=%v", *bao, *gh)
	}
}

func TestProvisionHarborRobotsPrimaryRobotTransportErrorFatal(t *testing.T) {
	// A transport failure AFTER the project ensure is the script's 000 status
	// falling through create_harbor_robot's != 201 branch: fatal, not a skip.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/projects") {
			w.WriteHeader(http.StatusCreated)
			return
		}
		panic(http.ErrAbortHandler) // drop the connection mid-request
	}))
	t.Cleanup(srv.Close)
	setHarborEnv(t, map[string]string{
		"REGION": "primary", "HARBOR_URL": "https://harbor.env.internal", "HARBOR_API_URL": srv.URL,
	})
	withHarborAdminSecret(t, "adminpass", false)
	bao, _ := withHarborSeams(t)
	err := runCIProvisionHarborRobots()
	if err == nil || !strings.Contains(err.Error(), "ci-firewall-controller creation failed") {
		t.Errorf("err = %v, want fatal robot-create transport failure", err)
	}
	if len(*bao) != 0 {
		t.Errorf("fatal robot create must not seed OpenBao: %v", *bao)
	}
}

// ── primary happy + 409 paths ─────────────────────────────────────────────────

// harborStub plays the Harbor API: records robot payloads, answers /projects
// with projectStatus and /robots from robotStatuses in call order.
func harborStub(t *testing.T, projectStatus int, robotStatuses []int) (*httptest.Server, *[]harborRobotPayload) {
	t.Helper()
	payloads := new([]harborRobotPayload)
	robotCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, pass, ok := r.BasicAuth(); !ok || user != "admin" || pass != "adminpass" {
			t.Errorf("bad basic auth: %s %s", user, pass)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q", ct)
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/api/v2.0/projects"):
			w.WriteHeader(projectStatus)
		case strings.HasSuffix(r.URL.Path, "/api/v2.0/robots"):
			var p harborRobotPayload
			if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
				t.Errorf("robot payload not JSON: %v", err)
			}
			*payloads = append(*payloads, p)
			status := robotStatuses[robotCalls]
			robotCalls++
			w.WriteHeader(status)
			if status == http.StatusCreated {
				fmt.Fprintf(w, `{"name":"robot$%s","secret":"sec-%s","id":1}`, p.Name, p.Name)
			}
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, payloads
}

func TestProvisionHarborRobotsPrimaryCreatesBoth(t *testing.T) {
	srv, payloads := harborStub(t, http.StatusCreated, []int{http.StatusCreated, http.StatusCreated})
	summary := setHarborEnv(t, map[string]string{
		"REGION": "primary", "HARBOR_URL": "https://harbor.env.internal", "HARBOR_API_URL": srv.URL,
	})
	withHarborAdminSecret(t, "adminpass", false)
	bao, gh := withHarborSeams(t)

	var err error
	out := captureStdout(t, func() { err = runCIProvisionHarborRobots() })
	if err != nil {
		t.Fatal(err)
	}

	// Payload contract: never-expiring system robots scoped to project platform.
	if len(*payloads) != 2 {
		t.Fatalf("robot creates = %d, want 2", len(*payloads))
	}
	for i, wantActions := range [][]string{{"push", "pull", "delete"}, {"pull"}} {
		p := (*payloads)[i]
		if p.Duration != -1 || p.Level != "system" {
			t.Errorf("robot %s: duration=%d level=%s, want -1/system", p.Name, p.Duration, p.Level)
		}
		if len(p.Permissions) != 1 || p.Permissions[0].Kind != "project" || p.Permissions[0].Namespace != "platform" {
			t.Errorf("robot %s permissions = %+v, want one project/platform entry", p.Name, p.Permissions)
		}
		var actions []string
		for _, a := range p.Permissions[0].Access {
			if a.Resource != "repository" {
				t.Errorf("robot %s access resource = %q", p.Name, a.Resource)
			}
			actions = append(actions, a.Action)
		}
		if strings.Join(actions, ",") != strings.Join(wantActions, ",") {
			t.Errorf("robot %s actions = %v, want %v", p.Name, actions, wantActions)
		}
	}
	if (*payloads)[0].Name != "ci-firewall-controller" || (*payloads)[1].Name != "pull-platform" {
		t.Errorf("robot names = %s, %s", (*payloads)[0].Name, (*payloads)[1].Name)
	}

	wantBao := []string{
		"secret/harbor/robot username=robot$ci-firewall-controller password=sec-ci-firewall-controller registry_host=harbor.env.internal",
		"secret/harbor/pull-robot username=robot$pull-platform password=sec-pull-platform registry_host=harbor.env.internal",
	}
	if strings.Join(*bao, " | ") != strings.Join(wantBao, " | ") {
		t.Errorf("bao calls = %v, want %v", *bao, wantBao)
	}
	wantGH := []string{
		"HARBOR_ROBOT_NAME=robot$ci-firewall-controller",
		"HARBOR_PASSWORD=sec-ci-firewall-controller",
		"HARBOR_PULL_ROBOT_NAME=robot$pull-platform",
		"HARBOR_PULL_PASSWORD=sec-pull-platform",
	}
	if strings.Join(*gh, " | ") != strings.Join(wantGH, " | ") {
		t.Errorf("gh calls = %v, want %v", *gh, wantGH)
	}
	for _, want := range []string{
		"Harbor project 'platform' created.",
		"::add-mask::adminpass",
		"::add-mask::sec-ci-firewall-controller",
		"::add-mask::sec-pull-platform",
		"Harbor CI robot account created; HARBOR_ROBOT_NAME and HARBOR_PASSWORD set.",
		"Harbor pull-only robot account created; HARBOR_PULL_ROBOT_NAME and HARBOR_PULL_PASSWORD set.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout %q missing %q", out, want)
		}
	}
	if got := readSummary(t, summary); got != "" {
		t.Errorf("happy path must not write summary notes, got %q", got)
	}
}

func TestProvisionHarborRobotsPrimaryExistingRobotContinues(t *testing.T) {
	srv, _ := harborStub(t, http.StatusConflict, []int{http.StatusConflict, http.StatusCreated})
	summary := setHarborEnv(t, map[string]string{
		"REGION": "primary", "HARBOR_URL": "https://harbor.env.internal", "HARBOR_API_URL": srv.URL,
	})
	withHarborAdminSecret(t, "adminpass", false)
	bao, gh := withHarborSeams(t)

	var err error
	out := captureStdout(t, func() { err = runCIProvisionHarborRobots() })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Harbor project 'platform' already exists.") {
		t.Errorf("stdout %q missing the existing-project note", out)
	}
	got := readSummary(t, summary)
	for _, want := range []string{
		"Harbor robot \"ci-firewall-controller\" already exists — credentials unchanged.",
		"To rotate: delete the robot in Harbor UI and re-run this workflow.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary %q missing %q", got, want)
		}
	}
	// Only the pull robot (the 201) gets seeded; the 409's credentials stay put.
	wantBao := []string{"secret/harbor/pull-robot username=robot$pull-platform password=sec-pull-platform registry_host=harbor.env.internal"}
	if strings.Join(*bao, " | ") != strings.Join(wantBao, " | ") {
		t.Errorf("bao calls = %v, want %v", *bao, wantBao)
	}
	wantGH := []string{"HARBOR_PULL_ROBOT_NAME=robot$pull-platform", "HARBOR_PULL_PASSWORD=sec-pull-platform"}
	if strings.Join(*gh, " | ") != strings.Join(wantGH, " | ") {
		t.Errorf("gh calls = %v, want %v", *gh, wantGH)
	}
}

// ── seed/persist failure propagation ──────────────────────────────────────────

func TestProvisionHarborRobotsSecondaryBaoFailureIsFatal(t *testing.T) {
	setHarborEnv(t, map[string]string{
		"HA_ROLE": "standby", "HARBOR_URL": "https://harbor.env.internal",
		"EXISTING_ROBOT": "r", "EXISTING_SECRET": "s",
	})
	withHarborSeams(t)
	baoKVPutFn = func(string, map[string]string) error { return errors.New("bao kv put: permission denied") }
	if err := runCIProvisionHarborRobots(); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("err = %v, want the bao failure surfaced", err)
	}
}

func TestProvisionHarborRobotsPrimarySeedFailuresAreFatal(t *testing.T) {
	cases := map[string]func(){
		"bao":     func() { baoKVPutFn = func(string, map[string]string) error { return errors.New("boom") } },
		"gh name": func() { ghSetRepoSecretFn = func(name, _ string) error { return errors.New("boom: " + name) } },
		"gh pass": func() {
			ghSetRepoSecretFn = func(name, _ string) error {
				if strings.Contains(name, "PASSWORD") {
					return errors.New("boom: " + name)
				}
				return nil
			}
		},
	}
	for name, breakSeam := range cases {
		t.Run(name, func(t *testing.T) {
			srv, _ := harborStub(t, http.StatusCreated, []int{http.StatusCreated, http.StatusCreated})
			setHarborEnv(t, map[string]string{
				"REGION": "primary", "HARBOR_URL": "https://harbor.env.internal", "HARBOR_API_URL": srv.URL,
			})
			withHarborAdminSecret(t, "adminpass", false)
			withHarborSeams(t)
			breakSeam()
			var err error
			captureStdout(t, func() { err = runCIProvisionHarborRobots() })
			if err == nil || !strings.Contains(err.Error(), "boom") {
				t.Errorf("err = %v, want the %s failure surfaced", err, name)
			}
		})
	}
}

func TestProvisionHarborRobotsPrimaryUnparseableRobotJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		if strings.HasSuffix(r.URL.Path, "/robots") {
			fmt.Fprint(w, "not json")
		}
	}))
	t.Cleanup(srv.Close)
	setHarborEnv(t, map[string]string{
		"REGION": "primary", "HARBOR_URL": "https://harbor.env.internal", "HARBOR_API_URL": srv.URL,
	})
	withHarborAdminSecret(t, "adminpass", false)
	bao, _ := withHarborSeams(t)
	var err error
	captureStdout(t, func() { err = runCIProvisionHarborRobots() })
	if err == nil || !strings.Contains(err.Error(), "unparseable") {
		t.Errorf("err = %v, want unparseable-JSON failure", err)
	}
	if len(*bao) != 0 {
		t.Errorf("unparseable create response must not seed OpenBao: %v", *bao)
	}
}

func TestHarborAdminPasswordBadBase64SkipsGracefully(t *testing.T) {
	summary := setHarborEnv(t, map[string]string{
		"REGION": "primary", "HARBOR_URL": "https://harbor.env.internal",
	})
	orig := execOutput
	execOutput = func(string, ...string) ([]byte, error) { return []byte("%%not-base64%%"), nil }
	t.Cleanup(func() { execOutput = orig })
	withHarborSeams(t)
	if err := runCIProvisionHarborRobots(); err != nil {
		t.Fatalf("undecodable Secret must skip like an absent one, got %v", err)
	}
	if !strings.Contains(readSummary(t, summary), "harbor/harbor-admin-password Secret not found") {
		t.Error("summary missing the admin-Secret skip note")
	}
}

// ── the baoKVPut default implementation ───────────────────────────────────────

func TestBaoKVPutDefaultImpl(t *testing.T) {
	t.Setenv("OPENBAO_ROOT_TOKEN", "s.root")
	var gotPod, gotToken string
	var gotArgs []string
	withBaoExec(t, func(pod, token, stdin string, args ...string) (string, string, error) {
		gotPod, gotToken, gotArgs = pod, token, args
		return "", "", nil
	})
	err := baoKVPutFn("secret/harbor/robot", map[string]string{
		"username": "u", "registry_host": "h", "password": "p",
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotPod != rootOpenbaoPod || gotToken != "s.root" {
		t.Errorf("exec target = %s token=%s, want %s with the root token", gotPod, gotToken, rootOpenbaoPod)
	}
	// Fields ride the in-pod bao argv in deterministic (sorted) order.
	want := "kv put secret/harbor/robot password=p registry_host=h username=u"
	if strings.Join(gotArgs, " ") != want {
		t.Errorf("bao argv = %v, want %q", gotArgs, want)
	}

	withBaoExec(t, func(string, string, string, ...string) (string, string, error) {
		return "", "Code: 403. * permission denied", errors.New("exit status 2")
	})
	err = baoKVPutFn("secret/harbor/robot", map[string]string{"username": "u"})
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("err = %v, want the in-pod stderr surfaced", err)
	}

	t.Setenv("OPENBAO_ROOT_TOKEN", "")
	if err := baoKVPutFn("secret/harbor/robot", nil); err == nil || !strings.Contains(err.Error(), "OPENBAO_ROOT_TOKEN") {
		t.Errorf("err = %v, want missing-token refusal", err)
	}
}

// ── cobra wiring ──────────────────────────────────────────────────────────────

func TestCIProvisionHarborRobotsCmd(t *testing.T) {
	c := ciProvisionHarborRobotsCmd()
	if c.Use != "provision-harbor-robots" {
		t.Errorf("Use = %q", c.Use)
	}
	if !strings.Contains(c.Long, "provision-harbor-robots.sh") {
		t.Error("Long help must name the script it ports")
	}
	// RunE drives the secondary skip path end-to-end.
	summary := setHarborEnv(t, map[string]string{"HA_ROLE": "standby"})
	withHarborSeams(t)
	if err := c.RunE(c, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readSummary(t, summary), "run primary bootstrap first") {
		t.Error("RunE did not reach the secondary skip path")
	}
}
