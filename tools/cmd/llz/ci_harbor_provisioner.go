package main

// ci_harbor_provisioner.go implements `llz ci harbor-provisioner` — the
// IN-CLUSTER replacement for the retired `harbor` job in
// llz-bootstrap-openbao.yml (port-forward + root-token re-acquire +
// ensure-project + provision-harbor-robots + smoke). Runs as the
// harbor-robot-provisioner CronJob (platform-apl/components/harbor/) on the slim
// distroless llz image, so every I/O path is native Go:
//
//   - Harbor API:   plain HTTP against harbor-core.harbor.svc (no port-forward
//                   — the tunnel only existed because HARBOR_URL is internal
//                   DNS the GitHub runner cannot resolve).
//   - admin pass:   the harbor-admin-password Secret mounted as a file (the
//                   CronJob runs in the harbor namespace; optional mount — a
//                   missing file is "Harbor not deployed yet", a clean no-op).
//   - OpenBao:      Kubernetes-auth login as the harbor-provisioner role
//                   (write-scoped policy on secret/harbor/{robot,pull-robot};
//                   `llz ci bao-configure` owns the role + policy). No root
//                   token, no kubectl exec.
//   - GitHub:       native repo-secret writes (gh_secrets_native.go) with the
//                   dispatch token — the repo-level HARBOR_* secrets are the
//                   distribution channel a standby bootstrap seeds its OpenBao
//                   from (`llz ci seed-standby-harbor-robots`).
//
// The command is a convergence loop body, not a one-shot: every not-ready state
// (admin Secret absent, Harbor unreachable) exits 0 and the CronJob retries next
// tick; the steady state (robots seeded + smoke passes) is a cheap no-op. The
// only hard failure is a 401 smoke — seeded credentials that Harbor rejects —
// which needs an operator (delete the robot in Harbor UI; the next tick
// recreates + re-publishes it, replacing the old "re-run the workflow" runbook).
//
// Env contract (set by the CronJob manifest):
//   HARBOR_API_URL              Harbor REST base (default http://harbor-core.harbor.svc.cluster.local)
//   HARBOR_HOST                 registry host written as registry_host (per-env,
//                               patched by `llz render` from cluster.bootstrap.domainSuffix)
//   HARBOR_ADMIN_PASSWORD_FILE  mounted admin password (default /etc/harbor-admin/HARBOR_ADMIN_PASSWORD)
//   OPENBAO_ADDR / OPENBAO_KUBERNETES_MOUNT / OPENBAO_KUBERNETES_ROLE /
//   SA_TOKEN_FILE / OPENBAO_CA_FILE / OPENBAO_SKIP_VERIFY
//                               OpenBao k8s-auth login (same contract as the
//                               linode-cred-rotator; role defaults to harbor-provisioner)
//   GH_TOKEN / GH_REPO          repo-secret publication; unset → skip with a
//                               warning (single-cluster instances need no
//                               standby distribution)

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// Seams for tests.
var (
	newProvisionerBaoStore = openHarborProvisionerBaoStore
	ghPublishRepoSecret    = ghSetRepoSecretNative
	ghRepoSecretExists     = ghRepoSecretExistsNative
	readAdminPasswordFile  = os.ReadFile
)

func ciHarborProvisionerCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "harbor-provisioner",
		Short: "in-cluster convergence loop: ensure Harbor project + robots, seed OpenBao, publish repo secrets",
		Long: "In-cluster replacement for the bootstrap workflow's harbor job. Ensures the\n" +
			"`platform` project and the ci-firewall-controller / pull-platform robots\n" +
			"exist, seeds secret/harbor/{robot,pull-robot} via a Kubernetes-auth OpenBao\n" +
			"role (no root token), publishes the repo-level HARBOR_* GitHub secrets the\n" +
			"standby bootstrap seeds from, and smoke-tests the seeded credentials.\n" +
			"Not-ready states exit 0 (the CronJob retries); a 401 smoke exits 1 —\n" +
			"delete the stale robot in Harbor UI and the next tick recreates it.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIHarborProvisioner() },
	}
}

