package main

// ci_bao_seed_seal_key.go implements `llz ci bao-seed-seal-key` — it creates the
// per-cluster 32-byte static auto-unseal key as the `openbao-unseal-key` Secret
// in the openbao namespace. The chart's `seal "static"` stanza mounts that
// Secret at /openbao/seal/unseal.key and every pod unseals itself from it at
// boot (no managed KMS on Linode; the key lives only in etcd, encrypted at rest
// on LKE-E). It must run before the OpenBao StatefulSet's pods come up — a
// missing Secret volume leaves a pod in ContainerCreating (it waits, it does not
// crash-loop), so this step need only COMPLETE, not strictly precede the chart's
// Argo wave-0 sync.
//
// Idempotent and NEVER-rotating: an existing Secret is the live unseal key and
// is left untouched — a changed key would brick every pod, because the recovery
// keys from `bao operator init` authorize `generate-root` but CANNOT decrypt the
// root key. On a namespace rebuild where the Secret is gone but the
// infra-<region> OPENBAO_SEAL_KEY environment secret persisted, the key is
// RESTORED from there; only a truly first-ever bootstrap generates a new key,
// persists it to infra-<region> for disaster recovery, and tells the operator to
// back it up offline.

import (
	"encoding/base64"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
)

// sealKeySecretName is the Secret the chart's `seal "static"` stanza mounts; its
// single `unseal.key` entry holds the raw 32-byte AES-256-GCM-96 key.
const sealKeySecretName = "openbao-unseal-key"

// sealKeyBytes is the key length OpenBao's static seal requires (AES-256).
const sealKeyBytes = 32

// openbaoNSWait bounds how long the seed waits for the llz-openbao namespace to
// exist before applying the Secret. The namespace is pre-created at an early
// wave (-20) of the llz-cluster-foundation Argo app, which only starts syncing
// once apl-operator has brought Argo CD up — a convergence race the seed step
// (in the separate Bootstrap OpenBao job) otherwise loses when apl-operator is
// still young (e.g. it rolled and re-ran its helmfile). Bounded + fail-loud per
// the convergence contract: a namespace that never appears is a real wedge, not
// something to soft-continue past.
const (
	openbaoNSWait     = 300 * time.Second
	openbaoNSInterval = 5 * time.Second
)

// seams (overridable in tests):
var (
	// seedNamespaceExists reports whether the openbao namespace exists yet.
	seedNamespaceExists = func(ns string) bool { return kExists("get", "namespace", ns) }
	// seedSleep paces the namespace poll.
	seedSleep = time.Sleep
)

// waitForOpenbaoNamespace polls (immediate first probe, openbaoNSInterval cadence)
// until the namespace exists or the budget is spent, then fails loud.
func waitForOpenbaoNamespace(ns string, within time.Duration) error {
	attempts := int(within/openbaoNSInterval) + 1
	for i := 0; i < attempts; i++ {
		if seedNamespaceExists(ns) {
			return nil
		}
		if i < attempts-1 {
			if i == 0 {
				fmt.Printf("namespace %q not present yet — waiting up to %s for the llz-cluster-foundation Argo app to create it…\n", ns, within)
			}
			seedSleep(openbaoNSInterval)
		}
	}
	return fmt.Errorf("namespace %q not found after %s — the llz-cluster-foundation Argo app that pre-creates it (wave -20) has not synced yet; apl-operator's helmfile pipeline is likely still converging Argo CD", ns, within)
}

func ciBaoSeedSealKeyCmd() *cobra.Command {
	var region string
	c := &cobra.Command{
		Use:   "bao-seed-seal-key",
		Short: "create the per-cluster static auto-unseal key Secret (openbao-unseal-key)",
		Long: "Creates the `openbao-unseal-key` Secret holding this cluster's 32-byte static\n" +
			"auto-unseal key, which the chart's `seal \"static\"` stanza mounts at\n" +
			"/openbao/seal/unseal.key so every OpenBao pod unseals itself at boot (no managed\n" +
			"KMS on Linode). Run it before the OpenBao pods start; a missing Secret leaves a\n" +
			"pod in ContainerCreating, not crash-looping, so it need only complete. Idempotent\n" +
			"and never-rotating: an existing Secret is left untouched (a changed key bricks\n" +
			"unseal). On a namespace rebuild it restores the key from the infra-<region>\n" +
			"OPENBAO_SEAL_KEY secret; a first-ever bootstrap generates a new key, persists it\n" +
			"to infra-<region> for DR (requires GH_TOKEN/GH_REPO), and prints an offline-backup\n" +
			"banner — losing this key loses the data.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIBaoSeedSealKey(gopts, region) },
	}
	c.Flags().StringVar(&region, "region", "", "region whose infra-<region> environment backs up the key for DR (required)")
	return c
}

