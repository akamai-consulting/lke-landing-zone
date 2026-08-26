package tofudriver

// tofu.go — `llz tofu`, the hand-run passthrough. See internal/shared/tfenc for
// why a hand-run `tofu` fails without help.
//
// IT IS NOT A WRAPPER: it adds no verbs, rewrites no arguments and interprets no
// output. It resolves the environment and execs OpenTofu. The one exception is
// `init`'s backend configuration, gated on --region for the reason recorded at
// withBackendConfig.
//
// A child cannot export into its parent shell, so nothing here can fix a terminal
// that is already open. That is why there are three modes rather than one: run
// the command from a process that has the environment, or have the shell evaluate
// what we print (--export / --shell-init).

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/exitcode"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/tfbin"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/tfenc"
)

// TofuOpts are the flags `llz tofu` accepts before the passthrough arguments.
type TofuOpts struct {
	// Region is the deployment whose state this root should init against. Only
	// consulted for `init`.
	Region string
	// StateKey overrides the derived <module>/<region>/terraform.tfstate. The
	// shared-VPC root is the case that needs it: its state is keyed by NETWORK,
	// not by deployment, so several deployments share one key.
	StateKey string
	// Export prints shell exports instead of running anything.
	Export bool
	// ShellInit prints the rc snippet that makes a bare `tofu` work.
	ShellInit bool
	// Stdout is not a terminal — set by the caller from a real isatty check, so
	// the secret-printing guard is testable.
	StdoutIsTerminal bool

	// DryRun and Yes are llz's global flags, threaded in from cliopts.Global by
	// the cobra layer.
	//
	// THEY WERE MISSING, AND --dry-run IS DOCUMENTED AS "print commands; change
	// nothing". `llz --dry-run tofu -- apply -auto-approve` therefore APPLIED —
	// a safety flag silently not working on the one command here that can destroy
	// infrastructure. `teardown` is the pattern: it reads both, and narrows its
	// capability binding to cloudBinding(Yes && !DryRun) so a dry run cannot POST.
	DryRun bool
	Yes    bool
}

// stateBackendRegion is the S3 backend's required `region`, and it is a dummy:
// Linode Object Storage is not AWS and ignores it, but the backend refuses to
// configure without one. Same literal the terraform-init composite action and
// fetch-kubeconfig-state pass.
const stateBackendRegion = "us-east-1"

// knownRoots are the module directories whose state key is <module>/<region>. A
// key is only DERIVED when standing in one: a wrong key does not fail, it reads a
// different (usually empty) state, and an apply against that proposes creating
// everything.
var knownRoots = map[string]bool{"cluster": true, "vpc": true, "object-storage": true, "databases": true}

// currentModule is the Terraform root this process is standing in, by directory
// name. "" when the working directory cannot be read, which knownRoots then
// rejects — the same refusal as standing somewhere that is not a root.
func currentModule() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return filepath.Base(cwd)
}

// mode is which of the three things this command does. Exactly one runs.
//
// NAMED AND DISPATCHED RATHER THAN A CHAIN OF EARLY RETURNS, because that shape
// produces ordering bugs — a check placed after a return that skips it. A mode is
// chosen once, and the policy below is evaluated in one place for all of them: a
// mode added later cannot skip a gate by returning before it, because it does not
// return at all. It declares what it does and is dispatched.
type mode int

const (
	modeExec      mode = iota // run OpenTofu
	modeExport                // hand an environment to a shell
	modeShellInit             // print an rc snippet
)

// effect is what a mode does to the world. --dry-run and --yes are decided from
// THIS, centrally, rather than at each mode's own return statement.
type effect struct {
	// writesLocal: creates or changes something on this machine. The export
	// file is the case — 0600, and carrying the passphrase.
	writesLocal bool
	// mutatesInfra: can change infrastructure, so it needs --yes.
	mutatesInfra bool
	// describe renders what would have happened, for --dry-run. Names only,
	// never secret values.
	describe string
	// argv is the FULLY RESOLVED command line, backend config included.
	//
	// CARRIED, NEVER RECOMPUTED — that is the point of splitting plan from run.
	// The rehearsal and the real run execute the same slice, so a dry run cannot
	// describe something other than what happens.
	argv []string
}

