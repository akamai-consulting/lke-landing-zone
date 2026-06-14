package main

// ci_harbor.go implements `llz ci provision-harbor-robots` — the native port of
// provision-harbor-robots.sh, the bootstrap step that provisions Harbor robot
// accounts and seeds their credentials into OpenBao.
//
// On primary: creates system robots ci-firewall-controller (push/pull/delete)
// and pull-platform (pull-only) in Harbor, seeds OpenBao, and sets the
// repo-level GitHub secrets HARBOR_ROBOT_NAME / HARBOR_PASSWORD /
// HARBOR_PULL_ROBOT_NAME / HARBOR_PULL_PASSWORD. On secondary: seeds
// secret/harbor/robot and secret/harbor/pull-robot from the GitHub secrets
// written by the primary run. Every not-ready-yet state (HARBOR_URL unset,
// admin Secret absent, Harbor unreachable, primary's secrets not written yet)
// is a $GITHUB_STEP_SUMMARY note + clean exit so bootstrap can simply re-run.
//
// Env contract (identical to the script; set by the workflow step env: block):
//   REGION                — primary | secondary
//   HARBOR_URL            — Harbor registry base URL (may be empty on
//                           secondary) — used for registry_host in OpenBao
//   HARBOR_API_URL        — Harbor REST API base URL (set by the workflow's
//                           port-forward step to http://localhost:18080;
//                           falls back to HARBOR_URL if unset for callers
//                           that already run in-cluster)
//   EXISTING_ROBOT        — secrets.HARBOR_ROBOT_NAME
//   EXISTING_SECRET       — secrets.HARBOR_PASSWORD
//   EXISTING_PULL_ROBOT   — secrets.HARBOR_PULL_ROBOT_NAME
//   EXISTING_PULL_SECRET  — secrets.HARBOR_PULL_PASSWORD
//   OPENBAO_ROOT_TOKEN    — root token (consumed by the OpenBao writes)
//   GITHUB_STEP_SUMMARY   — step summary file path (set by GitHub Actions)

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

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

// ghSetRepoSecretFn writes a repo-level secret to the active forge with the
// value piped over stdin (never argv-visible). The repo-level sibling of
// ci_openbao_init.go's ghSetSecretFn (--env), which must stay environment-
// scoped; the Harbor robot credentials are read by region-agnostic workflows.
// The forge resolves auth + repo from its ambient context. Seamed for tests.
var ghSetRepoSecretFn = func(name, value string) error {
	return forgeFn("").SetSecret(bg(), name, value, scopeFor(""))
}

func ciProvisionHarborRobotsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "provision-harbor-robots",
		Short: "create Harbor CI robot accounts and seed their credentials into OpenBao",
		Long: "Native port of provision-harbor-robots.sh. On primary: ensures the `platform`\n" +
			"Harbor project, creates system robots ci-firewall-controller (push/pull/\n" +
			"delete) and pull-platform (pull-only), seeds secret/harbor/{robot,pull-robot}\n" +
			"in OpenBao, and sets the repo-level GitHub secrets HARBOR_ROBOT_NAME /\n" +
			"HARBOR_PASSWORD / HARBOR_PULL_ROBOT_NAME / HARBOR_PULL_PASSWORD. On\n" +
			"secondary: seeds the same OpenBao paths from those secrets (the EXISTING_*\n" +
			"env). Every not-ready-yet state (HARBOR_URL unset, admin Secret absent,\n" +
			"Harbor unreachable, primary's secrets not written yet) is a step-summary\n" +
			"note + exit 0 so bootstrap can re-run. Env: REGION, HARBOR_URL,\n" +
			"HARBOR_API_URL, EXISTING_{ROBOT,SECRET,PULL_ROBOT,PULL_SECRET},\n" +
			"OPENBAO_ROOT_TOKEN, GITHUB_STEP_SUMMARY.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIProvisionHarborRobots() },
	}
}

