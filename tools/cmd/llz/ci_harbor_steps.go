package main

// ci_harbor_steps.go implements `llz ci harbor-port-forward`,
// `llz ci harbor-ensure-project` and `llz ci harbor-smoke` — native ports of
// the three inline Harbor API steps in llz-bootstrap-openbao.yml. Harbor's
// hostname is internal-only DNS the runner cannot resolve, so the API calls
// tunnel through the kube-apiserver via `kubectl port-forward` and target
// localhost. The Harbor REST plumbing (admin password lookup, status-aware
// POST) is shared with `llz ci provision-harbor-robots` in ci_harbor.go.
//
// These steps deliberately defer failure instead of failing the step: a broken
// project create / 401 robot smoke sets BOOTSTRAP_ERRORS=true in $GITHUB_ENV
// and exits 0, so the remaining non-Harbor seed steps still run and the job's
// final 'Fail on bootstrap errors' gate reports everything at once.

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

// Pid/log file paths shared with the workflow's always() "Stop Harbor API
// port-forward" step — package vars so tests stay out of /tmp.
var (
	harborPFLog = "/tmp/harbor-pf.log"
	harborPFPid = "/tmp/harbor-pf.pid"
)

// flagBootstrapError records a deferred bootstrap failure: an ::error::
// annotation plus BOOTSTRAP_ERRORS=true for the job's final gate.
func flagBootstrapError(format string, a ...any) error {
	fmt.Fprintf(os.Stderr, "::error::"+format+"\n", a...)
	return appendGHAFile("GITHUB_ENV", "BOOTSTRAP_ERRORS=true")
}

func ciHarborPortForwardCmd() *cobra.Command {
	var localPort, timeout int
	c := &cobra.Command{
		Use:   "harbor-port-forward",
		Short: "start a background kubectl port-forward to the Harbor API and wait for it",
		Long: "Native port of the 'Start Harbor API port-forward' bootstrap step. Starts a\n" +
			"detached `kubectl -n harbor port-forward svc/harbor-core <port>:80` (pid\n" +
			"recorded in /tmp/harbor-pf.pid for the always() stop step), waits for the\n" +
			"local listener to answer /api/v2.0/health, and exports HARBOR_API_URL to\n" +
			"$GITHUB_ENV for the project/robot/smoke steps. Only the API REST plane is\n" +
			"tunnelled — never the registry plane (harbor-registry:5000). Skips cleanly\n" +
			"when harbor-core is absent (e.g. secondary has no in-cluster Harbor).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runCIHarborPortForward(localPort, timeout)
		},
	}
	c.Flags().IntVar(&localPort, "local-port", 18080, "local port to forward the Harbor API onto")
	c.Flags().IntVar(&timeout, "timeout", 30, "seconds to wait for the listener to accept connections")
	return c
}

// startHarborPortForward launches the detached port-forward and returns its
// pid. A package var so tests can stub the process spawn.
var startHarborPortForward = func(localPort int) (int, error) {
	log, err := os.Create(harborPFLog)
	if err != nil {
		return 0, err
	}
	defer log.Close()
	cmd := exec.Command("kubectl", "-n", "harbor", "port-forward", "svc/harbor-core",
		fmt.Sprintf("%d:80", localPort))
	cmd.Stdout, cmd.Stderr = log, log
	// New session (the nohup+disown of the bash version): the forward must
	// outlive this llz invocation — the project/robot/smoke steps use it, and
	// the always() stop step kills it by pid.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	return pid, nil
}

func runCIHarborPortForward(localPort, timeout int) error {
	// Run wherever Harbor is deployed (not just 'primary'). Skip gracefully when
	// harbor-core is absent so this never fails a Harbor-less environment.
	if !kExists("-n", "harbor", "get", "svc", "harbor-core") {
		return appendGHAFile("GITHUB_STEP_SUMMARY",
			"harbor-core absent — Harbor not deployed here; skipping port-forward.")
	}
	pid, err := startHarborPortForward(localPort)
	if err != nil {
		return fmt.Errorf("start kubectl port-forward: %w", err)
	}
	if err := os.WriteFile(harborPFPid, []byte(strconv.Itoa(pid)), 0o644); err != nil {
		return err
	}

	apiURL := fmt.Sprintf("http://localhost:%d", localPort)
	client := &http.Client{Timeout: 2 * time.Second}
	ready := waitPoll(time.Duration(timeout)*time.Second, time.Second, func() bool {
		resp, err := client.Get(apiURL + "/api/v2.0/health")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode >= 200 && resp.StatusCode < 300
	})
	if !ready {
		fmt.Fprintf(os.Stderr, "::error::Harbor port-forward did not become reachable within %ds. Log:\n", timeout)
		if log, err := os.ReadFile(harborPFLog); err == nil {
			os.Stderr.Write(log)
		}
		return fmt.Errorf("harbor port-forward not reachable within %ds", timeout)
	}
	fmt.Printf("Harbor port-forward ready on %s (pid=%d).\n", apiURL, pid)
	return appendGHAFile("GITHUB_ENV", "HARBOR_API_URL="+apiURL)
}

func ciHarborEnsureProjectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "harbor-ensure-project",
		Short: "idempotently create the Harbor `platform` project (409 = already exists)",
		Long: "Native port of the 'Ensure Harbor platform project exists' bootstrap step.\n" +
			"Harbor does not auto-create projects on push: `platform` must exist before\n" +
			"the robot accounts scoped to it are created and before the release workflow\n" +
			"pushes images. 201/409 are success; Harbor unreachable is a warning skip\n" +
			"(re-run after Harbor is up); any other status defers failure via\n" +
			"BOOTSTRAP_ERRORS so the remaining seed steps still run. Reads\n" +
			"HARBOR_API_URL (from harbor-port-forward).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIHarborEnsureProject() },
	}
}

func runCIHarborEnsureProject() error {
	apiURL := os.Getenv("HARBOR_API_URL")
	if apiURL == "" {
		return appendGHAFile("GITHUB_STEP_SUMMARY",
			"HARBOR_API_URL not set (port-forward step skipped or failed) — skipping Harbor project creation.")
	}
	adminPass := harborAdminPassword()
	if adminPass == "" {
		return appendGHAFile("GITHUB_STEP_SUMMARY",
			"harbor-admin-password Secret not found — Harbor not yet deployed; skipping project creation.")
	}
	maskGHA(adminPass)

	h := &harborAPI{baseURL: apiURL, adminPass: adminPass,
		client: &http.Client{Timeout: 15 * time.Second}}
	status, body, err := h.post("/api/v2.0/projects",
		`{"project_name":"platform","metadata":{"public":"false"}}`)
	switch {
	case err != nil: // transport failure — curl's 000
		fmt.Fprintf(os.Stderr, "::warning::Harbor not reachable at %s — project creation skipped\n", apiURL)
		return nil
	case status == http.StatusCreated:
		fmt.Println("Harbor project 'platform' created.")
		return nil
	case status == http.StatusConflict:
		fmt.Println("Harbor project 'platform' already exists.")
		return nil
	default:
		return flagBootstrapError("Harbor project creation failed (HTTP %d): %s", status, body)
	}
}

func ciHarborSmokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "harbor-smoke",
		Short: "verify the seeded Harbor robot credentials can authenticate to the API",
		Long: "Native port of the 'Smoke-test Harbor robot credentials' bootstrap step.\n" +
			"Reads secret/harbor/robot back from OpenBao and lists projects with it: a\n" +
			"401 means the seeded credentials are stale (delete the robot in Harbor UI\n" +
			"and re-run) and defers failure via BOOTSTRAP_ERRORS — broken pulls would\n" +
			"otherwise surface much later as ImagePullBackOff. Unseeded secret or\n" +
			"unreachable Harbor skip cleanly. Reads HARBOR_API_URL, OPENBAO_ROOT_TOKEN.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIHarborSmoke() },
	}
}

// baoKVGetField reads one field of a KV path via the in-pod bao CLI, "" on any
// failure (unseeded path, sealed pod) — the bash `|| true`.
func baoKVGetField(path, field string) string {
	token := os.Getenv("OPENBAO_ROOT_TOKEN")
	out, _, err := baoExecFn(rootOpenbaoPod, token, "", "kv", "get", "-field="+field, path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func runCIHarborSmoke() error {
	robot := baoKVGetField("secret/harbor/robot", "username")
	secret := baoKVGetField("secret/harbor/robot", "password")
	if robot == "" || secret == "" {
		fmt.Println("secret/harbor/robot not yet populated — skipping Harbor smoke test.")
		return nil
	}
	apiURL := os.Getenv("HARBOR_API_URL")
	if apiURL == "" {
		fmt.Println("HARBOR_API_URL not set (port-forward step skipped/failed) — skipping Harbor smoke test.")
		return nil
	}
	maskGHA(secret)

	req, err := http.NewRequest(http.MethodGet, apiURL+"/api/v2.0/projects", nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(robot, secret)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil { // transport failure — curl's 000
		fmt.Fprintf(os.Stderr, "::warning::Harbor not reachable at %s — smoke test skipped\n", apiURL)
		return nil
	}
	resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
		fmt.Println("Harbor smoke test passed: robot authenticated successfully.")
		return nil
	case resp.StatusCode == http.StatusUnauthorized:
		return flagBootstrapError("Harbor smoke test FAILED: robot credentials invalid (HTTP 401). Seeded credentials may be stale — delete the robot in Harbor UI and re-run.")
	default:
		fmt.Fprintf(os.Stderr, "::warning::Harbor smoke test returned unexpected HTTP status %d — verify robot account manually\n", resp.StatusCode)
		return nil
	}
}
