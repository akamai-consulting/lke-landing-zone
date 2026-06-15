package main

// ci_guards.go — `llz ci require-secret` and `llz ci assert-destroy-confirm`,
// the native ports of instance-scripts/ci/{require-secret,assert-destroy-confirm}.sh:
// tiny pre-flight gates the workflows run before doing anything expensive.
// Both only inspect their inputs and print GitHub annotations; the logic lives
// in pure functions writing to injected io.Writers so tests cover every path.

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
)

func ciRequireSecretCmd() *cobra.Command {
	var hint string
	c := &cobra.Command{
		Use:   "require-secret <var-name>",
		Short: "fail with a ::error:: annotation when the named env secret is empty",
		Long: "Native port of require-secret.sh. Reads the value from the environment\n" +
			"variable <var-name> (the calling step maps the GitHub secret into env:), so\n" +
			"the value never appears on argv. Emits a ::error:: annotation (plus the\n" +
			"--hint line, which should say where the secret comes from) and exits 1 when\n" +
			"empty; prints \"<var-name>: present.\" otherwise.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			os.Exit(runCIRequireSecret(args[0], os.Getenv(args[0]), hint, os.Stdout, os.Stderr))
			return nil
		},
	}
	c.Flags().StringVar(&hint, "hint", "", "remediation line appended to the error annotation (where the secret comes from)")
	return c
}

func runCIRequireSecret(name, value, hint string, out, errOut io.Writer) int {
	if value == "" {
		fmt.Fprintf(errOut, "::error::%s is not set.\n", name)
		if hint != "" {
			fmt.Fprintf(errOut, "::error::%s\n", hint)
		}
		return 1
	}
	fmt.Fprintf(out, "%s: present.\n", name)
	return 0
}

func ciAssertDestroyConfirmCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "assert-destroy-confirm <region> <module> <confirm-value>",
		Short: "fail unless the typed confirm token is destroy:<region>:<module>",
		Long: "Native port of assert-destroy-confirm.sh. Validates the CONFIRM_DESTROY\n" +
			"token required before any terraform destroy: the caller must have typed the\n" +
			"full 'destroy:<region>:<module>' token, which prevents accidental destroys.",
		Args: cobra.ExactArgs(3),
		RunE: func(_ *cobra.Command, args []string) error {
			os.Exit(runCIAssertDestroyConfirm(args[0], args[1], args[2], os.Stderr))
			return nil
		},
	}
	return c
}

func runCIAssertDestroyConfirm(region, module, confirm string, errOut io.Writer) int {
	expected := fmt.Sprintf("destroy:%s:%s", region, module)
	if confirm != expected {
		fmt.Fprintf(errOut, "::error::Set confirm_destroy to '%s' to proceed.\n", expected)
		return 1
	}
	return 0
}
