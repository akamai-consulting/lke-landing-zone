package harbor

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
//   HARBOR_API_URL              Harbor REST base (default http://harbor-core.harbor.svc.cluster.local).
//                               PLAINTEXT by design — the pod is in the Istio mesh
//                               and its sidecar upgrades this to mTLS on the wire.
//                               harbor-core has no client-cert auth, so the mesh is
//                               the only way this hop is mutually authenticated.
//   HARBOR_HOST                 registry host written as registry_host (per-env,
//                               patched by `llz render` from cluster.bootstrap.domainSuffix)
//   HARBOR_ADMIN_PASSWORD_FILE  mounted admin password (default /etc/harbor-admin/HARBOR_ADMIN_PASSWORD)
//   OPENBAO_ADDR / OPENBAO_KUBERNETES_MOUNT / OPENBAO_KUBERNETES_ROLE /
//   SA_TOKEN_FILE / OPENBAO_CA_FILE / OPENBAO_CLIENT_CERT_FILE / OPENBAO_CLIENT_KEY_FILE
//                               OpenBao k8s-auth login over mTLS (same contract as
//                               every in-cluster caller — openbao_k8s_login.go;
//                               role defaults to harbor-provisioner).
//                               OPENBAO_SKIP_VERIFY is GONE: the listener requires
//                               a client certificate, so there is no unverified path.
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

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/cigate"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/credrotate"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/ghsecret"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/harborauth"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/openbao"
)

// Seams for tests.
var (
	newProvisionerBaoStore = openHarborProvisionerBaoStore
	ghPublishRepoSecret    = ghsecret.SetRepoNative
	ghRepoSecretExists     = ghsecret.RepoSecretExistsNative
	readAdminPasswordFile  = os.ReadFile
)

// istioQuitURL is the Envoy admin endpoint that asks an injected sidecar to exit.
// Overridable so the test can point it at a stub.
var istioQuitURL = "http://127.0.0.1:15020/quitquitquit"

// ShutdownIstioSidecar asks this pod's Istio sidecar to exit, so a MESHED Job
// can reach Complete.
//
// Envoy does not exit when the application container does: the pod would stay
// Running forever and the Job would never complete. That single fact is why this
// CronJob previously opted OUT of the mesh (`sidecar.istio.io/inject: "false"`)
// — and opting out is what left the Harbor admin password crossing the pod
// network in cleartext. This call is what buys the opt-out back.
//
// Best-effort by construction, and silent on every failure:
//   - no sidecar injected (native sidecars, or injection disabled) → connection
//     refused, which is the correct no-op
//   - istiod running ENABLE_NATIVE_SIDECARS → the proxy is a native init
//     container that terminates on its own; this is redundant, not wrong
//
// It must never turn a successful provisioning run into a failed one, so the
// error is discarded rather than returned.
func ShutdownIstioSidecar() {
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Post(istioQuitURL, "", nil)
	if err != nil {
		return // no sidecar to stop — the overwhelmingly common case off-mesh
	}
	_ = resp.Body.Close()
}

// harborauth.UsableRegistryHost reports whether HARBOR_HOST carries a real registry host.
//
// It exists because the two ways HARBOR_HOST goes wrong both produce a NON-EMPTY
// value, which used to sail past this command's `host == ""` discovery fallback and
// get seeded into OpenBao as registry_host — the credential then authenticates for
// a registry by that name, matches nothing, and every push/pull 401s with an error
// that reads like bad credentials:
//
//   - "harbor." — RenderHarborHostPatch's harbor.<domainSuffix> rendered with an
//     empty domainSuffix, i.e. EVERY Managed App Platform instance rendered before
//     the fix in clusterspec.HarborHost. Those instances carry the bad value in a
//     committed artifact, so this guard (not the render fix) is what heals them,
//     with no re-render needed.
//   - "REPLACE_ME" — the components/harbor base placeholder, if the per-env patch
//     never merged.
//
// Deliberately narrow: it rejects a value with an empty final label or the known
// placeholder, and nothing else. A single-label host ("harbor") stays usable — that
// is a legitimate in-cluster registry name, not a rendering accident.
// usableRegistryHost moved to internal/harborauth when objenc needed the registry
// client; this is the one caller left on this side.

