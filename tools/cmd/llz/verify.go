package main

// verify.go ports verify-lab-bootstrap.sh into `llz verify`: a read-only
// snapshot of a freshly-bootstrapped apl-core cluster — the SSH-via-_rawValues
// wiring landed, the github.com mirror is out of the loop, and the platform
// Applications are reconciling. It does NOT wait (re-run if a check is just
// mid-reconcile). Runs against the current kubectl context.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

type verifyOpts struct {
	sshSourceHost string // SSH source-of-truth host, if the GitOps repo is reached over SSH (e.g. a self-hosted Git host); empty skips the SSH-source checks
}

func runVerify(g globalOpts, o verifyOpts) error {
	if g.dryRun {
		fmt.Println("→ (dry-run) read-only verification snapshot via kubectl (current context)")
		return nil
	}

	v := &verifier{}

	// 1-3. SSH source checks — only when the GitOps repo is reached over SSH
	// (an SSH source host was provided via --ssh-source-host). Adopters using an
	// HTTPS values-repo mirror have no SSH source, so these are skipped.
	if o.sshSourceHost == "" {
		v.section("1-3. SSH source checks skipped (no --ssh-source-host; HTTPS values-repo path)")
	} else {
		// 1. ArgoCD repository Secret for the SSH source.
		v.section("1. ArgoCD repository Secret for SSH source")
		secretsJSON, _ := kubectlOut("-n", "argocd", "get", "secret",
			"-l", "argocd.argoproj.io/secret-type=repository", "-o", "json")
		name, hasKey, found := findSSHRepoSecret(secretsJSON, o.sshSourceHost)
		if !found {
			v.fail("no repository Secret references " + o.sshSourceHost + " (_rawValues filtered or wrong path)")
		} else {
			v.pass("repository Secret for " + o.sshSourceHost + " found: " + name)
			if hasKey {
				v.pass("Secret contains sshPrivateKey field")
			} else {
				v.fail("Secret missing sshPrivateKey field — _rawValues filtered or wrong key name")
			}
		}

		// 2. known_hosts CM populated.
		v.section("2. argocd-ssh-known-hosts-cm contains " + o.sshSourceHost)
		kh, _ := kubectlOut("-n", "argocd", "get", "cm", "argocd-ssh-known-hosts-cm",
			"-o", "jsonpath={.data.ssh_known_hosts}")
		switch {
		case strings.TrimSpace(kh) == "":
			v.fail("argocd-ssh-known-hosts-cm not found or empty")
		case knownHostsHas(kh, o.sshSourceHost):
			v.pass(o.sshSourceHost + " entry present in known_hosts")
		default:
			v.fail(o.sshSourceHost + " entry NOT in known_hosts — ArgoCD will reject the SSH handshake")
		}

		// 3. argocd-repo-server can authenticate (no SSH errors in recent logs).
		v.section("3. argocd-repo-server SSH handshake against " + o.sshSourceHost)
		logs, _ := kubectlOut("-n", "argocd", "logs", "deployment/argocd-repo-server", "--tail=500")
		if sshAuthError.MatchString(logs) {
			v.fail("argocd-repo-server logs contain SSH auth errors (permission denied / host key verification failed)")
		} else {
			v.pass("no permission-denied or host-key-verification errors in last 500 log lines")
		}
	}

	// 4. platform custom Applications Synced + Healthy.
	v.section("4. platform custom Applications Synced + Healthy")
	appsJSON, _ := kubectlOut("-n", "argocd", "get", "applications", "-o", "json")
	apps := selectPlatformApps(appsJSON)
	if len(apps) == 0 {
		v.fail("no platform-prefixed Applications found in argocd namespace")
	}
	for _, a := range apps {
		if a.healthy() {
			v.pass(fmt.Sprintf("%s  sync=%s  health=%s", a.Name, a.Sync, a.Health))
		} else {
			v.fail(fmt.Sprintf("%s  sync=%s  health=%s", a.Name, a.Sync, a.Health))
		}
	}

	// 5. apl-operator git-config points at the external HTTPS repo.
	v.section("5. apl-operator apl-git-config repoUrl")
	repoURL, _ := kubectlOut("-n", "apl-operator", "get", "cm", "apl-git-config", "-o", "jsonpath={.data.repoUrl}")
	switch {
	case strings.TrimSpace(repoURL) == "":
		v.fail("apl-git-config not found or has no repoUrl")
	case strings.Contains(strings.ToLower(repoURL), "gitea"):
		v.fail("repoUrl still points at the in-cluster Gitea (should be external HTTPS): " + repoURL)
	case strings.Contains(strings.ToLower(repoURL), "github.com"):
		v.pass("repoUrl points at the external HTTPS repo: " + repoURL)
	default:
		v.fail("repoUrl is neither the external repo nor Gitea: " + repoURL + " (verify intent)")
	}

	// 6. OpenBao seal status (informational).
	v.section("6. OpenBao seal status (informational)")
	pod, _ := kubectlOut("-n", openbaoNS, "get", "pod", "-l", "app.kubernetes.io/name=openbao",
		"-o", "jsonpath={.items[0].metadata.name}")
	if strings.TrimSpace(pod) == "" {
		fmt.Println("  INFO  no OpenBao pods found (may be pre-bootstrap)")
	} else {
		st, _, _ := baoExec(strings.TrimSpace(pod), "", "", "status", "-format=json")
		sealed, _ := parseBaoStatus(st)
		if strings.TrimSpace(st) == "" {
			fmt.Println("  INFO  could not determine seal status (pod may be initialising)")
		} else if sealed {
			fmt.Println("  INFO  OpenBao is sealed (run bootstrap-openbao.yml if first boot)")
		} else {
			v.pass("OpenBao is unsealed")
		}
	}

	// 7. ESO ClusterSecretStore health.
	v.section("7. ESO ClusterSecretStore for OpenBao")
	css, _ := kubectlOut("get", "clustersecretstore", "openbao",
		"-o", "jsonpath={.status.conditions[?(@.type==\"Ready\")].status}")
	switch strings.TrimSpace(css) {
	case "True":
		v.pass("ClusterSecretStore openbao Ready=True")
	case "":
		v.fail("ClusterSecretStore openbao not found")
	default:
		v.fail("ClusterSecretStore openbao Ready=" + strings.TrimSpace(css))
	}

	fmt.Printf("\n%d/%d checks passed.\n", v.passed, v.passed+v.failed)
	if v.failed > 0 {
		return fmt.Errorf("%d verification check(s) failed", v.failed)
	}
	return nil
}

