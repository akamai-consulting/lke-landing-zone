package main

// envtopology.go — the capability wiring for the `env-topology` extension
// (internal/envtopology).
//
// Installed from an init() for the same reason internal/kubectlprobe and
// internal/configreadiness are: several package main paths call in (`llz tokens`,
// the wizard's branch-policy lock), and a test that stubs main's execOutput must
// reach this extension's copy too.

import (
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/envtopology"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/render"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/answers"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cliopts"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghaout"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/proc"
)

func init() { installEnvTopologyDeps() }

func installEnvTopologyDeps() {
	envtopology.Install(envtopology.Deps{
		Exec:     execOutput,
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
		Render:      func(env string) error { return render.Run(cliopts.Global.DryRun, env, false, false, false) },
		DryRun:      cliopts.Global.DryRun,
		PromoteDeps: promoteDeps,
	})
}