// RunTofu selects one mode, applies the policy every mode shares, and runs it.
func RunTofu(stdout, stderr io.Writer, o TofuOpts, args []string) error {
	m, err := selectMode(o, args)
	if err != nil {
		return err
	}

	// --shell-init reads nothing and touches nothing, so it works in a checkout
	// that has not been onboarded — which is exactly where someone sets up their
	// shell. It deliberately does not sweep either: it runs on every shell start
	// for anyone who installed the rc line, and it creates no file for a sweep to
	// find.
	if m == modeShellInit {
		fmt.Fprint(stdout, ShellInit())
		return nil
	}

	// Remove any export file a previous run left behind. `--export` without an
	// `eval` writes a passphrase nobody consumes, and this is the only thing that
	// ever cleans it up.
	tfenc.SweepStaleExports()

	local, err := tfenc.Hydrate(".")
	if err != nil {
		return err
	}
	if local.Root == "" {
		return fmt.Errorf("not inside an instance checkout — `llz tofu` resolves your Terraform environment from %s at the instance root, and there is none above this directory.\n"+
			"  Run it from your instance repo (a `terraform-iac-bootstrap/<root>` directory is the usual spot), or run OpenTofu directly if you meant to work outside an instance", tfenc.SecretsFile)
	}

	eff, err := plan(m, o, local, args)
	if err != nil {
		return err
	}

	// ── the policy every mode shares, in one place ───────────────────────────
	if eff.mutatesInfra && !o.Yes && !o.DryRun {
		return fmt.Errorf("`tofu %s` changes infrastructure, and llz gates cloud-mutating commands behind --yes (same as `llz build`, `llz reap`, `llz credentials`).\n"+
			"  Rehearse it:  %s\n"+
			"  Run it:       %s",
			subcommand(args),
			color.Cyan("llz --dry-run tofu -- "+strings.Join(args, " ")),
			color.Cyan("llz --yes tofu -- "+strings.Join(args, " ")))
	}
	if o.DryRun && (eff.writesLocal || eff.mutatesInfra) {
		fmt.Fprintln(stderr, "→ (dry-run) "+eff.describe)
		if names := varNames(local); names != "" {
			fmt.Fprintln(stderr, color.Dim("   with: "+names))
		}
		return nil
	}

	switch m {
	case modeExport:
		return runExport(stdout, stderr, o, local)
	default:
		return runExec(stdout, stderr, local, eff.argv)
	}
}

// plan resolves what a mode WOULD do, without doing any of it.
//
// Separating this from running is what makes --dry-run honest: the dry run and
// the real run take the same path to the same decision, so a rehearsal cannot
// describe something other than what would happen.
func plan(m mode, o TofuOpts, local tfenc.Local, args []string) (effect, error) {
	switch m {
	case modeExport:
		if !local.Has(tfenc.EnvVar) {
			return effect{}, missingEncryptionErr(local,
				"the shell you are setting up would fail on every state-touching command")
		}
		return effect{
			writesLocal: true,
			describe:    "write the Terraform environment to a private 0600 file and print the command that sources and deletes it",
		}, nil
	default:
		if needsEncryption(args) && !local.Has(tfenc.EnvVar) {
			return effect{}, missingEncryptionErr(local,
				fmt.Sprintf("`tofu %s` refuses to run without it", subcommand(args)))
		}
		full, err := withBackendConfig(o, local, args)
		if err != nil {
			return effect{}, err
		}
		return effect{
			mutatesInfra: mutates(args),
			describe:     tfbin.Bin() + " " + strings.Join(full, " "),
			argv:         full,
		}, nil
	}
}

// runExport hands the environment to a shell through a private file.
func runExport(stdout, stderr io.Writer, o TofuOpts, local tfenc.Local) error {
	// STILL REFUSED ON A TERMINAL, even though stdout carries no secret. On a TTY
	// there is almost certainly no `eval` around this, and the handoff would leave
	// an unconsumed passphrase in a temp directory — worse than the printing it
	// replaced, because nothing on screen says it happened.
	if o.StdoutIsTerminal {
		return fmt.Errorf("`--export` hands the environment to a shell; run on its own it would leave an unconsumed passphrase in a temp file.\n"+
			"  Feed it to your shell:      %s\n"+
			"  Better, and permanent:      %s\n"+
			"  Or skip the shell entirely: %s",
			color.Cyan(`eval "$(llz tofu --export)"`),
			color.Cyan(`eval "$(llz tofu --shell-init)"`),
			color.Cyan("llz tofu -- <args>"))
	}
	snippet, err := local.WriteExports()
	if err != nil {
		return err
	}
	fmt.Fprint(stdout, snippet)
	reportResolution(stderr, local)
	return nil
}

