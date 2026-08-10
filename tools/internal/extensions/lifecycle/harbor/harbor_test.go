package harbor

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/baoread"
)

// harborEnvVars is the standby command's full env contract; setHarborEnv pins
// every one (empty unless the test overrides) so ambient CI values can't leak in.
var harborEnvVars = []string{
	"REGION", "HA_ROLE", "HARBOR_URL", "HARBOR_API_URL",
	"EXISTING_ROBOT", "EXISTING_SECRET", "EXISTING_PULL_ROBOT", "EXISTING_PULL_SECRET",
	"OPENBAO_ROOT_TOKEN",
}

func setHarborEnv(t *testing.T, vars map[string]string) (summaryPath string) {
	t.Helper()
	summaryPath = filepath.Join(t.TempDir(), "summary")
	t.Setenv("GITHUB_STEP_SUMMARY", summaryPath)
	t.Setenv("GITHUB_ACTIONS", "1") // ghsecret.Mask emits ::add-mask:: like the script's CI runs
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

// withStandbySeams swaps the OpenBao root-token seed seam, recording calls.
func withStandbySeams(t *testing.T) (bao *[]string) {
	t.Helper()
	origBao := baoread.KVPut
	bao = new([]string)
	baoread.KVPut = func(path string, fields map[string]string) error {
		*bao = append(*bao, fmt.Sprintf("%s username=%s password=%s registry_host=%s",
			path, fields["username"], fields["password"], fields["registry_host"]))
		return nil
	}
	t.Cleanup(func() { baoread.KVPut = origBao })
	return bao
}

// ── standby: replicate the active's published credentials ────────────────────

func TestSeedStandbyHarborRobotsSkipsWithoutActiveSecrets(t *testing.T) {
	cases := map[string]map[string]string{
		"neither set": {},
		"robot only":  {"EXISTING_ROBOT": "robot$x"},
		"secret only": {"EXISTING_SECRET": "hush"},
	}
	for name, vars := range cases {
		t.Run(name, func(t *testing.T) {
			summary := setHarborEnv(t, vars)
			bao := withStandbySeams(t)
			if err := SeedStandbyRobots("harbor.env.internal"); err != nil {
				t.Fatalf("want graceful skip, got %v", err)
			}
			got := readSummary(t, summary)
			if !strings.Contains(got, "HARBOR_ROBOT_NAME / HARBOR_PASSWORD not yet published") {
				t.Errorf("summary %q missing the not-published note", got)
			}
			if len(*bao) != 0 {
				t.Errorf("skip must not write anywhere: bao=%v", *bao)
			}
		})
	}
}

func TestSeedStandbyHarborRobotsSeedsRobotThenSkipsPull(t *testing.T) {
	summary := setHarborEnv(t, map[string]string{
		"EXISTING_ROBOT":      "robot$ci-firewall-controller",
		"EXISTING_SECRET":     "push-secret",
		"EXISTING_PULL_ROBOT": "robot$pull-platform", // pull SECRET missing
	})
	bao := withStandbySeams(t)
	var err error
	out := captureStdout(t, func() { err = SeedStandbyRobots("harbor.env.internal") })
	if err != nil {
		t.Fatalf("want graceful skip after seeding the push robot, got %v", err)
	}
	want := []string{"secret/harbor/robot username=robot$ci-firewall-controller password=push-secret registry_host=harbor.env.internal"}
	if strings.Join(*bao, " | ") != strings.Join(want, " | ") {
		t.Errorf("bao calls = %v, want %v", *bao, want)
	}
	if !strings.Contains(readSummary(t, summary), "HARBOR_PULL_ROBOT_NAME / HARBOR_PULL_PASSWORD not published") {
		t.Error("summary missing the pull-pair skip note")
	}
	if !strings.Contains(out, "::add-mask::push-secret") {
		t.Errorf("stdout %q missing mask of the existing secret", out)
	}
}

func TestSeedStandbyHarborRobotsBaoFailureIsFatal(t *testing.T) {
	setHarborEnv(t, map[string]string{
		"EXISTING_ROBOT": "r", "EXISTING_SECRET": "s",
	})
	orig := baoread.KVPut
	baoread.KVPut = func(string, map[string]string) error { return errors.New("bao kv put: permission denied") }
	t.Cleanup(func() { baoread.KVPut = orig })
	if err := SeedStandbyRobots("h"); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("err = %v, want the bao failure surfaced", err)
	}
}

