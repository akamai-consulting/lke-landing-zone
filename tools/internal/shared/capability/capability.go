// Package capability turns a binding's DECLARED GRANTS into the handles it can
// actually use. It is the implementation of the extension model's Decision 3 --
// "the grant IS the handle".
//
// WITHOUT IT, a binding declaring `cluster-read` and one declaring `cluster-write`
// receive the identical `func(name string, args ...string)` exec seam, and either
// can run `kubectl delete`, `bao`, `linode-cli` or anything else on PATH. Grants
// get checked against a state table at test time and handed no teeth, so the
// ceiling describes a permission system that does not exist.
//
// THE SURFACE IS MUCH NARROWER THAN THE SEAM COUNT SUGGESTS, which is what makes
// this tractable. Measured across internal/extensions:
//
//	kubectl read (get/describe/logs/wait/config)   78 sites, 17 packages
//	kubectl write (annotate/delete/rollout/patch…)  17 sites,  8 packages
//	tofu / git / gh                                  7 sites,  4 packages
//
// Nine of the seventeen kubectl-using packages never write. The "general purpose
// exec" was a kubectl client in 95% of its uses, and the escape hatch it provided
// was being used by four packages for three tools.
//
// SO THE READER TAKES ARGV AND POLICES THE VERB, rather than exposing one method
// per kubectl verb. That choice is deliberate and is the whole enforcement point:
//
//   - Per-verb methods would be prettier and would enforce at COMPILE time, but
//     they would rewrite 78 call sites that already have their argv assembled, and
//     several build it dynamically (`Kubectl(waitArgs...)`, `Exec("kubectl",
//     args...)`) where there is no static verb to dispatch on.
//   - Taking argv WITHOUT policing the verb would be the status quo wearing a new
//     type name: a caller holding a "reader" would pass "delete" and it would
//     work. That is worse than the current state, because it would look enforced.
//
// So argv in, verb checked, write verbs refused. The check is a runtime error
// rather than a compile error -- weaker, and honest about being weaker -- but it
// is the difference between a grant that constrains and a grant that annotates.
//
// UNGRANTED CAPABILITIES ARRIVE NON-NIL AND REFUSING. A binding that did not
// declare cluster-write still gets a ClusterWriter; it returns an error naming the
// grant it would need. A nil handle would panic at the call site and be reported as
// a crash rather than as a permission fault, and — worse — would tempt callers into
// `if w != nil` guards that silently skip the work.
package capability