// runExec is the passthrough itself.
func runExec(stdout, stderr io.Writer, local tfenc.Local, argv []string) error {
	reportResolution(stderr, local)
	// CommandResolved, not Command: `local` is already resolved, and Command would
	// walk the filesystem and re-read the cache to reach the same answer.
	cmd := tfbin.CommandResolved(local.Environ(), argv...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, stdout, stderr
	// Ctrl-C reaches the whole process group, so llz gets a signal meant for the
	// child. Without this it dies instantly, handing the operator a prompt while
	// OpenTofu is still writing state — and exitcode's 128+N path becomes
	// unreachable, since llz is gone before cmd.Run() can return.
	defer ignoreInterrupts(stderr)()
	// FromExec is what makes this a passthrough rather than a wrapper: OpenTofu's
	// status is the answer the caller asked for. `plan -detailed-exitcode` returns
	// 2 for "changes pending", and collapsing that into 1 breaks every script that
	// reads it.
	return exitcode.FromExec(cmd.Run())
}

// escalateInterrupt abandons the child and lets llz die, restoring the default
// action and re-raising so it applies. A package var because the alternative to
// seaming it is a test that terminates the test binary to prove termination.
var escalateInterrupt = func() {
	if p, err := os.FindProcess(os.Getpid()); err == nil {
		_ = p.Signal(os.Interrupt)
	}
}

// ignoreInterrupts gives the FIRST interrupt to the child and the SECOND to the
// operator.
//
// The first is addressed to OpenTofu, which handles SIGINT and needs the time to
// finish writing state; Go's default action would kill llz while it did. But
// suppressing outright removes the escape hatch — against a child that IGNORES
// the signal, Ctrl-C would then do nothing at all — so a second press restores
// the default. The note is what keeps an absorbed interrupt from looking like a
// hang.
//
// signal.Notify with a channel nobody reads is the idiom for suppression;
// buffered at 2 so neither press is lost between receives. SIGINT only: a SIGTERM
// is addressed to llz and should still terminate it.
//
// THE NOTE IS WRITTEN FROM A GOROUTINE, concurrently with whatever the child is
// writing. That is safe for the real caller because w is os.Stderr — an *os.File,
// which os/exec hands to the child as a raw descriptor, so the two writes are
// independent syscalls rather than shared Go memory. A caller passing anything
// else (a test buffer) MUST make it safe for concurrent use; this deliberately
// does not wrap it, because wrapping cmd.Stderr in a non-*os.File would put a
// pipe between OpenTofu and the terminal and cost it the isatty check its colour
// and progress output depend on.
func ignoreInterrupts(w io.Writer) func() {
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt)
	done := make(chan struct{})
	go func() {
		select {
		case <-ch:
			fmt.Fprintln(w, color.Yellow("llz:"),
				"interrupt passed to OpenTofu, which stops after the step it is on — press Ctrl-C again to abandon it (state may be left mid-write)")
		case <-done:
			return
		}
		select {
		case <-ch:
			signal.Stop(ch)
			escalateInterrupt()
		case <-done:
		}
	}()
	return func() {
		close(done)
		signal.Stop(ch)
	}
}

