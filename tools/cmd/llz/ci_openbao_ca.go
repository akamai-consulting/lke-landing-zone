package main

// ci_openbao_ca.go implements `llz ci extract-openbao-ca` — the native port of
// the two near-identical "Extract standby CA cert" inline-bash steps in
// llz-bootstrap-openbao.yml (the bootstrap job's secondary_ca step, which warns
// and exits 0 when the Secret is absent, and the reprovision-ca job's extract
// step, which errors and exits 1). Both read the openbao-tls Secret's public
// ca.crt and emit ca_b64 + ca_available step outputs for the provision-peer-ca
// job; the only difference was the on-missing behavior, so one command with a
// --required flag covers both and removes the copy.

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

func ciExtractOpenbaoCACmd() *cobra.Command {
	var required bool
	c := &cobra.Command{
		Use:   "extract-openbao-ca",
		Short: "read the openbao-tls CA cert and emit ca_b64/ca_available step outputs",
		Long: "Native port of the duplicated \"Extract standby CA cert\" steps. Reads the\n" +
			"public ca.crt of the openbao-tls Secret in the llz-openbao namespace and\n" +
			"writes ca_b64=<base64> + ca_available=true to $GITHUB_OUTPUT so the\n" +
			"provision-peer-ca job can create the openbao-peer-tls Secret in the active\n" +
			"peer's cluster. The cert is public material and deliberately NOT masked —\n" +
			"the runner empties masked values in JOB outputs, which would silently hand\n" +
			"the consumer an empty ca.crt. When the Secret is absent it writes\n" +
			"ca_available=false and, by default, warns + exits 0 (the bootstrap job's\n" +
			"non-fatal twin); --required makes the absence an error + exit 1 (the\n" +
			"reprovision-ca job).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIExtractOpenbaoCA(required) },
	}
	c.Flags().BoolVar(&required, "required", false, "fail (exit 1) when openbao-tls is absent instead of warning + exit 0")
	return c
}

func runCIExtractOpenbaoCA(required bool) error {
	// `2>/dev/null || true` of the bash: an absent Secret is a normal state
	// (handled below), not a hard error, so a non-zero kubectl just yields "".
	caB64 := ""
	if out, err := execOutput("kubectl", "-n", openbaoNS, "get", "secret", "openbao-tls",
		"-o", `jsonpath={.data.ca\.crt}`); err == nil {
		caB64 = strings.TrimSpace(string(out))
	}
	if caB64 == "" {
		if err := appendGHAFile("GITHUB_OUTPUT", "ca_available=false"); err != nil {
			return err
		}
		if required {
			fmt.Fprintln(os.Stderr, "::error::openbao-tls Secret not found in standby cluster — cannot extract CA")
			return fmt.Errorf("openbao-tls Secret not found in %s", openbaoNS)
		}
		fmt.Fprintln(os.Stderr, "::warning::openbao-tls Secret not found in standby cluster — CA not provisioned")
		return nil
	}
	fmt.Println("Standby CA cert extracted.")
	return appendGHAFile("GITHUB_OUTPUT", "ca_b64="+caB64, "ca_available=true")
}