func runCIProvisionHarborRobots() error {
	// HARBOR_URL (e.g. harbor.<env>.internal) is internal-only DNS not
	// resolvable from the GitHub Actions runner. The workflow's port-forward
	// step exposes the Harbor API on localhost; the API calls here must target
	// it. Fallback to HARBOR_URL preserves the legacy path for callers running
	// in-cluster (where the internal hostname IS resolvable).
	harborURL := os.Getenv("HARBOR_URL")
	apiURL := firstNonEmpty(os.Getenv("HARBOR_API_URL"), harborURL)
	registryHost := strings.TrimPrefix(strings.TrimPrefix(harborURL, "http://"), "https://")

	// Provision robots wherever Harbor is deployed in-cluster — i.e. on an
	// `active` or `standalone` deployment. Only the `standby` peer has no
	// in-cluster Harbor; it replicates the active's credentials from the GitHub
	// secrets the active run published. HA_ROLE is resolved from the cluster
	// tfvars (`llz env role`) by the calling workflow.
	if os.Getenv("HA_ROLE") == roleStandby {
		return seedStandbyHarborRobots(registryHost)
	}
	return provisionLocalHarborRobots(harborURL, apiURL, registryHost)
}

// ── standby: replicate the active's credentials ──────────────────────────────

// seedStandbyHarborRobots seeds both robot credentials from the GitHub secrets
// the active run set. The two pairs gate independently — a re-run after only the
// push robot was provisioned still seeds it before skipping on the missing pull
// pair — and each skip is a summary note + clean exit.
func seedStandbyHarborRobots(registryHost string) error {
	robot, secret := os.Getenv("EXISTING_ROBOT"), os.Getenv("EXISTING_SECRET")
	if robot == "" || secret == "" {
		return appendGHAFile("GITHUB_STEP_SUMMARY",
			"HARBOR_ROBOT_NAME / HARBOR_PASSWORD not yet set — run primary bootstrap first.",
			"Re-run this workflow after primary bootstrap completes.")
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
			"HARBOR_PULL_ROBOT_NAME / HARBOR_PULL_PASSWORD not set — run primary bootstrap first.")
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

// ── primary: create the robot accounts in Harbor ──────────────────────────────

// harborRobotSpec is one robot to provision plus where its credentials land.
type harborRobotSpec struct {
	payload    harborRobotPayload
	kvPath     string // OpenBao path for the credentials
	nameSecret string // repo-level GitHub secret for the robot name
	passSecret string // repo-level GitHub secret for the robot secret
	doneMsg    string
}

func provisionLocalHarborRobots(harborURL, apiURL, registryHost string) error {
	if harborURL == "" {
		return appendGHAFile("GITHUB_STEP_SUMMARY",
			"HARBOR_URL variable not set — skipping Harbor robot account provisioning.")
	}

	// apl-core 5.0.0 installs Harbor in the `harbor` namespace with the admin
	// password in harbor-admin-password.
	adminPass := harborAdminPassword()
	if adminPass == "" {
		return appendGHAFile("GITHUB_STEP_SUMMARY",
			"harbor/harbor-admin-password Secret not found — Harbor not yet deployed.",
			"Re-run this workflow after Harbor is up to provision robot accounts.")
	}
	maskGHA(adminPass)

	// The script's curl ran without -k: default TLS verification (the
	// port-forward target is plain http://localhost:18080 anyway). The 15s
	// timeout bounds the transport-error path the curl-000 handling expects.
	h := &harborAPI{baseURL: apiURL, adminPass: adminPass,
		client: &http.Client{Timeout: 15 * time.Second}}

	proceed, err := h.ensurePlatformProject()
	if err != nil || !proceed {
		return err
	}

	for _, spec := range []harborRobotSpec{
		{
			payload:    newHarborRobotPayload("ci-firewall-controller", "push", "pull", "delete"),
			kvPath:     "secret/harbor/robot",
			nameSecret: "HARBOR_ROBOT_NAME",
			passSecret: "HARBOR_PASSWORD",
			doneMsg:    "Harbor CI robot account created; HARBOR_ROBOT_NAME and HARBOR_PASSWORD set.",
		},
		{
			payload:    newHarborRobotPayload("pull-platform", "pull"),
			kvPath:     "secret/harbor/pull-robot",
			nameSecret: "HARBOR_PULL_ROBOT_NAME",
			passSecret: "HARBOR_PULL_PASSWORD",
			doneMsg:    "Harbor pull-only robot account created; HARBOR_PULL_ROBOT_NAME and HARBOR_PULL_PASSWORD set.",
		},
	} {
		name, secret, created, err := h.createRobot(spec.payload)
		if err != nil {
			return err
		}
		if !created { // 409: summary already notes the credentials are unchanged
			continue
		}
		if err := baoKVPutFn(spec.kvPath, map[string]string{
			"username": name, "password": secret, "registry_host": registryHost,
		}); err != nil {
			return err
		}
		if err := ghSetRepoSecretFn(spec.nameSecret, name); err != nil {
			return err
		}
		if err := ghSetRepoSecretFn(spec.passSecret, secret); err != nil {
			return err
		}
		fmt.Println(spec.doneMsg)
	}
	return nil
}

// harborAdminPassword reads Harbor's admin password from the
// harbor/harbor-admin-password Secret. Empty on any failure (Secret absent,
// bad base64): Harbor isn't deployed yet, which the caller treats as a
// graceful skip — the script's `2>/dev/null | base64 -d || true`.
func harborAdminPassword() string {
	out, err := execOutput("kubectl", "-n", "harbor", "get", "secret", "harbor-admin-password",
		"-o", "jsonpath={.data.HARBOR_ADMIN_PASSWORD}")
	if err != nil {
		return ""
	}
	dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(out)))
	if err != nil {
		return ""
	}
	return string(dec)
}

