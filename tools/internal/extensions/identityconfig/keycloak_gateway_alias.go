package identityconfig

// ci_keycloak_gateway_alias.go — `llz ci pin-keycloak-gateway-alias`.
//
// OpenBao validates team-login tokens against Keycloak's realm JWKS, and that
// fetch must be over VERIFIED TLS because it carries signing keys (ADR 0010).
// The only endpoint in this cluster that serves Keycloak over TLS is the Istio
// ingress gateway: apl-core gives the Keycloak pod no TLS key material, so its
// :8443 is a declared-but-dead port (see keycloakJWKSURL).
//
// The gateway is addressed by the PUBLIC hostname, because that is the name on
// the certificate (`otomi-wildcard`, SAN `*.<domainSuffix>`). But that name
// resolves to the cluster's external LoadBalancer IP, and hairpinning back in
// through it is unsupported on LKE-E — the reason the public URL was ruled out
// originally.
//
// This command closes the gap: it resolves the gateway Service's CLUSTER IP and
// pins `keycloak.<domain> -> <clusterIP>` into the OpenBao pods' /etc/hosts via
// the StatefulSet's hostAliases. The request then never leaves the cluster,
// while Host and SNI still carry the public name, so the wildcard certificate
// verifies against the CA OpenBao already mounts.

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/baoread"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cigate"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/health"
)

const (
	// The namespace apl-core puts its Gateway API gateways in.
	gatewayNamespace = "istio-system"
	// Istio's Gateway API implementation labels the Service it generates for a
	// Gateway with the gateway's name. Preferred over guessing "<name>-istio",
	// which is only a naming convention.
	gatewayNameLabel = "gateway.networking.k8s.io/gateway-name"
	// The upstream subchart derives the StatefulSet name from the Helm RELEASE
	// name, which platform-apl/components/openbao/openbao.yaml pins to
	// `platform-openbao` (and calls out as load-bearing for exactly this reason).
	openbaoStatefulSet = "platform-openbao"
)

func RunPinGatewayAlias(region string) error {
	issuer := keycloakIssuerFor(region)
	if issuer == "" {
		// No issuer means no teams configured / no domain — bao-configure already
		// skips the keycloak mount in that case, so there is nothing to pin.
		fmt.Println("no Keycloak issuer resolved — nothing to pin (team login is not configured).")
		return nil
	}
	host := keycloakHostFromIssuer(issuer)
	if host == "" {
		return fmt.Errorf("could not extract a hostname from Keycloak issuer %q", issuer)
	}

	ip, svc, err := gatewayClusterIP()
	if err != nil {
		return err
	}
	fmt.Printf("→ %s/%s (ClusterIP %s) will serve %s inside the OpenBao pods\n", gatewayNamespace, svc, ip, host)

	current, ok := statefulSetHostAliasIP(baoread.Namespace, openbaoStatefulSet, host)
	if !ok {
		return nil // warned already; the pod wait below owns this failure
	}
	if current == ip {
		fmt.Printf("hostAliases already pins %s -> %s; no write (avoids a needless rollout).\n", host, ip)
		return nil
	}
	if current != "" {
		fmt.Fprintf(os.Stderr, "::warning::%s hostAlias moved %s -> %s (gateway Service was recreated); patching, which rolls OpenBao.\n", host, current, ip)
	}

	patch := fmt.Sprintf(`{"spec":{"template":{"spec":{"hostAliases":[{"ip":%q,"hostnames":[%q]}]}}}}`, ip, host)
	if err := patchWithWebhookRetry(patch); err != nil {
		return err
	}
	fmt.Printf("pinned %s -> %s on %s/%s.\n", host, ip, baoread.Namespace, openbaoStatefulSet)
	return nil
}

// keycloakHostFromIssuer extracts the bare hostname from a realm issuer URL of
// the form https://keycloak.<domain>/realms/otomi. Pure, so the parsing is
// unit-tested rather than only exercised against a cluster.
func keycloakHostFromIssuer(issuer string) string {
	s, ok := strings.CutPrefix(issuer, "https://")
	if !ok {
		return ""
	}
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	// Guard against a port sneaking in — hostAliases hostnames are names, not
	// host:port, and a bad entry silently fails to resolve.
	if strings.ContainsAny(s, ":") || s == "" {
		return ""
	}
	return s
}

