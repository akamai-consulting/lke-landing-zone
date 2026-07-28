package main

// tfbin.go — the one place that decides which Terraform-family binary llz shells
// out to. The Go half of template-scripts/lib-common.sh's detect_tf, and it
// resolves in the SAME order, so a shell script and llz can never disagree about
// which binary is running against a given state file.
//
// The landing zone runs OpenTofu (docs/adr/0008-opentofu-migration.md). Seven
// call sites used to hardcode "terraform", which is how the toolchain split went
// unnoticed: the CI image shipped HashiCorp Terraform while every local checkout
// resolved a `tofu` on PATH (often via an `alias terraform=tofu`), so a clean
// local run proved nothing about what CI executed.
//
// `terraform` remains a FALLBACK rather than being removed outright, for
// adopters mid-migration whose runners still only have it. It is not the
// preferred name anywhere, and the CI image no longer contains it.

import (
	"os"
	"os/exec"
)

// tfBinOverride is the escape hatch, mirroring detect_tf's $TF. Set it to run
// against a specific binary (a pinned tofu build, or terraform during a
// migration) without editing anything.
const tfBinEnv = "TF"

// resolveTFBin is a package var so tests can pin the answer without depending on
// what happens to be installed on the machine running them.
var resolveTFBin = func() string {
	if v := os.Getenv(tfBinEnv); v != "" {
		return v
	}
	if p, err := exec.LookPath("tofu"); err == nil && p != "" {
		return "tofu"
	}
	if p, err := exec.LookPath("terraform"); err == nil && p != "" {
		return "terraform"
	}
	// Neither is present. Return the preferred name so the failure names what
	// SHOULD be installed ("exec: tofu: not found") rather than the legacy one.
	return "tofu"
}

// tfBin returns the Terraform-family binary to exec.
//
// Deliberately NOT cached. Caching was the first instinct — "a plan and its
// apply must not straddle two binaries" — but nothing mutates PATH inside a
// running llz process, so the cache bought no real determinism while silently
// defeating every test that stubs a fake binary on PATH: whichever test ran
// first would pin the answer for the whole package. A LookPath per call is
// cheap, and being observable in a test is worth more here.
func tfBin() string { return resolveTFBin() }

// tfCommand builds an exec.Cmd for the resolved binary. Every llz shell-out to
// Terraform/OpenTofu goes through here.
func tfCommand(args ...string) *exec.Cmd {
	return exec.Command(tfBin(), args...) // #nosec G204 -- binary is resolved from a fixed allowlist above
}