import (
	"fmt"
	"os/exec"
	"sort"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/baoread"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

// readVerbs are the kubectl subcommands a cluster-read handle may run.
//
// THE LIST IS CLOSED AND THE DEFAULT IS REFUSAL. An unknown verb is rejected
// rather than allowed, because kubectl grows subcommands and the ones it grows are
// as likely to mutate as not — `kubectl debug` did exactly that. A allowlist that
// fails closed needs a line added when a genuinely new read verb appears; a
// denylist that fails open silently widens every reader in the tree.
var readVerbs = map[string]bool{
	"get": true, "describe": true, "logs": true, "top": true,
	"version": true, "explain": true, "api-resources": true, "api-versions": true,
	// `wait` blocks on a condition and changes nothing.
	"wait": true,
	// `config` reads kubeconfig; `config set-*` writes it, and that is caught
	// below rather than here, because the mutation is in the SECOND word.
	"config": true,
	// `auth can-i` asks what the caller may do; `auth reconcile` WRITES RBAC. Same
	// shape as config and rollout, so the judgement is in subverbReads and this
	// entry only says the verb is reachable at all.
	"auth": true,
}

// writeVerbs are what a cluster-write handle adds. Listed explicitly rather than
// derived as "not a read verb", so that a verb nobody has classified is refused by
// BOTH handles and shows up as a decision someone has to make.
var writeVerbs = map[string]bool{
	"apply": true, "patch": true, "delete": true, "create": true, "replace": true,
	"rollout": true, "annotate": true, "label": true, "scale": true, "edit": true,
	"set": true, "cp": true, "drain": true, "cordon": true, "uncordon": true,
	"taint": true, "expose": true, "autoscale": true,
	// `exec` runs a command inside a pod. It is classified as a WRITE despite
	// often being used to read, because what it may do is bounded by the
	// container's entrypoint and not by kubectl.
	"exec": true,
	// `debug` attaches an ephemeral container — a mutation of the pod spec.
	"debug": true, "port-forward": true, "proxy": true, "attach": true,
}

// subverbReads lists, for the verbs whose MEANING IS IN THE SECOND WORD, which
// subcommands READ. It is an allowlist for the same reason the verb table is, and
// it was written the other way round first: as `subverbWrites`, permitting
// anything not listed. That fails OPEN — `kubectl rollout <anything-unlisted>`
// sailed through a read-only handle, which the reader test caught on the first
// run. An unknown subcommand of a mutating verb is refused. `config view` reads and `config set-context` rewrites
// kubeconfig; `rollout status` blocks on a condition and `rollout restart` rolls
// pods. A verb-only check gets both of these wrong, and it got `rollout status`
// wrong until the write census listed the argv side by side and the read was
// sitting in the middle of them.
var subverbReads = map[string]map[string]bool{
	"config": {
		"view": true, "current-context": true, "get-contexts": true,
		"get-clusters": true, "get-users": true,
	},
	"rollout": {
		// `rollout status` BLOCKS ON A CONDITION and changes nothing. The verb-only
		// check called it a write; the census exposed that by listing `rollout
		// status deploy/argocd-redis --timeout=120s` directly beneath `rollout
		// restart deploy/argocd-redis` in converge.
		"status": true, "history": true,
	},
	// `auth` WAS CLASSIFIED VERB-ONLY AND THAT WAS AN RBAC HOLE. The readVerbs
	// entry was justified in a comment naming `auth can-i` — "the one verb whose
	// whole purpose is to be safe to call" — which is true of that subcommand and
	// of no other. `kubectl auth reconcile -f rbac.yaml` CREATES AND UPDATES
	// ClusterRoles and ClusterRoleBindings, and it went through a cluster-read
	// handle clean, because nothing looked at the second word.
	//
	// It is the same defect config and rollout were already fixed for, found the
	// same way and one verb later: a verb whose meaning is in the second word,
	// classified on the first. The lesson worth carrying is that the JUSTIFYING
	// COMMENT named a subcommand while the CODE keyed on the verb — whenever those
	// two disagree, the table is the wrong shape rather than the entry being wrong.
	//
	// `can-i` alone. `auth whoami` reads too and is deliberately absent: no caller
	// in this tree runs it, and this table follows the earned-row rule the ceiling
	// tables follow. An unlisted subcommand is refused, so adding one is a line.
	"auth": {"can-i": true},
}

// dryRunFlag is the verbs for which `--dry-run` IS A KUBECTL FLAG, and therefore
// the only verbs whose argv can carry one meaningfully.
//
// ─────────────────────────────────────────────────────────────────────────────
// THE EXEMPTION USED TO BE UNSCOPED AND IT WAS A HOLE IN BOTH DIRECTIONS.
//
// Permits() scanned the WHOLE argv for a `--dry-run` prefix, before classifying
// anything, and returned nil on a match. Two consequences, both probed:
//
//	kubectl exec pod -- helm upgrade --dry-run     -> permitted, cluster-read
//	kubectl frobnicate --dry-run=server            -> permitted, cluster-read
//
// The first is the live one. kubectl STOPS PARSING AT `--`, so that `--dry-run`
// belongs to the container's command line and kubectl runs the exec for real —
// arbitrary in-pod execution out of a read grant. The second contradicted this
// file's own rule three lines further down ("Unclassified: refused by both
// handles, on purpose"), because the scan ran before the classification it was
// meant to be an exception to.
//
// SO THE EXEMPTION IS NOW SCOPED THREE WAYS: to kubectl's own flags (the argv
// before `--`), to a verb that is CLASSIFIED as a write, and to a verb kubectl
// actually implements the flag for. Any one of the three would have closed the
// exec case; all three are cheap, and the third is what keeps a verb that runs a
// PROGRAM (exec, cp, attach, port-forward, proxy, debug) from ever being reached
// by this path at all, whatever a future argv looks like.
//
// THE ROWS ARE TRANSCRIBED FROM KUBECTL, not judged. That is the difference
// between this table and the ceiling tables next door: which verbs accept
// `--dry-run` is an external fact anyone can check against `kubectl <verb> -h`,
// so listing the ones that do is not the speculative widening the earned-row rule
// exists to prevent. `apply` is the only one a shipping caller uses today
// (assert-network's wave-health gate, `apply --dry-run=server -f -`); the rest are
// here so a second caller is not tempted to widen the rule instead of the table.
// ─────────────────────────────────────────────────────────────────────────────
var dryRunFlag = map[string]bool{
	"apply": true, "create": true, "delete": true, "patch": true, "replace": true,
	"set": true, "annotate": true, "label": true, "scale": true, "taint": true,
	"expose": true, "autoscale": true,
}

// Cluster is the handle a cluster-read or cluster-write binding receives. Both
// grants produce the same INTERFACE and different permissions — a caller cannot
// tell from the type which it holds, and should not try: it runs its argv and
// handles the error, exactly as it does today when kubectl itself refuses.
type Cluster interface {
	// Run executes kubectl with args and returns stdout.
	Run(args ...string) ([]byte, error)
	// Combined returns stdout+stderr and ignores exit status, for the callers that
	// classify a message rather than branch on success.
	Combined(args ...string) string
	// Permits reports whether this handle would allow argv, without running it. It
	// exists so a caller can fail early with its own message rather than
	// discovering the refusal mid-sequence.
	Permits(args ...string) error
}

type cluster struct {
	exec func(string, ...string) ([]byte, error)
	comb func(string, ...string) string
}

// Verb extracts the kubectl subcommand from an argv, skipping leading global
// flags and their values. Exported because the refusal message and the tests both
// need to agree with the checker about what the verb IS.
func Verb(args []string) string {
	// Flags that take a separate value; anything else beginning with - is either a
	// boolean flag or --flag=value, and consumes only itself.
	valued := map[string]bool{
		"-n": true, "--namespace": true, "--kubeconfig": true, "--context": true,
		"--cluster": true, "--user": true, "--as": true, "--as-group": true,
		"--request-timeout": true, "-s": true, "--server": true,
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			return a
		}
		if valued[a] {
			i++
		}
	}
	return ""
}