// gatewayClusterIP finds the ClusterIP of the Service fronting apl-core's ingress
// Gateway. Prefers the Gateway-API label Istio stamps on the generated Service;
// an installation with several gateways is resolved by preferring one that
// carries a 443 port, since that is the listener terminating TLS.
// It falls back to an unlabelled sweep of the namespace when the label matches
// nothing. The label is Istio's current convention rather than a guarantee, and
// this command runs in the bootstrap critical path — a renamed label must not
// take the whole cluster build down for a team-login-only feature. The 443
// preference then does the real selecting.
func gatewayClusterIP() (string, string, error) {
	out, err := execOutput("kubectl", "-n", gatewayNamespace, "get", "svc",
		"-l", gatewayNameLabel, "-o", "json")
	if err != nil {
		return "", "", fmt.Errorf("list gateway Services in %s: %w: %s", gatewayNamespace, err, strings.TrimSpace(string(out)))
	}
	if ip, name, ok := parseGatewayServices(out); ok {
		return ip, name, nil
	}
	fmt.Fprintf(os.Stderr, "::warning::no Service in %s carries %s — falling back to any Service there exposing :443\n",
		gatewayNamespace, gatewayNameLabel)
	out, err = execOutput("kubectl", "-n", gatewayNamespace, "get", "svc", "-o", "json")
	if err != nil {
		return "", "", fmt.Errorf("list Services in %s: %w: %s", gatewayNamespace, err, strings.TrimSpace(string(out)))
	}
	ip, name, ok := parseGatewayServices(out)
	if !ok {
		return "", "", fmt.Errorf("no Service with a usable ClusterIP exposing :443 found in %s.\n"+
			"OpenBao needs one to reach Keycloak's JWKS over verified TLS; without it the fetch would\n"+
			"hairpin through the external LoadBalancer, which LKE-E does not support", gatewayNamespace)
	}
	return ip, name, nil
}

// parseGatewayServices decodes a `kubectl get svc -o json` list and applies the
// selection rules. Split out so both the labelled query and the fallback sweep
// share one parser.
func parseGatewayServices(out []byte) (ip, name string, ok bool) {
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				ClusterIP string `json:"clusterIP"`
				Ports     []struct {
					Port int `json:"port"`
				} `json:"ports"`
			} `json:"spec"`
		} `json:"items"`
	}
	if json.Unmarshal(out, &list) != nil {
		return "", "", false
	}
	svcs := make([]gatewaySvc, 0, len(list.Items))
	for _, it := range list.Items {
		ports := make([]int, 0, len(it.Spec.Ports))
		for _, p := range it.Spec.Ports {
			ports = append(ports, p.Port)
		}
		svcs = append(svcs, gatewaySvc{Name: it.Metadata.Name, ClusterIP: it.Spec.ClusterIP, Ports: ports})
	}
	ip, name = pickGatewayClusterIP(svcs)
	return ip, name, ip != ""
}

// gatewaySvc is the slice of a Service this command needs.
type gatewaySvc struct {
	Name      string
	ClusterIP string
	Ports     []int
}

// pickGatewayClusterIP chooses the gateway Service to alias to. Pure, so the
// selection rules are unit-tested.
//
// "None" is excluded explicitly: a headless Service has no ClusterIP to dial,
// and pinning the literal string "None" into /etc/hosts produces an entry that
// fails to resolve rather than an error anyone would notice.
func pickGatewayClusterIP(svcs []gatewaySvc) (ip, name string) {
	var fallbackIP, fallbackName string
	for _, s := range svcs {
		if s.ClusterIP == "" || s.ClusterIP == "None" {
			continue
		}
		for _, p := range s.Ports {
			if p == 443 {
				return s.ClusterIP, s.Name
			}
		}
		if fallbackIP == "" {
			fallbackIP, fallbackName = s.ClusterIP, s.Name
		}
	}
	return fallbackIP, fallbackName
}

// statefulSetWait bounds how long to wait for Argo to create the StatefulSet.
// Short: the pin only needs to land before the pods start, and the step that
// genuinely waits for them (`llz ci wait-pods`, 600s) runs immediately after.
var (
	statefulSetWait = 90 * time.Second
	statefulSetPoll = 5 * time.Second
)