// selectMode picks the one thing this invocation asked for, and refuses
// invocations that asked for two. Without it the modes swallow each other
// silently: `--export -- plan` exports, never runs plan, and exits 0.
func selectMode(o TofuOpts, args []string) (mode, error) {
	switch {
	case o.ShellInit && o.Export:
		return 0, fmt.Errorf("--shell-init and --export are different answers to the same question: --shell-init prints an rc snippet that routes a bare `tofu` through llz, --export hands one shell an environment. Pick one")
	case o.ShellInit && (o.Region != "" || o.StateKey != ""):
		return 0, fmt.Errorf("--shell-init prints an rc snippet and runs no OpenTofu, so --region/--state-key have nothing to configure")
	case o.ShellInit && len(args) > 0:
		return 0, fmt.Errorf("--shell-init prints an rc snippet and runs no OpenTofu, so it cannot also run `%s`.\n"+
			"  Install the snippet, then a bare `tofu` carries the environment:  %s",
			strings.Join(args, " "), color.Cyan(`eval "$(llz tofu --shell-init)"`))
	case o.ShellInit:
		return modeShellInit, nil

	case o.Export && len(args) > 0:
		return 0, fmt.Errorf("--export hands your shell an environment; it does not run OpenTofu, so `%s` would be silently dropped.\n"+
			"  Set the shell up, then run it:  %s\n"+
			"  Or do it in one step:           %s",
			strings.Join(args, " "),
			color.Cyan(`eval "$(llz tofu --export)" && tofu `+strings.Join(args, " ")),
			color.Cyan("llz tofu -- "+strings.Join(args, " ")))
	case o.Export && (o.Region != "" || o.StateKey != ""):
		return 0, fmt.Errorf("--region/--state-key configure an `init` backend, and --export runs no OpenTofu — the exported environment is the same either way")
	case o.Export:
		return modeExport, nil

	case len(args) == 0:
		return 0, fmt.Errorf("usage: llz tofu [--region <env>] -- <tofu args>\n" +
			"  e.g. llz tofu --region primary -- init -upgrade\n" +
			"       llz tofu -- plan -var-file=primary.tfvars")
	}
	return modeExec, nil
}

// varNames lists what would be set, by NAME. Never by value: a dry run is read by
// exactly the people who paste terminal output into tickets.
func varNames(local tfenc.Local) string {
	var names []string
	for _, v := range local.Vars {
		names = append(names, v.Name)
	}
	return strings.Join(names, ", ")
}

// readOnlyVerbs are the subcommands that cannot change infrastructure. An
// allow-list, so it fails closed. `init` is here because it writes `.terraform`,
// not the cloud; `plan` has to stay frictionless or the gate just teaches people
// to type --yes reflexively. `state`/`workspace` are resolved a token deeper.
var readOnlyVerbs = map[string]bool{
	"": true, "version": true, "fmt": true, "validate": true, "help": true,
	"init": true, "plan": true, "show": true, "graph": true, "providers": true,
	"output": true, "console": true, "metadata": true,
}

// mutatingFlags are the flags that make an otherwise-read-only VERB change state.
// Same rule as readOnlySubVerbs — the danger can live past the verb —
// applied to flags: `-migrate-state` rewrites remote state between backends and
// `-reconfigure` discards the backend association. `init` is the only read-only
// verb with such a flag.
var mutatingFlags = map[string]map[string]bool{
	"init": {"-migrate-state": true, "-reconfigure": true, "-force-copy": true},
}

// readOnlySubVerbs are the safe halves of the two verbs whose danger lives in
// their SECOND word.
var readOnlySubVerbs = map[string]map[string]bool{
	"state":     {"list": true, "show": true, "pull": true},
	"workspace": {"list": true, "show": true, "select": true},
}

// mutates reports whether this invocation must pass llz's --yes gate.
//
// A gate at all, when `tofu apply` already prompts, because `-auto-approve`
// exists and because the extension declares cloud-mutate while root.go's help
// says those execute only with --yes. Gating on whether OpenTofu would prompt
// would mean reimplementing its rules here.
func mutates(args []string) bool {
	verb := subcommand(args)
	if flags, ok := mutatingFlags[verb]; ok {
		for _, a := range args {
			// `-flag` and `-flag=value` both count; the value is irrelevant, the
			// presence is the decision.
			if name, _, _ := strings.Cut(a, "="); flags[name] {
				return true
			}
		}
	}
	if sub, ok := readOnlySubVerbs[verb]; ok {
		for i, a := range args {
			if a == verb && i+1 < len(args) {
				return !sub[args[i+1]]
			}
		}
		return true // `tofu state` alone is a usage error; treat it as unsafe
	}
	return !readOnlyVerbs[verb]
}

// needsEncryption reports whether this invocation will consult the `encryption`
// block. An allow-list, so it FAILS CLOSED — a verb OpenTofu has not shipped yet
// needs the key rather than meeting OpenTofu's "Invalid expression".
//
// Measured against 1.12.6 with the shipped encryption.tf: version/fmt/validate run
// clean without TF_ENCRYPTION; init/plan/providers/graph/show/output/console all
// fail on it. Gating them all would make the passthrough stricter than the thing
// it wraps.
func needsEncryption(args []string) bool {
	switch subcommand(args) {
	case "", "version", "fmt", "validate", "help":
		return false
	}
	return true
}