// ── harbor REST stub (shared with the provisioner tests) ─────────────────────

// harborStub serves the project-create, robot-create and (for the smoke) the
// project-list endpoints. robotStatuses drives successive robot creates.
func harborStub(t *testing.T, projectStatus int, robotStatuses []int) (*httptest.Server, *[]harborRobotPayload) {
	t.Helper()
	payloads := new([]harborRobotPayload)
	robotCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2.0/projects"):
			// The smoke: any non-admin basic auth is accepted as a valid robot.
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2.0/systeminfo"):
			// Harbor's own view of its external registry host — the ground truth the
			// provisioner falls back to when HARBOR_HOST is absent or unusable.
			// Deliberately unlike the harbor.env.internal that setProvisionerEnv pins,
			// so a test can tell which of the two a seed came from.
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"registry_url":"https://harbor.lke635371.akamai-apl.net"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/api/v2.0/projects"):
			if user, pass, ok := r.BasicAuth(); !ok || user != "admin" || pass != "adminpass" {
				t.Errorf("bad basic auth: %s %s", user, pass)
			}
			w.WriteHeader(projectStatus)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/api/v2.0/robots"):
			var p harborRobotPayload
			if err := jsonDecode(r, &p); err != nil {
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
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, payloads
}

// ── shared GHA-file test helpers (used by ci_bao_seed_test / ci_seed_special_test) ──

// ── the baoKVPut default implementation ───────────────────────────────────────

func TestBaoKVPutDefaultImpl(t *testing.T) {
	t.Setenv("OPENBAO_ROOT_TOKEN", "s.root")
	var gotPod, gotToken string
	var gotArgs []string
	withBaoExec(t, func(pod, token, stdin string, args ...string) (string, string, error) {
		gotPod, gotToken, gotArgs = pod, token, args
		return "", "", nil
	})
	err := baoread.KVPut("secret/harbor/robot", map[string]string{
		"username": "u", "registry_host": "h", "password": "p",
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotPod != baoread.RootPod || gotToken != "s.root" {
		t.Errorf("exec target = %s token=%s, want %s with the root token", gotPod, gotToken, baoread.RootPod)
	}
	// Fields ride the in-pod bao argv in deterministic (sorted) order.
	want := "kv put secret/harbor/robot password=p registry_host=h username=u"
	if strings.Join(gotArgs, " ") != want {
		t.Errorf("bao argv = %v, want %q", gotArgs, want)
	}

	withBaoExec(t, func(string, string, string, ...string) (string, string, error) {
		return "", "Code: 403. * permission denied", errors.New("exit status 2")
	})
	err = baoread.KVPut("secret/harbor/robot", map[string]string{"username": "u"})
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("err = %v, want the in-pod stderr surfaced", err)
	}

	t.Setenv("OPENBAO_ROOT_TOKEN", "")
	if err := baoread.KVPut("secret/harbor/robot", nil); err == nil || !strings.Contains(err.Error(), "OPENBAO_ROOT_TOKEN") {
		t.Errorf("err = %v, want missing-token refusal", err)
	}
}

// TestHarborRegistryHostFromURL covers normalizing Harbor's systeminfo
// registry_url to a bare host — the managed-cluster registry_host discovery.
func TestHarborRegistryHostFromURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"harbor.lke634487.akamai-apl.net", "harbor.lke634487.akamai-apl.net"},
		{"https://harbor.lke634487.akamai-apl.net", "harbor.lke634487.akamai-apl.net"},
		{"https://harbor.example.com/", "harbor.example.com"},
		{"http://harbor.example.com", "harbor.example.com"},
		{"  harbor.example.com  ", "harbor.example.com"},
		{"", ""},
	}
	for _, c := range cases {
		if got := harborRegistryHostFromURL(c.in); got != c.want {
			t.Errorf("harborRegistryHostFromURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
