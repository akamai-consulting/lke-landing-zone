package main

// ci_baoca.go — the `llz ci extract-openbao-ca` and `provision-peer-ca` flag sets.
//
// The verbs are tools/internal/baoca. This is the CA slice of the catalog's
// openbao-lifecycle row, taken on its own because it is the one part of that row
// with a boundary: two cobra constructors in, nothing else out.

import (
	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/baoca"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/cliopts"
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
		RunE: func(_ *cobra.Command, _ []string) error { return baoca.RunExtractCA(required) },
	}
	c.Flags().BoolVar(&required, "required", false, "fail (exit 1) when openbao-tls is absent instead of warning + exit 0")
	return c
}

func ciProvisionPeerCACmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "provision-peer-ca",
		Short: "create the openbao-peer-tls Secret in the active peer cluster from $CA_B64",
		Long: "Native port of the duplicated \"Provision openbao-peer-tls Secret\" steps.\n" +
			"Reads the standby's CA cert from $CA_B64 (a base64 ca.crt extracted by\n" +
			"extract-openbao-ca and passed across the job boundary), guards the empty\n" +
			"handoff (a masked job output is redacted to empty — provisioning an empty\n" +
			"ca.crt would silently break trust), and idempotently applies the\n" +
			"openbao-peer-tls Secret in the llz-openbao namespace of the active peer\n" +
			"(the runner's kubeconfig already points there). Establishes cross-cluster\n" +
			"trust so standby operations can run with VAULT_SKIP_VERIFY=false.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return baoca.RunProvisionPeerCA(cliopts.Global.DryRun) },
	}
	return c
}

// RunProvisionPeerCA writes the peer cluster's CA into the openbao-peer-tls
// Secret so the two OpenBao halves trust each other.
//
// dryRun is a plain BOOL, not package main's globalOpts: a flag is not a
// capability, and taking the struct would drag main's flag model across the
// boundary to read one bit.
