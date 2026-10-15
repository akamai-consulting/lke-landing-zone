package objenc

// cobra_objenc.go — the object-storage encryption verb, reduced to a flag set.
//
// Everything it does is `obj-encryption` and lives in this package. What stays
// here is ObjencDeps(): a kubectl read and a Secret decoder.

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kube"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

// ObjencDeps is a VAR, not a func, so a test can swap the whole capability set —
// the same seam pattern package main already uses for its kubectl and Linode
// clients.
var ObjencDeps = func() Deps {
	return Deps{
		KubectlOut:  kubectlprobe.Out,
		SecretField: kube.SecretField,
	}
}

func AssertObjEncryptionCmd() *cobra.Command {
	var endpoint, bucket, harborBucket, region string
	var sample int
	var since time.Duration
	c := &cobra.Command{
		Use:   "assert-obj-encryption",
		Short: "fail unless objects written to Object Storage are actually encrypted at rest",
		Long: "Runtime gate on Object Storage encryption at rest.\n\n" +
			"Linode applies SSE-S3 by default on E2/E3 endpoints, so there is no config\n" +
			"to check — only objects. Two checks, both on real objects:\n" +
			"  1. Harbor pushes a fresh blob, and it must come back with\n" +
			"     x-amz-server-side-encryption on HEAD (the one object this gate can\n" +
			"     guarantee was written NOW, by a real writer)\n" +
			"  2. a sample of the Harbor and Loki buckets: any object written within\n" +
			"     --since that is stored in the clear fails the gate\n\n" +
			"Fails on E0/E1 endpoints, which default SSE-S3 does not cover. Plaintext\n" +
			"objects older than --since are reported as residue (rewrite them with\n" +
			"copy-object to encrypt), not as a failure.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return RunAssertEncryption(ObjencDeps(), endpoint, bucket, harborBucket, region, sample, since)
		},
	}
	c.Flags().StringVar(&endpoint, "endpoint", "", "endpoint host override (default: derived from the env's cluster.objectStorage.cluster)")
	c.Flags().StringVar(&harborBucket, "harbor-bucket", "", "Harbor registry bucket override (default: derived from the spec). Its push is the only check that guarantees a fresh object")
	c.Flags().StringVar(&region, "region", os.Getenv("REGION"), "deployment (spec env name) whose buckets are checked")
	c.Flags().IntVar(&sample, "sample", 50, "how many objects per bucket to sample for check 2. Any plaintext in the sample fails; a clean sample is evidence, not proof")
	c.Flags().DurationVar(&since, "since", 24*time.Hour, "judge only objects written within this window; older plaintext is reported as pre-enablement residue")
	c.Flags().StringVar(&bucket, "bucket", "", "Loki chunks bucket override (default: derived from the spec)")
	return c
}

// SeedSSECKeyCmd is the retired SSE-C key seed, kept as a hidden no-op. Instance
// repos vendor their workflows, so a not-yet-upgraded instance's bootstrap still
// calls it; failing there would break that instance on an image bump alone. It
// goes once `llz upgrade` has carried the new bootstrap workflow everywhere.
//
// It deliberately does NOT touch secret/obj/ssec. An instance that ran the gateway
// holds objects only that key can read, and a retired verb deleting it would be the
// one irreversible thing this retirement could do.
func SeedSSECKeyCmd() *cobra.Command {
	var region string
	c := &cobra.Command{
		Use:    "seed-ssec-key",
		Short:  "retired — the obj-proxy SSE-C gateway is gone; accepted and does nothing",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			fmt.Println("::notice::seed-ssec-key is retired with the obj-proxy SSE-C gateway and does nothing " +
				"(Linode applies SSE-S3 by default on E2/E3 endpoints). Any existing key at secret/obj/ssec is " +
				"left in place. Run `llz upgrade` to drop the call from the vendored bootstrap workflow.")
			return nil
		},
	}
	c.Flags().StringVar(&region, "region", "", "accepted and ignored (vendored-workflow compatibility)")
	return c
}
