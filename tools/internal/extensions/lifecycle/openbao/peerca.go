package openbao

// ci_openbao_ca.go implements `llz ci extract-openbao-ca` — the native port of
// the two near-identical "Extract standby CA cert" inline-bash steps in
// llz-bootstrap-openbao.yml (the bootstrap job's secondary_ca step, which warns
// and exits 0 when the Secret is absent, and the reprovision-ca job's extract
// step, which errors and exits 1). Both read the openbao-tls Secret's public
// ca.crt and emit ca_b64 + ca_available step outputs for the provision-peer-ca
// job; the only difference was the on-missing behavior, so one command with a
// --required flag covers both and removes the copy.

import (
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/baoread"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kube"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

func RunExtractCA(required bool) error {
	// `2>/dev/null || true` of the bash: an absent Secret is a normal state
	// (handled below), not a hard error, so a non-zero kubectl just yields "".
	caB64 := ""
	if out, err := kubectlprobe.Exec("kubectl", "-n", baoread.Namespace, "get", "secret", "openbao-tls",
		"-o", `jsonpath={.data.ca\.crt}`); err == nil {
		caB64 = strings.TrimSpace(string(out))
	}
	if caB64 == "" {
		if err := appendGHAFile("GITHUB_OUTPUT", "ca_available=false"); err != nil {
			return err
		}
		if required {
			fmt.Fprintln(os.Stderr, "::error::openbao-tls Secret not found in standby cluster — cannot extract CA")
			return fmt.Errorf("openbao-tls Secret not found in %s", baoread.Namespace)
		}
		fmt.Fprintln(os.Stderr, "::warning::openbao-tls Secret not found in standby cluster — CA not provisioned")
		return nil
	}
	fmt.Println("Standby CA cert extracted.")
	return appendGHAFile("GITHUB_OUTPUT", "ca_b64="+caB64, "ca_available=true")
}

// ── provision-peer-ca ───────────────────────────────────────────────────────
// The consumer twin of extract-openbao-ca: the two byte-identical "Provision
// openbao-peer-tls Secret" steps (one in the provision-peer-ca job after a
// standby bootstrap, one in the standalone reprovision-ca job) differed only in
// which job output fed $CA_B64, so one command covers both.

func RunProvisionPeerCA(dryRun bool) error {
	caB64 := strings.TrimSpace(os.Getenv("CA_B64"))
	if caB64 == "" {
		fmt.Fprintln(os.Stderr, "::error::CA_B64 is empty — refusing to provision an empty ca.crt")
		return fmt.Errorf("CA_B64 is empty")
	}
	caPEM, err := base64.StdEncoding.DecodeString(caB64)
	if err != nil {
		return fmt.Errorf("CA_B64 is not valid base64: %w", err)
	}
	if dryRun {
		fmt.Fprintf(os.Stderr, "→ (dry-run) would apply openbao-peer-tls Secret in %s\n", baoread.Namespace)
		return nil
	}
	// kube.SecretManifest base64-encodes the value, so passing the decoded PEM
	// yields data."ca.crt" == the original CA_B64 — same Secret the bash produced
	// via `kubectl create secret … --from-literal=ca.crt=$(base64 -d) | apply`.
	if err := kube.Apply(kube.SecretManifest(baoread.Namespace, "openbao-peer-tls", "ca.crt", string(caPEM))); err != nil {
		return fmt.Errorf("apply openbao-peer-tls: %w", err)
	}
	fmt.Println("openbao-peer-tls Secret provisioned in the active peer cluster.")
	fmt.Println("Establishes cross-cluster trust (VAULT_SKIP_VERIFY=false) for standby operations.")
	return nil
}

// appendGHAFile appends lines to the GitHub Actions command file named by envVar.
// Pure, localised, and it does the REAL append rather than defaulting to a no-op:
// a no-op default turns every test that asserts on the step output into a
// tautology. (internal/envtopology wrote that reason down first.)
func appendGHAFile(envVar string, lines ...string) error {
	path := os.Getenv(envVar)
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open $%s: %w", envVar, err)
	}
	for _, l := range lines {
		if _, err := fmt.Fprintln(f, l); err != nil {
			f.Close()
			return fmt.Errorf("write $%s: %w", envVar, err)
		}
	}
	return f.Close()
}
