package main

// apl_app.go — `llz apl app`, the App Platform apps (components) verb of the apl
// front door (ADR 0013). `list` shows the component registry; `enable`/`disable`
// flip a component in environments/<env>.yaml — the declarative GitOps source —
// then re-render, so the toggle can't be forgotten (the apl-overlay reconciler
// propagates it onto apl-<env> from there). It is validated, friendlier sugar over
// `llz env set <env> components.<app>.enabled=<bool>`: it checks the app is a real
// component and refuses to disable a mandatory one.

import (
	"fmt"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/envtopology"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/render"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/yamledit"
	"github.com/spf13/cobra"
	yaml "gopkg.in/yaml.v3"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/color"
)

func aplAppCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "app",
		Short: "App Platform apps (components): list, enable, disable",
		Long: "Manage which App Platform apps (spec.components) an environment runs.\n" +
			"`list` shows the toggleable registry; `enable`/`disable` set the toggle in\n" +
			"environments/<env>.yaml and re-render — the declarative source of truth, from\n" +
			"which the apl-overlay reconciler propagates the change onto apl-<env>.",
	}
	c.AddCommand(
		renamed(clusterspec.ComponentsCmd(), "list", "list the component registry (default state, backends, sizing knobs)"),
		aplAppToggleCmd("enable", true),
		aplAppToggleCmd("disable", false),
	)
	return c
}

func aplAppToggleCmd(verb string, enable bool) *cobra.Command {
	var env string
	c := &cobra.Command{
		Use:   verb + " <app> --env <env>",
		Short: verb + " an App Platform app in environments/<env>.yaml + re-render",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runAppToggle(env, args[0], enable)
		},
	}
	c.Flags().StringVar(&env, "env", "", "deployment/env to toggle the app in (required)")
	_ = c.MarkFlagRequired("env")
	return c
}

// runAppToggle flips components.<app>.enabled in environments/<env>.yaml and
// re-renders. It validates the app against the component registry and refuses to
// disable a mandatory component (the cluster would not converge without it).
func runAppToggle(env, app string, enable bool) error {
	if env == "" {
		return fmt.Errorf("--env is required")
	}
	comp, ok := findComponent(app)
	if !ok {
		return fmt.Errorf("unknown app %q — see `llz apl app list` for the toggleable set", app)
	}
	if comp.Mandatory && !enable {
		return fmt.Errorf("%q is a required component and cannot be disabled (the cluster would not converge)", app)
	}

	envFile, err := envtopology.SpecFile(env)
	if err != nil {
		return err
	}
	path := "components." + app + ".enabled"
	value := "true"
	if !enable {
		value = "false"
	}
	if err := yamledit.EditSpecFile(envFile, func(doc *yaml.Node) error {
		return yamledit.SetSpecPath(doc, path, value)
	}, func(b []byte) error { _, e := clusterspec.DecodeClusterDefinition(b); return e }); err != nil {
		return err
	}

	done := "enabled"
	if !enable {
		done = "disabled"
	}
	fmt.Printf("  %s %s in %s (spec.%s = %s)\n", color.Green(done), app, env, path, value)
	fmt.Printf("\n%s\n", color.Bold(fmt.Sprintf("Reconciling (`llz render %s`):", env)))
	return render.Run(gopts.dryRun, env, false, false, false)
}

// findComponent returns the component registry entry for an exact name.
func findComponent(name string) (clusterspec.Component, bool) {
	for _, c := range clusterspec.Components {
		if c.Name == name {
			return c, true
		}
	}
	return clusterspec.Component{}, false
}