// subcommand returns the OpenTofu verb in args, "" if there is none.
//
// NOT args[0]: OpenTofu takes global options BEFORE the subcommand, so anchoring
// on the first token reads `-chdir=…` as the verb and makes --region a silent
// no-op. `-help`/`-version` mean the verb never runs, so they resolve to their
// subcommand spellings.
//
// Measured against 1.12.6: `-chdir=DIR` is the only global option that continues
// to a subcommand and it requires the `=` form; anything else before the verb is
// rejected outright. Skipping unknown leading flags is therefore defensive.
func subcommand(args []string) string {
	for _, a := range args {
		switch {
		case a == "-help" || a == "--help":
			return "help"
		case a == "-version" || a == "--version":
			return "version"
		case strings.HasPrefix(a, "-"):
			continue // a global option; the verb is still ahead
		default:
			return a
		}
	}
	return ""
}

// hasChdir reports whether args carry OpenTofu's -chdir, which moves the
// directory the command actually operates in.
func hasChdir(args []string) bool {
	for _, a := range args {
		// The `=` form ONLY. OpenTofu rejects a space-separated value outright
		// ("must include an equals sign followed by a directory path"), verified
		// against 1.12.6 — so a bare `-chdir` never reaches a real invocation, and
		// matching it was dead code pretending to be defensive.
		if strings.HasPrefix(a, "-chdir=") {
			return true
		}
	}
	return false
}

// withBackendConfig appends the S3 backend flags to an `init` that did not bring
// its own.
//
// ONLY FOR `init`, ONLY WITH AN EXPLICIT --region, and only in a known root. The
// state key selects WHICH CLUSTER'S STATE this directory operates on, and
// OpenTofu does not validate it: a key nothing has written yet initializes
// cleanly against an EMPTY state, at which point `plan` proposes building a
// second cluster and `destroy` reports there is nothing to destroy. Inferring
// the deployment from a spec with one environment would work right up until the
// instance has two, and then it would be wrong silently. So it is typed, or it
// is not supplied — and an init without it simply behaves as it does today.
func withBackendConfig(o TofuOpts, local tfenc.Local, args []string) ([]string, error) {
	if subcommand(args) != "init" || (o.Region == "" && o.StateKey == "") {
		return args, nil
	}
	// -chdir MOVES THE DIRECTORY OpenTofu operates in, and the state key is
	// derived from THIS process's working directory. Deriving one anyway is the
	// worst available outcome and the tempting one: `--region prod --
	// -chdir=../vpc init` would produce `cluster/prod/terraform.tfstate` while
	// OpenTofu initialises `vpc/` — the wrong root's state, silently, which is the
	// whole reason this function refuses to guess. So it refuses here too rather
	// than reading the path out of the flag and hoping the two agree.
	// ONLY --region is unsafe: it derives the root from THIS working directory
	// while -chdir moves the one OpenTofu uses, so `--region prod -- -chdir=../vpc
	// init` would point the init at `cluster/prod`'s state while initialising
	// `vpc/`. --state-key names the key outright, so it stays legal.
	if o.StateKey == "" && hasChdir(args) {
		return nil, fmt.Errorf("--region derives the state key from the directory you are standing in (%s), and -chdir moves the one OpenTofu will use — deriving from the wrong one would point this init at another root's state.\n"+
			"  cd into the root you mean and drop -chdir:  %s\n"+
			"  or name the key outright, which -chdir cannot invalidate:\n      %s",
			currentModule(),
			color.Cyan("cd <root> && llz tofu --region "+o.Region+" -- init"),
			color.Cyan("llz tofu --state-key <root>/"+o.Region+"/terraform.tfstate -- -chdir=… init"))
	}
	for _, a := range args {
		if strings.HasPrefix(a, "-backend-config") {
			return args, nil // the caller is driving; do not second-guess it
		}
	}
	bucket := local.Value("TF_STATE_BUCKET")
	if bucket == "" {
		return nil, fmt.Errorf("TF_STATE_BUCKET is not set and is not in %s — it names the object-storage bucket holding your state, and `init` cannot configure the backend without it.\n"+
			"  `llz tokens` writes it to %s; a workflow reads it from the repository variable of the same name",
			filepath.Join(local.Root, tfenc.VarsFile), tfenc.VarsFile)
	}
	key := o.StateKey
	if key == "" {
		module := currentModule()
		if !knownRoots[module] {
			return nil, fmt.Errorf("--region derives the state key as <root>/%s/terraform.tfstate, and %q is not one of the Terraform roots (cluster, vpc, object-storage, databases).\n"+
				"  cd into the root you mean, or pass the key yourself with --state-key", o.Region, module)
		}
		key = module + "/" + o.Region + "/terraform.tfstate"
	}
	// A FRESH SLICE. `append(args, …)` writes into cobra's backing array whenever
	// it has spare capacity, so the caller's argv would be mutated by a function
	// that only claims to derive one. Harmless today — cobra does not read args
	// after RunE — but it is a shared buffer being written by a helper, which is
	// the kind of thing that is fine until someone reuses the input.
	out := append([]string(nil), args...)
	return append(out,
		"-backend-config=bucket="+bucket,
		"-backend-config=key="+key,
		"-backend-config=region="+stateBackendRegion,
	), nil
}