// statefulSetHostAliasIP returns the IP currently aliased to host, waiting for the
// StatefulSet to exist first. ok=false means it never appeared.
//
// A MISSING STATEFULSET IS NOT AN ERROR, and treating it as one broke a run.
// The original comment here claimed this command "runs after the OpenBao
// Application has synced" — but the preceding gate (`llz ci assert-argo-app`)
// only asserts the Application EXISTS, not that Argo has reconciled its
// resources. So the StatefulSet legitimately does not exist yet, and this
// hard-failed the whole bootstrap for a team-login-only pin:
//
//	llz: read llz-openbao/platform-openbao hostAliases: exit status 1:
//
// It waits instead, and gives up quietly: the very next step waits 600s for the
// pods and reports a far better error if they never arrive. Losing the pin costs
// team login; failing here costs the cluster.
func statefulSetHostAliasIP(ns, name, host string) (ip string, ok bool) {
	deadline := time.Now().Add(statefulSetWait)
	for attempt := 1; ; attempt++ {
		// Combined output: kubectl writes "not found" to STDERR, so capturing only
		// stdout produced `exit status 1:` with an empty reason — the same blind
		// spot that made the earlier wait-loop diagnostics useless.
		out := execCombined("kubectl", "-n", ns, "get", "statefulset", name,
			"-o", "jsonpath={.spec.template.spec.hostAliases}", "--request-timeout=10s")
		if !strings.Contains(out, "not found") && !strings.Contains(out, "Unable to connect") {
			return hostAliasIP([]byte(out), host), true
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr, "::warning::runner-acl/keycloak-pin: %s/%s did not appear within %s (%d attempt(s)): %s — "+
				"skipping the JWKS host pin. Team login will fail until it is applied, but the pod wait below "+
				"reports the real problem if the StatefulSet never syncs.\n",
				ns, name, statefulSetWait, attempt, cigate.FirstLine(strings.TrimSpace(out)))
			return "", false
		}
		fmt.Printf("keycloak-pin: waiting for %s/%s to be created by Argo (attempt %d)...\n", ns, name, attempt)
		time.Sleep(statefulSetPoll)
	}
}

// patchWithWebhookRetry applies the hostAliases patch, retrying while Kyverno's
// admission webhook is unreachable.
//
// THE RACE. This step runs during bootstrap, and the write goes through Kyverno's
// validating webhook, which is failurePolicy: Fail. If Kyverno's endpoint is not
// serving yet the apiserver rejects the patch outright:
//
//	Internal error occurred: failed calling webhook "validate.kyverno.svc-fail":
//	failed to call webhook: Post "https://kyverno-svc.kyverno.svc:443/validate/fail":
//	No agent available
//
// The step already waited for the StatefulSet to be CREATED by Argo, which is a
// different readiness question and told us nothing about admission. So it failed the
// whole bootstrap job on a transient that clears in well under a minute — twice,
// observed, and the first time the log gave no clue why.
//
// ONLY the race is retried. A non-race patch failure (a bad field, RBAC) is returned
// immediately, because retrying those just delays a real error behind a timeout.
func patchWithWebhookRetry(patch string) error {
	deadline := keycloakPinNow().Add(keycloakPinWebhookBudget)
	for attempt := 1; ; attempt++ {
		out, err := execOutput("kubectl", "-n", baoread.Namespace, "patch", "statefulset", openbaoStatefulSet,
			"--type=strategic", "-p", patch)
		if err == nil {
			return nil
		}
		text := strings.TrimSpace(string(out)) + " " + err.Error()
		if !health.IsWebhookRace(text) {
			return fmt.Errorf("patch %s/%s hostAliases: %w: %s", baoread.Namespace, openbaoStatefulSet, err, strings.TrimSpace(string(out)))
		}
		if !keycloakPinNow().Before(deadline) {
			return fmt.Errorf("patch %s/%s hostAliases: Kyverno's admission webhook was still unreachable after %s: %w: %s",
				baoread.Namespace, openbaoStatefulSet, keycloakPinWebhookBudget, err, strings.TrimSpace(string(out)))
		}
		fmt.Printf("keycloak-pin: Kyverno's admission webhook is not serving yet (attempt %d) — retrying in %s\n",
			attempt, keycloakPinWebhookInterval)
		keycloakPinSleep(keycloakPinWebhookInterval)
	}
}

// Bounded so a genuinely broken Kyverno fails the job rather than hanging it. Three
// minutes covers the observed 30-90s window with room for a slow node.
var (
	keycloakPinWebhookBudget   = 3 * time.Minute
	keycloakPinWebhookInterval = 10 * time.Second
	keycloakPinNow             = time.Now
	keycloakPinSleep           = time.Sleep
)

// hostAliasIP parses a hostAliases JSON array and returns the IP mapped to host.
// Pure and unit-tested; an empty/absent/garbled list reads as "not pinned",
// which makes the caller write the alias rather than skip it.
func hostAliasIP(raw []byte, host string) string {
	var aliases []struct {
		IP        string   `json:"ip"`
		Hostnames []string `json:"hostnames"`
	}
	if json.Unmarshal(raw, &aliases) != nil {
		return ""
	}
	for _, a := range aliases {
		for _, h := range a.Hostnames {
			if h == host {
				return a.IP
			}
		}
	}
	return ""
}
