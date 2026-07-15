package main

// ci_harbor.go — the CI-side remainder of Harbor provisioning, plus the Harbor
// REST plumbing shared with the in-cluster provisioner.
//
// The ACTIVE-path provisioning (ensure `platform` project, create the
// ci-firewall-controller / pull-platform robots, seed OpenBao, publish the
// repo-level HARBOR_* GitHub secrets, smoke) moved IN-CLUSTER: the
// harbor-robot-provisioner CronJob (platform-apl/components/harbor/) runs
// `llz ci harbor-provisioner` (ci_harbor_provisioner.go) on the slim llz
// image. That retired the workflow's port-forward (only needed because
// HARBOR_URL is internal DNS the runner can't resolve), the root-token
// re-acquire via recovery-key quorum (the CronJob writes through a scoped
// Kubernetes-auth role), and a whole cluster-access/ACL cycle.
//
// What stays here is `llz ci seed-standby-harbor-robots` — the STANDBY path.
// A standby peer has no in-cluster Harbor; it replicates the active's robot
// credentials from the repo-level GitHub secrets the active's provisioner
// published (HARBOR_ROBOT_NAME / HARBOR_PASSWORD / HARBOR_PULL_ROBOT_NAME /
// HARBOR_PULL_PASSWORD — the EXISTING_* env). It runs inside the bootstrap
// job while the root token is live, so the OpenBao writes go through the same
// in-pod bao CLI passthrough as the generic seeds.
//
// Env contract (set by the workflow step env: block):
//   HARBOR_URL            — Harbor registry base URL (the ACTIVE's registry —
//                           standby consumers pull from it); used for
//                           registry_host in OpenBao
//   EXISTING_ROBOT        — secrets.HARBOR_ROBOT_NAME
//   EXISTING_SECRET       — secrets.HARBOR_PASSWORD
//   EXISTING_PULL_ROBOT   — secrets.HARBOR_PULL_ROBOT_NAME
//   EXISTING_PULL_SECRET  — secrets.HARBOR_PULL_PASSWORD
//   OPENBAO_ROOT_TOKEN    — root token (consumed by the OpenBao writes)
//   GITHUB_STEP_SUMMARY   — step summary file path (set by GitHub Actions)

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// baoKVPutFn writes one KV path through the in-pod bao CLI — the same kubectl
// exec passthrough as `llz openbao exec kv put …` (which replaced the
// bao-exec.sh the script shelled), run in-process via the baoExecFn seam. The
// field values appear only on the kubectl exec argv that passthrough already
// exposes, never on any other local process argv. Seamed for tests.
var baoKVPutFn = func(path string, fields map[string]string) error {
	token := os.Getenv("OPENBAO_ROOT_TOKEN")
	if token == "" {
		return fmt.Errorf("OPENBAO_ROOT_TOKEN must be set (the OpenBao seed writes run through the in-pod bao CLI)")
	}
	args := []string{"kv", "put", path}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic argv
	for _, k := range keys {
		args = append(args, k+"="+fields[k])
	}
	out, errOut, err := baoExecFn(rootOpenbaoPod, token, "", args...)
	if err != nil {
		return fmt.Errorf("bao kv put %s: %s", path, strings.TrimSpace(firstNonEmpty(errOut, out)))
	}
	return nil
}

func ciSeedStandbyHarborRobotsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "seed-standby-harbor-robots",
		Short: "seed secret/harbor/{robot,pull-robot} on a standby peer from the active's published GitHub secrets",
		Long: "The standby half of Harbor robot provisioning. A standby peer has no\n" +
			"in-cluster Harbor, so it replicates the active's robot credentials from the\n" +
			"repo-level HARBOR_* GitHub secrets the active's in-cluster\n" +
			"harbor-robot-provisioner CronJob published (the EXISTING_* env). Each\n" +
			"not-ready state (active's secrets not published yet) is a step-summary note\n" +
			"+ clean exit so bootstrap can simply re-run. Env: HARBOR_URL,\n" +
			"EXISTING_{ROBOT,SECRET,PULL_ROBOT,PULL_SECRET}, OPENBAO_ROOT_TOKEN.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			harborURL := os.Getenv("HARBOR_URL")
			registryHost := strings.TrimPrefix(strings.TrimPrefix(harborURL, "http://"), "https://")
			return seedStandbyHarborRobots(registryHost)
		},
	}
}