func runCIHarborProvisioner() error {
	ctx := context.Background()
	apiURL := envOr("HARBOR_API_URL", "http://harbor-core.harbor.svc.cluster.local")
	registryHost := os.Getenv("HARBOR_HOST")

	// Admin password: a missing/empty mounted file means Harbor's Helm release
	// hasn't created harbor-admin-password yet — not an error, just "not yet".
	passFile := envOr("HARBOR_ADMIN_PASSWORD_FILE", "/etc/harbor-admin/HARBOR_ADMIN_PASSWORD")
	passRaw, err := readAdminPasswordFile(passFile)
	adminPass := strings.TrimSpace(string(passRaw))
	if err != nil || adminPass == "" {
		fmt.Printf("harbor admin password not available at %s — Harbor not deployed yet; nothing to do.\n", passFile)
		return nil
	}

	bao, err := newProvisionerBaoStore(ctx)
	if err != nil {
		return fmt.Errorf("openbao login (harbor-provisioner role): %w", err)
	}

	h := &harborAPI{baseURL: apiURL, adminPass: adminPass,
		client: &http.Client{Timeout: 15 * time.Second}}

	// Steady state: both robots seeded and the push robot authenticates → verify
	// the GitHub publication still exists (a failed publish after the OpenBao
	// seed, or a deleted repo secret, would otherwise strand the standby channel
	// forever — the values are re-publishable from OpenBao without touching
	// Harbor), then no-op.
	if seeded, creds := robotsSeeded(ctx, bao); seeded {
		if err := smokeSeededRobot(h, creds); err != nil {
			return err
		}
		return republishMissingRepoSecrets(ctx, bao)
	}

	// Ensure the `platform` project. Transport error = Harbor still coming up —
	// clean exit, next tick retries (the workflow step's warn-and-skip).
	status, body, err := h.post("/api/v2.0/projects",
		`{"project_name":"platform","metadata":{"public":"false"}}`)
	switch {
	case err != nil:
		fmt.Printf("harbor not reachable at %s (%v) — retrying next tick.\n", apiURL, err)
		return nil
	case status == http.StatusCreated:
		fmt.Println("harbor project 'platform' created.")
	case status == http.StatusConflict:
		fmt.Println("harbor project 'platform' already exists.")
	default:
		return fmt.Errorf("harbor project create failed (HTTP %d): %s", status, body)
	}

	for _, spec := range []harborRobotSpec{
		{
			payload:    newHarborRobotPayload("ci-firewall-controller", "push", "pull", "delete"),
			kvPath:     "secret/harbor/robot",
			nameSecret: "HARBOR_ROBOT_NAME",
			passSecret: "HARBOR_PASSWORD",
			doneMsg:    "harbor CI robot created; OpenBao seeded; HARBOR_ROBOT_NAME/HARBOR_PASSWORD published.",
		},
		{
			payload:    newHarborRobotPayload("pull-platform", "pull"),
			kvPath:     "secret/harbor/pull-robot",
			nameSecret: "HARBOR_PULL_ROBOT_NAME",
			passSecret: "HARBOR_PULL_PASSWORD",
			doneMsg:    "harbor pull-only robot created; OpenBao seeded; HARBOR_PULL_ROBOT_NAME/HARBOR_PULL_PASSWORD published.",
		},
	} {
		name, secret, created, err := h.createRobot(spec.payload)
		if err != nil {
			return err
		}
		if !created {
			// 409: the robot exists but its OpenBao path is unseeded (we only get
			// here when robotsSeeded was false). The secret is unrecoverable from
			// Harbor — an operator must delete the robot so the next tick can
			// recreate it with a fresh secret. Loud, but keep going: the sibling
			// robot may still provision.
			fmt.Fprintf(os.Stderr, "robot %q exists but secret/harbor path is unseeded — delete the robot in Harbor UI so the next tick recreates it.\n", spec.payload.Name)
			continue
		}
		if err := bao.Write(ctx, spec.kvPath, map[string]string{
			"username": name, "password": secret, "registry_host": registryHost,
		}); err != nil {
			return fmt.Errorf("seed %s: %w", spec.kvPath, err)
		}
		if os.Getenv("GH_TOKEN") == "" || os.Getenv("GH_REPO") == "" {
			fmt.Fprintf(os.Stderr, "GH_TOKEN/GH_REPO unset — skipping repo-secret publication for %s (standby seeding will not work without it).\n", spec.payload.Name)
		} else {
			if err := ghPublishRepoSecret(spec.nameSecret, name); err != nil {
				return err
			}
			if err := ghPublishRepoSecret(spec.passSecret, secret); err != nil {
				return err
			}
		}
		fmt.Println(spec.doneMsg)
	}
	return nil
}