func runCIBaoSeedSealKey(g globalOpts, region string) error {
	if region == "" {
		return fmt.Errorf("--region is required")
	}
	if g.dryRun {
		fmt.Fprintf(os.Stderr, "→ (dry-run) would ensure the %s/%s static auto-unseal key Secret exists\n", openbaoNS, sealKeySecretName)
		return nil
	}

	// The Secret lands in llz-openbao, which the llz-cluster-foundation Argo app
	// pre-creates (wave -20) once Argo CD is up. This step runs in a separate job
	// that can reach `seed` before that sync lands (apl-operator still converging),
	// so wait for the namespace first — otherwise both the idempotency check below
	// and the apply race it, and a fresh key would be generated + persisted only to
	// fail on `kubectl apply`. Fail loud if it never appears.
	if err := waitForOpenbaoNamespace(openbaoNS, openbaoNSWait); err != nil {
		return err
	}

	// An existing Secret is the live unseal key — never overwrite it.
	if kExists("-n", openbaoNS, "get", "secret", sealKeySecretName) {
		fmt.Printf("%s/%s already exists — leaving the static seal key untouched.\n", openbaoNS, sealKeySecretName)
		return nil
	}

	key, err := resolveSealKey(region)
	if err != nil {
		return err
	}

	// The Secret stores the RAW 32 bytes under unseal.key; the chart mounts it at
	// /openbao/seal/unseal.key and the `seal "static"` stanza reads it as
	// file:///openbao/seal/unseal.key.
	if err := kubectlApplyFn(sealKeySecretManifest(openbaoNS, sealKeySecretName, key)); err != nil {
		return fmt.Errorf("apply %s/%s: %w", openbaoNS, sealKeySecretName, err)
	}
	fmt.Printf("Created %s/%s (32-byte static auto-unseal key).\n", openbaoNS, sealKeySecretName)
	return nil
}

// resolveSealKey returns the 32 raw key bytes to seed: restored from the
// infra-<region> OPENBAO_SEAL_KEY secret if present (a namespace rebuild), else
// freshly generated, persisted to infra-<region> for DR, and flagged for offline
// backup.
func resolveSealKey(region string) ([]byte, error) {
	if enc := os.Getenv("OPENBAO_SEAL_KEY"); enc != "" {
		key, err := base64.StdEncoding.DecodeString(enc)
		if err != nil {
			return nil, fmt.Errorf("OPENBAO_SEAL_KEY is not valid base64: %w", err)
		}
		if len(key) != sealKeyBytes {
			return nil, fmt.Errorf("OPENBAO_SEAL_KEY decodes to %d bytes, want %d", len(key), sealKeyBytes)
		}
		fmt.Printf("Restoring the static seal key from the infra-%s OPENBAO_SEAL_KEY secret.\n", region)
		return key, nil
	}

	// First-ever bootstrap: the key must be persisted for DR, so a missing
	// secrets-write PAT is fatal — otherwise the only copy would be the in-cluster
	// Secret, and a namespace loss would be unrecoverable.
	if os.Getenv("GH_TOKEN") == "" {
		return nil, fmt.Errorf("no existing %s/%s, no OPENBAO_SEAL_KEY to restore, and GH_TOKEN (OPENBAO_SECRETS_WRITE_TOKEN) is not set — a new static seal key must be persisted as the infra-%s OPENBAO_SEAL_KEY secret for disaster recovery", openbaoNS, sealKeySecretName, region)
	}

	key := make([]byte, sealKeyBytes)
	if err := seedRandRead(key); err != nil {
		return nil, fmt.Errorf("crypto/rand for the static seal key: %w", err)
	}
	enc := base64.StdEncoding.EncodeToString(key)
	maskGHA(enc)

	// Offline-backup banner first — the key is generated exactly once. (It is NOT
	// printed here; the operator retrieves it from the infra-<region> secret.)
	if err := appendGHAFile("GITHUB_STEP_SUMMARY",
		"## OpenBao static auto-unseal key generated — Back It Up Now",
		"",
		"**OPERATOR ACTION REQUIRED:**",
		"A new 32-byte static auto-unseal key was generated for this cluster and stored",
		fmt.Sprintf("as the infra-%s `OPENBAO_SEAL_KEY` environment secret. Copy it to secure", region),
		"offline storage immediately: it is the ONLY key that can unseal this cluster's",
		"data. The recovery keys from `bao operator init` authorize generate-root but",
		"CANNOT decrypt the root key, so losing this key loses the data."); err != nil {
		return nil, err
	}
	if err := ghSetSecretFn("OPENBAO_SEAL_KEY", "infra-"+region, enc); err != nil {
		return nil, err
	}
	fmt.Printf("Generated a new static seal key and persisted it to infra-%s::OPENBAO_SEAL_KEY.\n", region)
	return key, nil
}

// sealKeySecretManifest renders an Opaque Secret holding the raw 32-byte static
// seal key under unseal.key — the native form of `kubectl create secret generic
// … --from-file=unseal.key=… --dry-run=client -o yaml | kubectl apply -f -`.
func sealKeySecretManifest(ns, name string, key []byte) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %s
  namespace: %s
type: Opaque
data:
  unseal.key: %s
`, name, ns, base64.StdEncoding.EncodeToString(key))
}
