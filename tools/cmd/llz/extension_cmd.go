package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func extensionCmd() *cobra.Command {
	x := &cobra.Command{
		Use:   "extension",
		Short: "EXPERIMENT (issue #10): scaffold + lint user extensions (the core-bloat relief valve)",
		Long: "Scaffolder-first take on the recipe/extension vehicle. `new` clones an\n" +
			"embedded skeleton so a compliant extension is cheaper to create than a new\n" +
			"ci_*.go in core; `lint` enforces the capability ceiling (argv-only manifest +\n" +
			"tests for logic-bearing kinds). Experimental — no loader/fetch/lock yet.",
	}
	var kind, dir string
	newC := &cobra.Command{
		Use:   "new <name>",
		Short: "scaffold an extension from an embedded skeleton (--kind check|tool|observability)",
		Args:  cobra.ExactArgs(1),
		RunE:  func(_ *cobra.Command, a []string) error { return runExtensionNew(gopts, a[0], dir, kind) },
	}
	newC.Flags().StringVar(&kind, "kind", "check", "skeleton to clone: check (logic-bearing, ships tests) | tool (thin wrap of an external tool) | observability (custom alerts + dashboard component)")
	newC.Flags().StringVar(&dir, "dir", "extensions", "parent directory to create <name>/ under")

	lintC := &cobra.Command{
		Use:   "lint <dir>",
		Short: "enforce the ceiling: argv-only manifest + tests for logic-bearing kinds",
		Args:  cobra.ExactArgs(1),
		RunE:  func(_ *cobra.Command, a []string) error { return runExtensionLint(a[0]) },
	}

	var check bool
	var upgradeRoot string
	upgradeC := &cobra.Command{
		Use:   "upgrade [dir]",
		Short: "migrate manifest(s) AND re-apply files for every enabled extension (or one [dir])",
		Long: "Brings extensions up to the manifest schema this llz binary speaks, then\n" +
			"re-applies their files: into the instance — the same \"propagate with the\n" +
			"binary, no template round-trip\" model as `llz upgrade`. With no argument it\n" +
			"runs over every enabled extension (.llz/extensions.yaml); pass a [dir] for one.\n" +
			"--check aggregates schema drift and scaffold drift; both write nothing.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, a []string) error {
			if len(a) == 1 {
				return runExtensionUpgrade(gopts, a[0], upgradeRoot, check)
			}
			return runExtensionUpgradeAll(gopts, upgradeRoot, check)
		},
	}
	upgradeC.Flags().BoolVar(&check, "check", false, "report drift and exit non-zero when behind; write nothing")
	upgradeC.Flags().StringVar(&upgradeRoot, "root", ".", "instance repo root to re-apply files into")

	var excludeRoot string
	excludeC := &cobra.Command{
		Use:   "exclude",
		Short: "print the copier _exclude block for extension-owned paths (from the lock)",
		Args:  cobra.NoArgs,
		RunE:  func(_ *cobra.Command, _ []string) error { return runExtensionExclude(excludeRoot) },
	}
	excludeC.Flags().StringVar(&excludeRoot, "root", ".", "instance repo root")

	wiringC := &cobra.Command{
		Use:   "wiring <dir>",
		Short: "print copier `migrations:` + renovate custom-manager glue for distributing an extension",
		Long: "Emits the reuse-path glue for a remote extension: a copier `migrations:`\n" +
			"block (copier sequences the update/merge, then calls the binary's tested\n" +
			"`extension upgrade`) and a renovate custom manager (PRs the pinned ref forward\n" +
			"when upstream tags). Paste each block where its banner says.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, a []string) error { return runExtensionWiring(a[0]) },
	}

	var ciCheck bool
	var ciRoot string
	ciWorkflowC := &cobra.Command{
		Use:   "ci-workflow [extensions-dir]",
		Short: "generate .github/workflows/llz-extensions.yml from enabled extensions' ci: steps (anchor → needs:)",
		Long: "Renders a static, needs:-chained workflow from each extension's ci: steps —\n" +
			"the same codegen pattern as `llz env pipeline` (promote.yml). With no argument\n" +
			"it uses the enabled set (.llz/extensions.yaml); pass a [dir] to scan it instead.\n" +
			"Each step's `anchor` places it around the core `converge` pivot; `dependsOn`\n" +
			"becomes an inter-extension `needs:` edge. --check is the CI drift gate.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, a []string) error {
			var jobs []extCIJob
			var err error
			if len(a) == 1 {
				jobs, err = loadExtensionCIJobs(a[0])
			} else {
				jobs, err = enabledCIJobs(ciRoot)
			}
			if err != nil {
				return err
			}
			return runExtensionCIWorkflow(gopts, jobs, ciRoot, ciCheck)
		},
	}
	ciWorkflowC.Flags().BoolVar(&ciCheck, "check", false, "report drift and exit non-zero when the workflow is stale; write nothing")
	ciWorkflowC.Flags().StringVar(&ciRoot, "root", ".", "instance repo root")

	var applyRoot string
	var applyCheck bool
	applyC := &cobra.Command{
		Use:   "apply [extension-dir]",
		Short: "render files: into the instance for every enabled extension (or one [dir]); --check for drift",
		Long: "Renders every files: entry into the instance repo and records the owned paths\n" +
			"+ digests in .llz/extensions.lock. With no argument it runs over every enabled\n" +
			"extension; pass a [dir] for one. --check reports drift (a scaffolded file hand-\n" +
			"edited, missing, or orphaned) and writes nothing.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, a []string) error {
			if len(a) == 1 {
				return runExtensionApply(gopts, a[0], applyRoot, applyCheck)
			}
			return runExtensionApplyAll(gopts, applyRoot, applyCheck)
		},
	}
	applyC.Flags().StringVar(&applyRoot, "root", ".", "instance repo root to scaffold into")
	applyC.Flags().BoolVar(&applyCheck, "check", false, "report scaffold drift; write nothing; exit non-zero")

	lifecycleC := &cobra.Command{
		Use:     "lifecycle",
		Aliases: []string{"anchors"},
		Short:   "print the lifecycle registry: phases, engines, CI anchor jobs, fired hooks, and day-2 actions",
		Args:    cobra.NoArgs,
		RunE:    func(_ *cobra.Command, _ []string) error { return runLifecycle() },
	}

	var regRoot string
	listC := &cobra.Command{
		Use:   "list",
		Short: "show available extensions and which are enabled (.llz/extensions.yaml)",
		Args:  cobra.NoArgs,
		RunE:  func(_ *cobra.Command, _ []string) error { return runExtensionListEnabled(regRoot) },
	}
	enableC := &cobra.Command{
		Use:   "enable <name>",
		Short: "enable an extension (records it in .llz/extensions.yaml and scaffolds its files)",
		Args:  cobra.ExactArgs(1),
		RunE:  func(_ *cobra.Command, a []string) error { return runExtensionEnable(gopts, regRoot, a[0]) },
	}
	disableC := &cobra.Command{
		Use:   "disable <name>",
		Short: "disable an extension (removes it from .llz/extensions.yaml; leaves files in place)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, a []string) error {
			if err := runExtensionDisable(gopts, regRoot, a[0]); err != nil {
				return err
			}
			// Disable is non-destructive by design; point at the Decommission actions
			// that undo the scaffold/seed it leaves behind (the inverse arc).
			fmt.Fprintf(os.Stderr, "hint: files + seeded secrets are left in place — `llz extension teardown %s` removes its files; `llz extension unseed %s` revokes its secrets\n", a[0], a[0])
			return nil
		},
	}
	var reconcileRoot string
	var reconcileCheck bool
	reconcileC := &cobra.Command{
		Use:   "reconcile",
		Short: "run every contribution in lifecycle order over built-ins + enabled",
		Long: "The lifecycle driver: loads the unified Extension set (compiled-in built-ins +\n" +
			"enabled local/remote) and runs each Contribution in lifecycle order, derived\n" +
			"from the central registry — Scaffold (files), Configure (config), Gate (check),\n" +
			"Bootstrap (ci). One idempotent pass; --check reports drift without writing.\n" +
			"(`llz upgrade` fires Sustain/files; runLint fires the Gate via lifecycleGate.)",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runExtensionReconcile(gopts, reconcileRoot, reconcileCheck)
		},
	}
	reconcileC.Flags().StringVar(&reconcileRoot, "root", ".", "instance repo root")
	reconcileC.Flags().BoolVar(&reconcileCheck, "check", false, "report drift across all phases; write nothing")

	var doctorRoot string
	doctorC := &cobra.Command{
		Use:   "doctor",
		Short: "check declared vars/secrets across enabled extensions (Configure phase)",
		Long: "Reports declared vars/secrets that are unsatisfied across the enabled set —\n" +
			"a var with no default + no LLZ_VAR_<NAME> override, or a secret absent from the\n" +
			"environment. A missing required secret exits non-zero. (Fold into `llz doctor`\n" +
			"for the unified readiness gate.)",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runExtensionConfigDoctor(doctorRoot) },
	}
	doctorC.Flags().StringVar(&doctorRoot, "root", ".", "instance repo root")

	var seedRoot string
	seedC := &cobra.Command{
		Use:   "seed",
		Short: "wire enabled extensions' declared secrets into their stores (OpenBao / GH env) — needs --yes",
		Long: "Reads each declared secret's value from the environment and writes it to its\n" +
			"target — an OpenBao `bao: path#key` and/or a GitHub Environment `ghEnv:` —\n" +
			"reusing `llz openbao set` / `gh secret set`. Values are never stored in the repo\n" +
			"or printed. Cloud-mutating: --dry-run / no --yes prints the plan (targets only).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runExtensionSeed(gopts, seedRoot) },
	}
	seedC.Flags().StringVar(&seedRoot, "root", ".", "instance repo root")

	var rotateRoot, rotateOnly string
	rotateC := &cobra.Command{
		Use:   "rotate [name]",
		Short: "rotate token(s) for enabled extensions that implement the TokenRotator interface — needs --yes",
		Long: "Runs each enabled extension's rotate: (mint a fresh token) and re-seeds the\n" +
			"new value into its declared secret target (OpenBao/GH env), reusing the seed\n" +
			"machinery — the plugin side of the secret-rotation lifecycle. Cloud-mutating:\n" +
			"--dry-run / no --yes prints the plan. Pass [name] to rotate one extension.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, a []string) error {
			if len(a) == 1 {
				rotateOnly = a[0]
			}
			return runExtensionRotate(gopts, rotateRoot, rotateOnly)
		},
	}
	rotateC.Flags().StringVar(&rotateRoot, "root", ".", "instance repo root")

	var teardownRoot, teardownOnly string
	var teardownForce bool
	teardownC := &cobra.Command{
		Use:   "teardown [name]",
		Short: "remove an extension's scaffolded files (per the lock) and clear its lock entries — needs --yes",
		Long: "The inverse of `extension apply` (the Decommission phase): removes the files an\n" +
			"extension owns per .llz/extensions.lock and drops its lock entry. With no [name] it\n" +
			"tears down every recorded extension; pass one to scope it. Refuses to strip files an\n" +
			"enabled extension's check/ci hook still consumes (disable it first, or --force).\n" +
			"--dry-run / no --yes prints the plan and removes nothing.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, a []string) error {
			if len(a) == 1 {
				teardownOnly = a[0]
			}
			return runExtensionTeardown(gopts, teardownRoot, teardownOnly, teardownForce)
		},
	}
	teardownC.Flags().StringVar(&teardownRoot, "root", ".", "instance repo root")
	teardownC.Flags().BoolVar(&teardownForce, "force", false, "tear down even a still-enabled extension whose hooks depend on the files")

	var unseedRoot, unseedOnly string
	unseedC := &cobra.Command{
		Use:   "unseed [name]",
		Short: "revoke secrets an extension seeded — delete GH env secrets; print OpenBao removals — needs --yes",
		Long: "The inverse of `extension seed` (the Decommission phase): deletes each declared\n" +
			"secret's GitHub Environment entry (reusing the gh machinery `llz ci clear-cluster-\n" +
			"secrets` uses). OpenBao `bao:` targets are NOT auto-deleted — a path may hold sibling\n" +
			"keys — so the exact per-key removal is printed for the operator. With no [name] it\n" +
			"covers every enabled extension. Cloud-mutating: --dry-run / no --yes prints the plan.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, a []string) error {
			if len(a) == 1 {
				unseedOnly = a[0]
			}
			return runExtensionUnseed(gopts, unseedRoot, unseedOnly)
		},
	}
	unseedC.Flags().StringVar(&unseedRoot, "root", ".", "instance repo root")

	var provisionRoot string
	var provisionCheck bool
	provisionC := &cobra.Command{
		Use:   "provision",
		Short: "install enabled extensions' declared tools via mise (generates .mise.toml) — needs --yes",
		Long: "The host/local supply side (Configure-phase ActionProvision): aggregates every\n" +
			"enabled extension's declared tools (a pinned mise backend ref like `pipx:yamllint`)\n" +
			"into a generated .mise.toml and runs `mise install`. The extension declares WHAT to\n" +
			"install (pinned, registry-resolvable) — never HOW — so a remote extension cannot\n" +
			"smuggle host execution. Host-mutating: --dry-run / no --yes prints the plan; --check\n" +
			"reports config drift without installing.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runExtensionProvision(gopts, provisionRoot, provisionCheck)
		},
	}
	provisionC.Flags().StringVar(&provisionRoot, "root", ".", "instance repo root")
	provisionC.Flags().BoolVar(&provisionCheck, "check", false, "report .mise.toml drift and exit non-zero; install nothing")

	var syncRoot string
	var syncUpdate bool
	syncC := &cobra.Command{
		Use:   "sync",
		Short: "fetch git-pinned sources into the cache and lock their SHA+digest (--yes to fetch)",
		Long: "Clones each source in .llz/extensions.yaml at its pinned ref into a gitignored\n" +
			"cache and records the resolved commit SHA + content digest in the lock. A later\n" +
			"sync that sees a different SHA/digest for the same pin is a hard error (upstream\n" +
			"moved the ref) unless --update re-pins. Fetch is gated — needs --yes.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runExtensionSync(gopts, syncRoot, syncUpdate) },
	}
	syncC.Flags().StringVar(&syncRoot, "root", ".", "instance repo root")
	syncC.Flags().BoolVar(&syncUpdate, "update", false, "re-pin to the currently-fetched SHA/digest instead of failing on drift")

	for _, c := range []*cobra.Command{listC, enableC, disableC} {
		c.Flags().StringVar(&regRoot, "root", ".", "instance repo root")
	}

	x.AddCommand(newC, lintC, upgradeC, wiringC, ciWorkflowC, lifecycleC, applyC, excludeC, listC, enableC, disableC, syncC, doctorC, seedC, rotateC, teardownC, unseedC, provisionC, reconcileC)
	return x
}