// ── Harbor REST ───────────────────────────────────────────────────────────────

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

// ensurePlatformProject ensures the `platform` Harbor project exists before
// robot accounts scoped to it are created. Harbor only ships the default
// `library` project; robot creation against a missing project returns HTTP
// 404 "project platform not found", which is what bit the first
// cold-bootstrap attempt.
//
// Status handling:
//
//	201, 409          → success (idempotent).
//	transport error   → curl's 000: Harbor unreachable (DNS unresolved or
//	                    connection refused). Common during early bootstrap
//	                    when *.<env>.internal hostnames are placeholders
//	                    without external DNS entries yet. Warn and signal
//	                    the caller to skip, mirroring the sibling "Ensure
//	                    Harbor platform project exists" workflow step.
//	anything else     → fatal.
//
// Returns proceed=true to continue, proceed=false (nil error) to gracefully
// skip the rest of the provisioning.
func (h *harborAPI) ensurePlatformProject() (proceed bool, err error) {
	status, body, err := h.post("/api/v2.0/projects",
		`{"project_name":"platform","metadata":{"public":"false"}}`)
	if err != nil {
		fmt.Fprintf(os.Stderr, "::warning::Harbor not reachable at %s — skipping robot provisioning. Check that the port-forward step succeeded.\n", h.baseURL)
		return false, appendGHAFile("GITHUB_STEP_SUMMARY",
			fmt.Sprintf("Harbor not reachable at `%s` — robot provisioning skipped.", h.baseURL))
	}
	switch status {
	case http.StatusCreated:
		fmt.Println("Harbor project 'platform' created.")
	case http.StatusConflict:
		fmt.Println("Harbor project 'platform' already exists.")
	default:
		fmt.Fprintf(os.Stderr, "::error::Harbor project create failed (HTTP %d): %s\n", status, body)
		return false, fmt.Errorf("harbor project create failed (HTTP %d)", status)
	}
	return true, nil
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
// the 409 already-exists case (summary noted, caller moves on); a transport
// failure is fatal like the script's status-000 fallthrough to the != 201
// branch.
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
			"To rotate: delete the robot in Harbor UI and re-run this workflow.")
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