// republishMissingRepoSecrets re-publishes any absent repo-level HARBOR_*
// secret from the OpenBao values — the recovery path for a publication that
// failed after the seed (or a repo secret someone deleted). No-op without GH
// env; existing secrets are never overwritten (rotation happens only through
// robot recreation).
func republishMissingRepoSecrets(ctx context.Context, bao baoStore) error {
	if os.Getenv("GH_TOKEN") == "" || os.Getenv("GH_REPO") == "" {
		fmt.Println("harbor robots seeded and authenticated — nothing to do (GH publication unconfigured).")
		return nil
	}
	for _, m := range []struct{ kvPath, field, secret string }{
		{"secret/harbor/robot", "username", "HARBOR_ROBOT_NAME"},
		{"secret/harbor/robot", "password", "HARBOR_PASSWORD"},
		{"secret/harbor/pull-robot", "username", "HARBOR_PULL_ROBOT_NAME"},
		{"secret/harbor/pull-robot", "password", "HARBOR_PULL_PASSWORD"},
	} {
		exists, err := ghRepoSecretExists(m.secret)
		if err != nil {
			return fmt.Errorf("check repo secret %s: %w", m.secret, err)
		}
		if exists {
			continue
		}
		v, ok, err := bao.Get(ctx, m.kvPath, m.field)
		if err != nil || !ok {
			return fmt.Errorf("re-publish %s: read %s.%s from OpenBao failed (ok=%v err=%v)", m.secret, m.kvPath, m.field, ok, err)
		}
		if err := ghPublishRepoSecret(m.secret, v); err != nil {
			return err
		}
		fmt.Printf("re-published missing repo secret %s from OpenBao.\n", m.secret)
	}
	fmt.Println("harbor robots seeded and authenticated — nothing to do.")
	return nil
}

// robotsSeeded reports whether both OpenBao robot paths hold credentials, and
// returns the push robot's for the smoke test.
func robotsSeeded(ctx context.Context, bao baoStore) (bool, [2]string) {
	user, ok1, err1 := bao.Get(ctx, "secret/harbor/robot", "username")
	pass, ok2, err2 := bao.Get(ctx, "secret/harbor/robot", "password")
	_, ok3, err3 := bao.Get(ctx, "secret/harbor/pull-robot", "username")
	if err1 != nil || err2 != nil || err3 != nil {
		return false, [2]string{}
	}
	return ok1 && ok2 && ok3 && user != "" && pass != "", [2]string{user, pass}
}

// smokeSeededRobot verifies the seeded push robot authenticates. 401 is the one
// hard failure this loop can't self-heal (the robot exists, so a create would
// 409, and Harbor never re-reveals a robot secret): exit non-zero so the failed
// Job is visible, and tell the operator the one-step fix.
func smokeSeededRobot(h *harborAPI, creds [2]string) error {
	req, err := http.NewRequest(http.MethodGet, h.baseURL+"/api/v2.0/projects", nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(creds[0], creds[1])
	resp, err := h.client.Do(req)
	if err != nil {
		fmt.Printf("harbor not reachable at %s (%v) — smoke deferred to next tick.\n", h.baseURL, err)
		return nil
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return nil // caller prints the steady-state note after the GH-publication check
	case http.StatusUnauthorized:
		return fmt.Errorf("seeded harbor robot credentials rejected (HTTP 401) — delete robot %q in Harbor UI; the next tick recreates and re-publishes it", creds[0])
	default:
		fmt.Fprintf(os.Stderr, "harbor smoke returned HTTP %d — verify robot account manually.\n", resp.StatusCode)
		return nil
	}
}

// openHarborProvisionerBaoStore logs in to OpenBao via Kubernetes auth as the
// harbor-provisioner role using the pod's ServiceAccount token — the same
// contract as the linode-cred-rotator's login (see openLinodeRotatorBaoStore).
func openHarborProvisionerBaoStore(ctx context.Context) (baoStore, error) {
	return openInClusterBaoStore(ctx, "harbor-provisioner")
}