func (c cluster) Permits(args ...string) error {
	v := Verb(args)
	if v == "" {
		return fmt.Errorf("capability: refusing a kubectl call with no subcommand (argv %q) — "+
			"the verb is what the grant is checked against, so an argv that has none cannot be judged", args)
	}
	// Verbs whose mutation is in the SECOND word are judged on that word.
	if reads, split := subverbReads[v]; split {
		sub := secondWord(args, v)
		if reads[sub] {
			return nil // `config view`, `rollout status`
		}
		if sub == "" {
			return fmt.Errorf("capability: kubectl %s needs a subcommand — %s alone cannot be "+
				"judged read or write", v, v)
		}
		return c.deny(v + " " + sub)
	}
	if readVerbs[v] {
		return nil
	}
	if writeVerbs[v] {
		// A DRY RUN IS A READ, but only where it is one. `apply --dry-run=server`
		// sends the object to the apiserver for validation and admission and
		// persists nothing — assert-network's wave-health gate is built entirely on
		// that, and refusing it would force a read-only gate to declare
		// cluster-write for a call that cannot change anything. `--dry-run=client`
		// never reaches the cluster at all.
		//
		// The exemption sits HERE, inside the classified-write branch and after the
		// second-word verbs have already been answered, rather than ahead of the
		// whole check where it started. See dryRunFlag for what it let through
		// there.
		if dryRunFlag[v] && hasDryRun(args) {
			return nil
		}
		// A GENERIC WRITE IS NEVER PERMITTED THROUGH THIS HANDLE, whatever the
		// binding declared. Writes go through the named operations on Writer, which
		// is the whole point of the granular pass: `cluster-write` used to mean "any
		// kubectl mutation", including `drain`, `exec` and `delete namespace`. It now
		// means the nine specific shapes writer.go measured, and a reviewer sees Annotate/Delete/PatchMerge
		// in the diff rather than an argv they have to parse.
		return c.deny(v)
	}
	// Unclassified: refused by both handles, on purpose.
	return fmt.Errorf("capability: kubectl %q is not a classified verb — add it to readVerbs or "+
		"writeVerbs in internal/shared/capability, which is a decision about whether it mutates", v)
}

