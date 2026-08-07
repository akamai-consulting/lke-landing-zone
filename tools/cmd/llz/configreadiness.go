package main

// configreadiness.go — the capability wiring for the `config-readiness`
// extension (internal/configreadiness).
//
// The Exec seam is installed from an init() for the same reason exec.go does it
// for internal/kubectlprobe: several package main verbs call into this extension
// (`llz status`, `llz build`, `llz tokens`, the root-token nag), and a test that
// stubs main's execOutput must reach the extension's copy too. withExecOutput
// swaps both — stubbing one leaves the other shelling out to a real `gh`.

import (
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/configreadiness"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/linode"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/render"
)

func init() { installConfigReadinessDeps() }

func installConfigReadinessDeps() {
	configreadiness.Install(configreadiness.Deps{
		Exec:               func(n string, a ...string) ([]byte, error) { return execOutput(n, a...) },
		CloudToken:         linode.TokenFromEnv,
		LoadSpec:           func() (*clusterspec.LandingZone, bool, error) { return clusterspec.Detected() },
		CheckManifestDrift: render.CheckManifestDrift,
	})
}
