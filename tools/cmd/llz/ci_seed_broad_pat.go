package main

// ci_seed_broad_pat.go implements `llz ci seed-broad-pat --region <env>` — the
// bootstrap seed for the in-cluster broad-PAT rotator's minting credential.
//
// The rotator (platform-apl/components/broadPatRotator, `llz ci rotate-broad-pat`)
// mints its successor with the CURRENT broad account:read_write PAT, which it
// reads from secret/linode/broad-pat via ESO. Nothing else seeds that path, so
// this step primes it from LINODE_API_TOKEN — the same broad CI/TF token that
// carries the minting privilege — the first time the component is enabled.
//
// GATING is the whole point of a dedicated command (vs. an entry in the generic
// bao-seed-all table): the broad PAT is ACCOUNT-wide, so it must live in exactly
// ONE cluster — the one deployment that enables broadPatRotator. This reads the
// LandingZone spec and no-ops on every deployment that leaves the component off,
// so a cluster never carries the broad PAT unless it owns the rotation. Keeping
// the decision here (unit-tested) rather than in workflow bash is the repo's
// untestable-loc contract.
//
// rotated_at is seeded to 0 so the first CronJob tick reads the path as due;
// --skip-if-present token makes re-runs idempotent — once the rotator has minted
// and written a successor, this never clobbers it back to the bootstrap token.

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
)

// broadPATRotatorComponent is the spec.components key that gates the seed.
const broadPATRotatorComponent = "broadPatRotator"

// broadPATSeedEnabled reports whether the broad-PAT rotator is enabled for region
// in the loaded spec — the gate that keeps the account-wide broad PAT out of every
// cluster that does not own the rotation. A missing env is not enabled.
func broadPATSeedEnabled(lz *clusterspec.LandingZone, region string) bool {
	e, ok := lz.Env(region)
	if !ok {
		return false
	}
	return clusterspec.ComponentEnabled(e.Components, broadPATRotatorComponent)
}

func ciSeedBroadPATCmd() *cobra.Command {
	var region string
	c := &cobra.Command{
		Use:   "seed-broad-pat",
		Short: "seed secret/linode/broad-pat from LINODE_API_TOKEN when the broadPatRotator component is enabled",
		Long: "Bootstrap seed for the in-cluster broad-PAT rotator's minting credential.\n" +
			"No-ops unless spec.components.broadPatRotator is enabled for --region: the\n" +
			"broad account:read_write PAT is ACCOUNT-wide, so only the one deployment that\n" +
			"runs the rotator carries it. When enabled, seeds secret/linode/broad-pat from\n" +
			"LINODE_API_TOKEN (token=) with rotated_at=0 so the first tick is due, and\n" +
			"--skip-if-present token so a later rotation-minted value is never clobbered.\n" +
			"Env: LINODE_API_TOKEN, OPENBAO_* (root-token bootstrap posture).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCISeedBroadPAT(region) },
	}
	c.Flags().StringVar(&region, "region", "", "deployment (spec env name) whose broadPatRotator toggle gates the seed (required)")
	return c
}

func runCISeedBroadPAT(region string) error {
	if region == "" {
		return fmt.Errorf("--region is required")
	}
	lz, err := clusterspec.LoadInstance(".")
	if err != nil {
		return fmt.Errorf("seed-broad-pat: load spec: %w", err)
	}
	if !broadPATSeedEnabled(lz, region) {
		fmt.Printf("broadPatRotator not enabled for %s — skipping secret/linode/broad-pat seed (the broad PAT stays out of this cluster).\n", region)
		return nil
	}
	if os.Getenv("LINODE_API_TOKEN") == "" {
		return fmt.Errorf("seed-broad-pat: LINODE_API_TOKEN must be set (the broad account:read_write PAT the rotator mints its successor with)")
	}
	// Delegate the actual OpenBao write to the generic seed primitive so the
	// skip-if-present guard, ::add-mask::, and error handling stay identical to
	// every other bootstrap seed.
	return runCIBaoSeed(baoSeedOpts{
		path:          "secret/linode/broad-pat",
		fieldSpecs:    []string{"token=env:LINODE_API_TOKEN", "rotated_at=literal:0"},
		skipIfPresent: "token",
		onMissing:     "error",
	})
}