func (c cluster) deny(v string) error {
	return fmt.Errorf("capability: kubectl %s is a mutation and cannot go through a cluster read handle — "+
		"declare %q and call the named operation on Writer (%s) instead of assembling an argv",
		v, extension.ClusterWrite, strings.Join(WriterOperations(), ", "))
}

// hasDryRun reports whether KUBECTL was given a dry-run flag — which means the
// argv before the `--` separator, and nothing after it.
//
// EVERYTHING AFTER `--` BELONGS TO SOMETHING ELSE. kubectl stops parsing there and
// hands the rest to the container (`exec`, `debug`, `attach`, `run`), so a flag
// found past it is the OTHER program's flag. Reading one as kubectl's is how a
// read handle came to permit `kubectl exec pod -- helm upgrade --dry-run`.
func hasDryRun(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		// `--dry-run=none` is an explicit request to really do it, and a bare
		// `--dry-run` is kubectl's deprecated spelling of a client-side one.
		if strings.HasPrefix(a, "--dry-run") && a != "--dry-run=none" {
			return true
		}
	}
	return false
}

func secondWord(args []string, verb string) string {
	for i, a := range args {
		if a == verb && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func (c cluster) Run(args ...string) ([]byte, error) {
	if err := c.Permits(args...); err != nil {
		return nil, err
	}
	return c.exec("kubectl", args...)
}

func (c cluster) Combined(args ...string) string {
	if err := c.Permits(args...); err != nil {
		// Combined's contract is "text out, no error", and its callers match on the
		// text. Returning the refusal AS the text keeps that contract and makes the
		// refusal visible in exactly the place the caller is already looking.
		return err.Error()
	}
	return c.comb("kubectl", args...)
}

// deniedCluster is what a binding that declared NEITHER cluster grant receives.
// It is not nil: a nil handle panics at the call site and reads as a crash rather
// than as a permission fault.
type deniedCluster struct{}

func (deniedCluster) Permits(args ...string) error {
	return fmt.Errorf("capability: this binding declared neither %q nor %q, so it has no cluster handle "+
		"(attempted: kubectl %s)", extension.ClusterRead, extension.ClusterWrite, Verb(args))
}
func (d deniedCluster) Run(args ...string) ([]byte, error) { return nil, d.Permits(args...) }
func (d deniedCluster) Combined(args ...string) string     { return d.Permits(args...).Error() }

// Handles is what a binding receives. Every field is non-nil; a capability the
// binding did not declare is present and refuses.
type Handles struct {
	// Cluster READS. It is read-only for every binding, including one holding
	// cluster-write — mutations do not go through an argv at all.
	Cluster Cluster
	// Writer is the named mutations (WriterOperations lists them), present-and-refusing
	// without cluster-write.
	Writer Writer
	// Secrets READS OpenBao, for a binding declaring secret-read or secret-custody.
	// Its Get returns a VERDICT rather than a bool: a refusal is Unknown, never
	// Absent, so a narrower grant can never invite an overwrite. See secrets.go.
	Secrets Secrets
	// Custodian PLACES secret material, for secret-custody only.
	Custodian Custodian
	// BaoAdmin is OpenBao's NON-KV surface: policy, auth, token, operator and
	// status. Reads need secret-read or secret-custody; anything changing who may
	// reach what needs secret-custody. See baoadmin.go for why it is not a seventh
	// grant.
	BaoAdmin BaoAdminHandle
	// Forge is the `gh` CLI, gated by THREE grants rather than two: reads need
	// cloud-read or read-repo, changes need cloud-mutate, and setting a repository
	// secret needs secret-custody. See forge.go for why that split is not invented.
	Forge Forge
	// Cloud is the Linode API, for cloud-read and cloud-mutate.
	//
	// IT USED TO BE REACHABLE ONLY THROUGH CloudFor, WHICH MADE THIS STRUCT'S OWN
	// DOCSTRING FALSE. `For` claimed to build "the handles a binding's declared
	// grants entitle it to" while answering for four of the eight grants that have
	// a handle at all — a caller that took For(b) and stopped had no cloud handle
	// and nothing told it so. CloudFor still exists and still works; this is the
	// same value, reachable from the place the doc points at.
	Cloud Cloud
}

// THE REPO GRANTS ARE THE ONE PAIR THIS STRUCT CANNOT CARRY, and the reason is a
// property of read-repo rather than an omission.
//
// A repo handle is a FENCE AROUND A ROOT: RepoAt(b, root), RepoForGate(e, root),
// RepoContaining(b, dir). The root is what makes it a capability rather than an
// unbounded os.ReadFile, and a Binding does not know one — the gate driver
// resolves it by walking up for a `.git` (registry/gates.go), and a transition
// takes it from a flag. Handing back a repo handle here would mean choosing a root
// on the caller's behalf, and the wrong root is exactly the defect the driver's
// hardcoded `..` turned out to be: a correct fence around the wrong tree.
//
// So read-repo and write-repo keep their own constructors, and this comment is
// here so that is a stated boundary rather than something a reader has to notice.

// For builds the handles a binding's declared grants entitle it to — every grant
// except read-repo and write-repo, which need a root and are documented above.
//
// It reads the BINDING, not the extension: grants are per-binding precisely so
// that an extension holding a read assertion and a write transition cannot use the
// transition's grant from inside the assertion. Passing the whole extension here
// would hand back the union and undo that.
//
// AND IT HONOURS THE BINDING'S KIND, not only its grants. A gate and an assertion
// each carry a BLANKET rule — read-repo alone, and read grants only — which the
// validator enforces over what may be WRITTEN. Nothing enforced it over what runs:
// this function read b.Grants and never asked whether the binding was legal, so a
// self-contradictory declaration (an assertion holding cluster-write) would have
// been handed the write handle at runtime, and `llz extension list` would have
// shown a reach the validator would have refused.
//
// Applying the two blanket rules HERE is not the ceiling tables arriving at
// runtime, and the difference matters. bindableStates and grantStates are lookups
// keyed on a STATE, and re-checking those would mean an operator seeing an error
// about a developer's mistake — the argument registry.Validate makes for staying a
// build-time lint, which stands. A kind's blanket rule needs no table and no state:
// it is a property of the kind alone, it costs a switch, and it fails in the safe
// direction by handing back LESS. The declaration cannot contradict itself on the
// way to a handle.
func For(b extension.Binding) Handles {
	// The blanket rules, in the direction that narrows. A gate reaches files and
	// nothing else; an assertion observes and does not change what it is measuring.
	switch b.Kind {
	case extension.Gate:
		return Handles{Cluster: deniedCluster{}, Writer: deniedWriter{}, Secrets: DeniedSecrets(),
			Custodian: DeniedCustodian(), Forge: DeniedForge(), BaoAdmin: DeniedBaoAdmin(),
			Cloud: DeniedCloud()}
	case extension.Assertion:
		b = readOnlyGrants(b)
	}

	var read, write bool
	for _, g := range b.Grants {
		switch g {
		case extension.ClusterRead:
			read = true
		case extension.ClusterWrite:
			write = true
		}
	}
	// cluster-write implies the ability to read: every mutating lane in the tree
	// reads back what it wrote, and forcing it to declare both would make the
	// grant line noisier without making it more informative.
	var w Writer = deniedWriter{}
	if write {
		w = writer{exec: kubectlprobe.Exec, stdin: execStdin}
	}
	sec, cust := secretHandles(b)
	fg := forgeHandle(b)
	ba := baoAdminHandle(b, baoread.ExecFn)
	cl := CloudFor(b)
	if !read && !write {
		return Handles{Cluster: deniedCluster{}, Writer: w, Secrets: sec,
			Custodian: cust, Forge: fg, BaoAdmin: ba, Cloud: cl}
	}
	return Handles{
		Cluster:   cluster{exec: kubectlprobe.Exec, comb: kubectlprobe.Combined},
		Writer:    w,
		Secrets:   sec,
		Custodian: cust,
		Forge:     fg,
		BaoAdmin:  ba,
		Cloud:     cl,
	}
}

// readOnlyGrants drops the mutating grants from a binding, for the ASSERTION rule.
//
// It NARROWS RATHER THAN REFUSING, which is the right failure direction here and
// worth saying why. Validate() already rejects this binding, so anything reaching
// here is either a declaration that has not been through the lint or one minted at
// a call site — and in both cases the useful outcome is that the read half still
// works and the write half does not. Refusing outright would turn a
// mis-declaration into an outage; handing out the union would make the validator's
// rule decorative, which is the state this whole layer exists to end.
func readOnlyGrants(b extension.Binding) extension.Binding {
	kept := make([]extension.Grant, 0, len(b.Grants))
	for _, g := range b.Grants {
		if extension.IsReadOnly(g) {
			kept = append(kept, g)
		}
	}
	b.Grants = kept
	return b
}

// WithExec is For with the process seam replaced, for tests that must not shell
// out. It takes the same Binding so a test cannot accidentally grant itself more
// than the declaration allows.
func WithExec(b extension.Binding, exec func(string, ...string) ([]byte, error), comb func(string, ...string) string) Handles {
	h := For(b)
	if c, ok := h.Cluster.(cluster); ok {
		c.exec, c.comb = exec, comb
		h.Cluster = c
	}
	if _, ok := h.Writer.(writer); ok {
		h.Writer = writer{
			exec: exec,
			// The stdin runner routes through the SAME fake, dropping the manifest:
			// a test that stubs the process must not have one operation escape to a
			// real kubectl because it happens to pipe its input.
			stdin: func(_ string, args ...string) ([]byte, error) { return exec("kubectl", args...) },
		}
	}
	// denied stays denied: a test cannot widen a binding by stubbing.
	return h
}

// ClassifiedVerbs returns every verb this package knows, for the doc-agreement
// test and for the error above to be checkable.
func ClassifiedVerbs() (read, write []string) {
	for v := range readVerbs {
		read = append(read, v)
	}
	for v := range writeVerbs {
		write = append(write, v)
	}
	sort.Strings(read)
	sort.Strings(write)
	return read, write
}

// execStdin runs kubectl with a manifest on stdin.
//
// It is a direct exec.Command rather than kubectlprobe.Exec because that seam
// takes no stdin — which is precisely why every `apply -f -` in this tree bypassed
// the seams and went unmeasured. Giving the capability layer its own stdin path is
// what lets those calls be declared instead of hidden.
var execStdin = func(in string, args ...string) ([]byte, error) {
	cmd := exec.Command("kubectl", args...)
	cmd.Stdin = strings.NewReader(in)
	return cmd.CombinedOutput()
}

// IsRefusing reports whether a handle is the present-and-refusing variant — the
// one a binding gets when it did not declare the grant.
//
// ─────────────────────────────────────────────────────────────────────────────
// IT MAKES A DESIGN CHOICE OBSERVABLE, WITHOUT UNDOING IT.
//
// This package hands back a non-nil refusing handle rather than nil, deliberately:
// "a nil handle would panic at the call site and be reported as a crash rather
// than as a permission fault, and — worse — would tempt callers into `if w != nil`
// guards that silently skip the work." That is right, and it has a cost nobody had
// paid: a Deps struct assembled from the WRONG binding is indistinguishable from
// one assembled correctly, because both fields are non-nil and both look like
// handles. The difference only shows up when the code path runs, as a permission
// error naming a grant nobody forgot.
//
// That is how two assert-suite lanes shipped unable to mutate. So the ASSEMBLY
// layer gets a way to ask, and the answer is used exactly there — not at the call
// site, which must keep treating a refusal as a refusal.
//
// It is a type switch rather than an interface method, so that adding a refusing
// type without listing it here fails this package's own test rather than silently
// reporting a denied handle as live.
// ─────────────────────────────────────────────────────────────────────────────
func IsRefusing(h any) bool {
	switch h.(type) {
	case deniedCluster, deniedWriter, deniedSecrets, deniedCustodian,
		deniedBaoAdmin, deniedForge, deniedCloud, deniedRepo, deniedRepoWriter, deniedKube:
		return true
	}
	return false
}
