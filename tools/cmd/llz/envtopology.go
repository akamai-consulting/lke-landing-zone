package main

// envtopology.go — the capability wiring for the `env-topology` extension
// (internal/envtopology).
//
// Installed from an init() for the same reason internal/kubectlprobe and
// internal/configreadiness are: several package main paths call in (`llz tokens`,
// the wizard's branch-policy lock), and a test that stubs main's execOutput must
// reach this extension's copy too.

import (
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/answers"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/envtopology"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/ghaout"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/proc"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/render"
)

func init() { installEnvTopologyDeps() }

func installEnvTopologyDeps() {
	envtopology.Install(envtopology.Deps{
		Exec:     func(n string, a ...string) ([]byte, error) { return execOutput(n, a...) },
		ExecArgv: proc.Run,
		Summary:  ghaout.Append,
		LoadSpec: func() (*clusterspec.LandingZone, bool, error) { return clusterspec.Detected() },
		// Narrowed to the one field, as internal/promote's is.
		InstanceRepo: func() string {
			a, _ := answers.Read(".")
			if a == nil {
				return ""
			}
			return a.InstanceRepo
		},
		// `llz env set` writes the declarative source and then re-renders; this is
		// the second half of every mutation the extension performs.
		Render:      func(env string) error { return render.Run(gopts.dryRun, env, false, false, false) },
		DryRun:      gopts.dryRun,
		PromoteDeps: promoteDeps,
	})
}