func RunProvisioner() error {
	ctx := context.Background()
	apiURL := cigate.EnvOr("HARBOR_API_URL", "http://harbor-core.harbor.svc.cluster.local")
	registryHost := os.Getenv("HARBOR_HOST")

	// Admin password: a missing/empty mounted file means Harbor's Helm release
	// hasn't created harbor-admin-password yet — not an error, just "not yet".
	passFile := cigate.EnvOr("HARBOR_ADMIN_PASSWORD_FILE", "/etc/harbor-admin/HARBOR_ADMIN_PASSWORD")
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
	seeded, creds, err := robotsSeeded(ctx, bao)
	if err != nil {
		// The seeded/unseeded decision drives everything below, up to and
		// including telling an operator to delete a live robot. A failed OpenBao
		// read is not an unseeded path — bail and let the next tick retry.
		return fmt.Errorf("cannot read the robot credentials from OpenBao, so this tick cannot tell "+
			"a seeded robot from an unseeded one: %w", err)
	}
	if seeded {
		if err := smokeSeededRobot(h, creds); err != nil {
			return err
		}
		if err := repairRegistryHost(ctx, bao, h, registryHost); err != nil {
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

	// registry_host resolution. HARBOR_HOST (render-injected harbor.<domainSuffix>)
	// wins when set AND usable — the self-install path. On a Managed App Platform
	// cluster the spec has no domainSuffix and render has no cluster access, so
	// HARBOR_HOST arrives empty (or, on an instance rendered before the HarborHost
	// fix, arrives as the unusable "harbor."); ask Harbor for its own external
	// registry host (ground truth). Harbor is confirmed reachable here (the project
	// ensure above just succeeded).
	if !harborauth.UsableRegistryHost(registryHost) {
		if strings.TrimSpace(registryHost) != "" {
			fmt.Printf("HARBOR_HOST is %q — not a usable registry host; ignoring it and asking Harbor for its own.\n", registryHost)
		}
		discovered, err := h.systemInfoRegistryHost()
		if err != nil {
			fmt.Printf("no usable HARBOR_HOST and harbor systeminfo unavailable (%v) — retrying next tick.\n", err)
			return nil
		}
		if discovered == "" {
			fmt.Println("no usable HARBOR_HOST and harbor systeminfo returned no registry_url — retrying next tick.")
			return nil
		}
		registryHost = discovered
		fmt.Printf("no usable HARBOR_HOST — discovered registry host %q from Harbor systeminfo (managed App Platform).\n", registryHost)
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
			// 409: the robot exists but its OpenBao path is unseeded. The premise
			// "unseeded" now holds — robotsSeeded returns its read error instead of
			// reporting false — which is what makes this advice safe to print: it
			// asks an operator to destroy a credential, so it must never be reached
			// on a failed read. The secret is unrecoverable from Harbor, so deleting
			// the robot is the only way the next tick can recreate it. Loud, but
			// keep going: the sibling robot may still provision.
			fmt.Fprintf(os.Stderr, "robot %q exists but %s is unseeded in OpenBao (read succeeded, path empty) — "+
				"delete the robot in Harbor UI so the next tick recreates it and re-seeds.\n",
				spec.payload.Name, spec.kvPath)
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
func republishMissingRepoSecrets(ctx context.Context, bao credrotate.BaoStore) error {
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
//
// The read error is RETURNED rather than folded into false. credrotate.BaoStore.Get already
// separates "absent" (ok) from "the read failed" (err); collapsing them here made
// an OpenBao 503 look like an unseeded path, and the caller's 409 branch then
// told the operator to delete a robot whose credentials were intact — destroying
// the very secret that was still recoverable.
func robotsSeeded(ctx context.Context, bao credrotate.BaoStore) (bool, [2]string, error) {
	user, ok1, err1 := bao.Get(ctx, "secret/harbor/robot", "username")
	pass, ok2, err2 := bao.Get(ctx, "secret/harbor/robot", "password")
	_, ok3, err3 := bao.Get(ctx, "secret/harbor/pull-robot", "username")
	for _, err := range []error{err1, err2, err3} {
		if err != nil {
			return false, [2]string{}, err
		}
	}
	return ok1 && ok2 && ok3 && user != "" && pass != "", [2]string{user, pass}, nil
}

// repairRegistryHost reconciles a STORED registry_host that is not a usable
// registry host, on a cluster whose robots are already seeded.
//
// Without this, the "harbor." fix reaches nobody who needs it. Every managed
// instance that rendered before it already seeded registry_host="harbor." into
// OpenBao, and a seeded cluster takes the steady-state branch above and returns —
// the resolution block never runs, so the bad value is permanent. Worse, the tick
// reports healthy: the smoke authenticates username/password, which are perfectly
// valid. Only the docker config DERIVED from registry_host is wrong, so every push
// and pull 401s while the loop says steady state. This command is a convergence
// loop, so a field that drifted out of the valid set gets reconciled like any other.
//
// Deliberately repairs only the UNUSABLE (see harborauth.UsableRegistryHost) — never a value
// that is merely different from what discovery reports. An operator may have
// pointed a cluster at a registry on purpose, and a loop that fights them every
// five minutes is worse than the bug.
//
// Credentials are never at risk: a KV v2 write REPLACES the secret, so the rewrite
// carries username/password back verbatim, and a path missing either is skipped
// loudly rather than rewritten with an empty credential.
func repairRegistryHost(ctx context.Context, bao credrotate.BaoStore, h *harborAPI, envHost string) error {
	var broken []string
	for _, p := range []string{"secret/harbor/robot", "secret/harbor/pull-robot"} {
		cur, _, err := bao.Get(ctx, p, "registry_host")
		if err != nil {
			return fmt.Errorf("read %s registry_host: %w", p, err)
		}
		if !harborauth.UsableRegistryHost(cur) {
			fmt.Printf("%s has registry_host %q — not a usable registry host; repairing.\n", p, cur)
			broken = append(broken, p)
		}
	}
	if len(broken) == 0 {
		return nil
	}

	// Same precedence as the seeding path: a usable rendered HARBOR_HOST wins,
	// otherwise ask Harbor for its own. Harbor being unreachable is not an error —
	// the next tick retries, exactly like the seeding path's not-ready exits.
	host := envHost
	if !harborauth.UsableRegistryHost(host) {
		discovered, err := h.systemInfoRegistryHost()
		if err != nil || discovered == "" {
			fmt.Printf("cannot resolve the registry host to repair with (systeminfo: %v) — retrying next tick.\n", err)
			return nil
		}
		host = discovered
	}

	for _, p := range broken {
		user, _, err := bao.Get(ctx, p, "username")
		if err != nil {
			return fmt.Errorf("read %s username: %w", p, err)
		}
		pass, _, err := bao.Get(ctx, p, "password")
		if err != nil {
			return fmt.Errorf("read %s password: %w", p, err)
		}
		if user == "" || pass == "" {
			// Rewriting now would replace a live credential with an empty one. The
			// wrong registry host is bad; a destroyed robot credential is worse.
			fmt.Fprintf(os.Stderr, "%s is missing username/password — refusing to rewrite registry_host, "+
				"which would drop the credential. Delete the robot in Harbor UI to have it re-provisioned.\n", p)
			continue
		}
		if err := bao.Write(ctx, p, map[string]string{
			"username": user, "password": pass, "registry_host": host,
		}); err != nil {
			return fmt.Errorf("repair %s registry_host: %w", p, err)
		}
		fmt.Printf("%s registry_host repaired to %q (credentials unchanged).\n", p, host)
	}
	return nil
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
func openHarborProvisionerBaoStore(ctx context.Context) (credrotate.BaoStore, error) {
	return openbao.OpenInClusterStore(ctx, "harbor-provisioner")
}