// ── result tracking ──────────────────────────────────────────────────────────

type verifier struct{ passed, failed int }

func (v *verifier) section(s string) { fmt.Printf("\n\033[1m%s\033[0m\n", s) }
func (v *verifier) pass(s string)    { fmt.Printf("  \033[32mPASS\033[0m  %s\n", s); v.passed++ }
func (v *verifier) fail(s string)    { fmt.Printf("  \033[31mFAIL\033[0m  %s\n", s); v.failed++ }

func kubectlOut(args ...string) (string, error) {
	out, err := execOutput("kubectl", args...)
	return string(out), err
}

// ── pure helpers (unit-tested) ───────────────────────────────────────────────

var sshAuthError = regexp.MustCompile(`(?i)permission denied|host key verification failed`)

var platformAppRe = regexp.MustCompile(`llz-linode-cidr-firewall|llz-cert-automation|llz-eso-cert-watcher|llz-cluster-foundation`)

// findSSHRepoSecret finds the ArgoCD repository Secret whose base64 .data.url
// contains host, reporting its name and whether it carries an sshPrivateKey.
func findSSHRepoSecret(secretsJSON, host string) (name string, hasKey, found bool) {
	var doc struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Data map[string]string `json:"data"`
		} `json:"items"`
	}
	if json.Unmarshal([]byte(secretsJSON), &doc) != nil {
		return "", false, false
	}
	for _, it := range doc.Items {
		urlB64 := it.Data["url"]
		dec, err := base64.StdEncoding.DecodeString(urlB64)
		if err != nil || !strings.Contains(string(dec), host) {
			continue
		}
		return it.Metadata.Name, len(it.Data["sshPrivateKey"]) > 0, true
	}
	return "", false, false
}

// knownHostsHas reports whether any line begins with "<host> ".
func knownHostsHas(knownHosts, host string) bool {
	for _, line := range strings.Split(knownHosts, "\n") {
		if strings.HasPrefix(line, host+" ") {
			return true
		}
	}
	return false
}

// selectPlatformApps returns the platform-* (or known llz-*) Applications.
func selectPlatformApps(appsJSON string) []argoApp {
	var doc struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Status struct {
				Sync   struct{ Status string } `json:"sync"`
				Health struct{ Status string } `json:"health"`
			} `json:"status"`
		} `json:"items"`
	}
	if json.Unmarshal([]byte(appsJSON), &doc) != nil {
		return nil
	}
	var out []argoApp
	for _, it := range doc.Items {
		n := it.Metadata.Name
		if strings.HasPrefix(n, "platform-") || platformAppRe.MatchString(n) {
			out = append(out, argoApp{n, it.Status.Sync.Status, it.Status.Health.Status})
		}
	}
	return out
}