// missingEncryptionErr names the SECRET, not the variable: an operator who reads
// "TF_ENCRYPTION is unset" goes looking for a variable to set, which is the
// twelve-line document they should never write by hand.
func missingEncryptionErr(local tfenc.Local, because string) error {
	return fmt.Errorf("%s is not set and cannot be built: %s is absent from your environment and from %s.\n"+
		"  Your Terraform roots encrypt state at rest, so %s — that is the tripwire working, not a broken checkout.\n"+
		"  • gathered credentials before?   %s\n"+
		"  • have the passphrase to hand?   %s\n"+
		"  Losing it makes every state file unrecoverable; escrow it offline. See docs/adr/0007-terraform-state-encryption.md",
		tfenc.EnvVar, tfenc.PassphraseEnv, filepath.Join(local.Root, tfenc.SecretsFile), because,
		color.Cyan("llz tokens"),
		color.Cyan("export "+tfenc.PassphraseEnv+"=…"))
}

// reportResolution says what was resolved and what was not, one line each.
//
// The gaps are a WARNING and not a refusal: which variables a given command needs
// is not knowable from here — `fmt` needs none, `plan` needs the backend
// credentials — and a partly-populated cache is a normal state to debug in.
// OpenTofu's own error is the authority; this makes it legible when it arrives.
func reportResolution(w io.Writer, local tfenc.Local) {
	if n := len(local.Vars); n > 0 {
		fmt.Fprintln(w, color.Dim("llz: "+tfenc.ResolvedNote(n)))
	}
	if len(local.Missing) == 0 {
		return
	}
	var names []string
	for _, m := range local.Missing {
		// Only show the arrow when the two names actually differ. Printing
		// "TF_STATE_BUCKET (→ TF_STATE_BUCKET)" reads like a bug in the tool.
		if m.Key == m.Provides {
			names = append(names, m.Key)
			continue
		}
		names = append(names, fmt.Sprintf("%s (→ %s)", m.Key, m.Provides))
	}
	fmt.Fprintln(w, color.Yellow("llz:"), "not resolved from your environment or "+tfenc.SecretsFile+": "+strings.Join(names, ", "))
	fmt.Fprintln(w, color.Dim("     `llz tokens` gathers these; a command that does not need them will run fine without."))
}

// ShellInit returns the rc snippet that makes a BARE `tofu` carry the instance
// environment.
//
// WHY A FUNCTION AND NOT AN ALIAS: an alias would not survive `command tofu`,
// would not fall through when llz is absent, and cannot be exported to scripts.
// The function shadows the binary for interactive use only, and delegates to the
// same code path as `llz tofu --`, so there is one behavior and not two.
//
// NO RECURSION: `llz tofu` execs the resolved binary through tfbin (a direct
// exec of the program, not a shell), so the function never re-enters itself.
//
// It is safe outside an instance too: tfenc.Hydrate finds no instance root,
// contributes nothing, and OpenTofu runs exactly as it would have.
func ShellInit() string {
	return `# llz — make a bare ` + "`tofu`" + ` carry this instance's Terraform environment.
# Add to your shell rc:   eval "$(llz tofu --shell-init)"
#
# Outside an instance checkout this changes nothing: llz finds no .llz cache,
# adds no variables, and execs OpenTofu unmodified.
tofu() {
  if command -v llz >/dev/null 2>&1; then
    llz tofu -- "$@"
  else
    command tofu "$@"
  fi
}
`
}
