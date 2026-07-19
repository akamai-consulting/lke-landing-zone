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
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			return runCIRequireSecret(args[0], os.Getenv(args[0]), hint, os.Stdout, os.Stderr)
		},
	}
	c.Flags().StringVar(&hint, "hint", "", "remediation line appended to the error annotation (where the secret comes from)")
	return c
}

// runCIRequireSecret returns nil when the secret is present and an error when it
// is not — cobra turns that into the exit-1 the workflows gate on. The
// ::error:: annotations are still WRITTEN here (not folded into the returned
// error): GitHub only parses an annotation command at the START of a line, and
// the returned error reaches stderr behind main.go's "llz: " prefix, which would
// destroy it.
func runCIRequireSecret(name, value, hint string, out, errOut io.Writer) error {
	if value == "" {
		fmt.Fprintf(errOut, "::error::%s is not set.\n", name)
		if hint != "" {
			fmt.Fprintf(errOut, "::error::%s\n", hint)
		}
		return fmt.Errorf("%s is not set", name)
	}
	fmt.Fprintf(out, "%s: present.\n", name)
	return nil
}

func ciAssertDestroyConfirmCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "assert-destroy-confirm <region> <module> <confirm-value>",
		Short: "fail unless the typed confirm token is destroy:<region>:<module>",
		Long: "Native port of assert-destroy-confirm.sh. Validates the CONFIRM_DESTROY\n" +
			"token required before any terraform destroy: the caller must have typed the\n" +
			"full 'destroy:<region>:<module>' token, which prevents accidental destroys.",
		Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			return runCIAssertDestroyConfirm(args[0], args[1], args[2], os.Stderr)
		},
	}
	return c
}

func runCIAssertDestroyConfirm(region, module, confirm string, errOut io.Writer) error {
	expected := fmt.Sprintf("destroy:%s:%s", region, module)
	if confirm != expected {
		fmt.Fprintf(errOut, "::error::Set confirm_destroy to '%s' to proceed.\n", expected)
		return fmt.Errorf("set confirm_destroy to %q to proceed", expected)
	}
	return nil
}