// seedStandbyHarborRobots seeds both robot credentials from the GitHub secrets
// the active's provisioner set. The two pairs gate independently — a re-run
// after only the push robot was provisioned still seeds it before skipping on
// the missing pull pair — and each skip is a summary note + clean exit.
func seedStandbyHarborRobots(registryHost string) error {
	robot, secret := os.Getenv("EXISTING_ROBOT"), os.Getenv("EXISTING_SECRET")
	if robot == "" || secret == "" {
		return appendGHAFile("GITHUB_STEP_SUMMARY",
			"HARBOR_ROBOT_NAME / HARBOR_PASSWORD not yet published — the active peer's harbor-robot-provisioner CronJob sets them once Harbor is up.",
			"Re-run this workflow after the active peer's provisioner has run.")
	}
	maskGHA(secret)
	if err := baoKVPutFn("secret/harbor/robot", map[string]string{
		"username": robot, "password": secret, "registry_host": registryHost,
	}); err != nil {
		return err
	}

	pullRobot, pullSecret := os.Getenv("EXISTING_PULL_ROBOT"), os.Getenv("EXISTING_PULL_SECRET")
	if pullRobot == "" || pullSecret == "" {
		return appendGHAFile("GITHUB_STEP_SUMMARY",
			"HARBOR_PULL_ROBOT_NAME / HARBOR_PULL_PASSWORD not published — re-run after the active peer's provisioner has run.")
	}
	maskGHA(pullSecret)
	if err := baoKVPutFn("secret/harbor/pull-robot", map[string]string{
		"username": pullRobot, "password": pullSecret, "registry_host": registryHost,
	}); err != nil {
		return err
	}

	fmt.Println("secret/harbor/robot and secret/harbor/pull-robot seeded on the standby peer.")
	return nil
}

// ── Harbor REST (shared with ci_harbor_provisioner.go) ───────────────────────

// harborRobotSpec is one robot to provision plus where its credentials land.
type harborRobotSpec struct {
	payload    harborRobotPayload
	kvPath     string // OpenBao path for the credentials
	nameSecret string // repo-level GitHub secret for the robot name
	passSecret string // repo-level GitHub secret for the robot secret
	doneMsg    string
}

type harborAPI struct {
	baseURL   string
	adminPass string
	client    *http.Client
}

// post POSTs a JSON payload with admin basic auth. A non-nil error is a
// transport failure — the equivalent of curl's status 000 (DNS unresolved,
// connection refused) — distinct from any HTTP status code.
func (h *harborAPI) post(path, payload string) (status int, body string, err error) {
	req, err := http.NewRequest(http.MethodPost, h.baseURL+path, strings.NewReader(payload))
	if err != nil {
		return 0, "", err
	}
	req.SetBasicAuth("admin", h.adminPass)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, strings.TrimSpace(string(b)), nil
}

// Robot create payloads MUST include `duration` in Harbor 2.x — `-1` = never
// expires. Without it the API returns HTTP 400 BAD_REQUEST: "duration must be
// either -1(Never) or a positive integer". An earlier version of the script
// omitted the field and the create failed silently behind the workflow's
// continue-on-error, leaving secret/harbor/{robot,pull-robot} unseeded and
// the downstream ExternalSecrets stuck Degraded.
type harborRobotPayload struct {
	Name        string                  `json:"name"`
	Duration    int                     `json:"duration"`
	Level       string                  `json:"level"`
	Permissions []harborRobotPermission `json:"permissions"`
}

type harborRobotPermission struct {
	Kind      string              `json:"kind"`
	Namespace string              `json:"namespace"`
	Access    []harborRobotAccess `json:"access"`
}

type harborRobotAccess struct {
	Resource string `json:"resource"`
	Action   string `json:"action"`
}

// newHarborRobotPayload builds a never-expiring system robot scoped to the
// repository resource of the `platform` project with the given actions.
func newHarborRobotPayload(name string, actions ...string) harborRobotPayload {
	access := make([]harborRobotAccess, 0, len(actions))
	for _, a := range actions {
		access = append(access, harborRobotAccess{Resource: "repository", Action: a})
	}
	return harborRobotPayload{
		Name:     name,
		Duration: -1,
		Level:    "system",
		Permissions: []harborRobotPermission{
			{Kind: "project", Namespace: "platform", Access: access},
		},
	}
}

// createRobot creates one robot account. created=false with a nil error is
// the 409 already-exists case (the caller decides how loud that is); a
// transport failure is fatal.
func (h *harborAPI) createRobot(payload harborRobotPayload) (name, secret string, created bool, err error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return "", "", false, err
	}
	status, respBody, err := h.post("/api/v2.0/robots", string(body))
	if err != nil {
		fmt.Fprintf(os.Stderr, "::error::Harbor robot creation failed (HTTP 000): %v\n", err)
		return "", "", false, fmt.Errorf("harbor robot %s creation failed: %w", payload.Name, err)
	}
	if status == http.StatusConflict {
		return "", "", false, appendGHAFile("GITHUB_STEP_SUMMARY",
			fmt.Sprintf("Harbor robot \"%s\" already exists — credentials unchanged.", payload.Name),
			"To rotate: delete the robot in Harbor UI; the provisioner CronJob recreates it next tick.")
	}
	if status != http.StatusCreated {
		fmt.Fprintf(os.Stderr, "::error::Harbor robot creation failed (HTTP %d): %s\n", status, respBody)
		return "", "", false, fmt.Errorf("harbor robot %s creation failed (HTTP %d)", payload.Name, status)
	}
	var res struct {
		Name   string `json:"name"`
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal([]byte(respBody), &res); err != nil {
		return "", "", false, fmt.Errorf("harbor robot create returned unparseable JSON: %w", err)
	}
	maskGHA(res.Secret)
	return res.Name, res.Secret, true, nil
}
