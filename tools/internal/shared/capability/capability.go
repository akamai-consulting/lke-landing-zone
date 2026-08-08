// Package capability turns a binding's DECLARED GRANTS into the handles it can
// actually use. It is the implementation of the extension model's Decision 3 --
// "the grant IS the handle" -- which until now was an aspiration in a design doc.
//
// WHAT WAS WRONG. Twenty-six Deps structs carried a `func(name string, args
// ...string)` exec seam. A binding that declared `cluster-read` and a binding that
// declared `cluster-write` received the identical seam, and either could run
// `kubectl delete`, `bao`, `linode-cli` or anything else on PATH. Grants were
// checked against a state table at test time and then handed no teeth at all, so
// the ceiling described a permission system that did not exist.
//
// THE SURFACE IS MUCH NARROWER THAN THE SEAM COUNT SUGGESTED, which is why this is
// tractable. Measured across internal/extensions before any of it was designed:
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
	"sort"
	"strings"

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
	// `auth can-i` asks what the caller may do. It is the one verb whose whole
	// purpose is to be safe to call.
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

// configWriteSubverbs are the `kubectl config` subcommands that mutate kubeconfig.
// `config view` and `config current-context` read; these do not.
var configWriteSubverbs = map[string]bool{
	"set": true, "set-context": true, "set-cluster": true, "set-credentials": true,
	"unset": true, "use-context": true, "rename-context": true, "delete-context": true,
	"delete-cluster": true, "delete-user": true,
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
	write bool
	exec  func(string, ...string) ([]byte, error)
	comb  func(string, ...string) string
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
	if v == "config" && !c.write {
		// The mutation is in the second word, so `config` alone is not enough.
		if sub := secondWord(args, v); configWriteSubverbs[sub] {
			return c.deny(v + " " + sub)
		}
	}
	if readVerbs[v] {
		return nil
	}
	if writeVerbs[v] {
		if c.write {
			return nil
		}
		return c.deny(v)
	}
	// Unclassified: refused by both handles, on purpose.
	return fmt.Errorf("capability: kubectl %q is not a classified verb — add it to readVerbs or "+
		"writeVerbs in internal/shared/capability, which is a decision about whether it mutates", v)
}

func (c cluster) deny(v string) error {
	return fmt.Errorf("capability: kubectl %s needs the %q grant, which this binding did not declare — "+
		"either add it to the binding (and satisfy grantStates for its state) or stop mutating here",
		v, extension.ClusterWrite)
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
	// Cluster is the kubectl handle, scoped by cluster-read / cluster-write.
	Cluster Cluster
}

// For builds the handles a binding's declared grants entitle it to.
//
// It reads the BINDING, not the extension: grants are per-binding precisely so
// that an extension holding a read assertion and a write transition cannot use the
// transition's grant from inside the assertion. Passing the whole extension here
// would hand back the union and undo that.
func For(b extension.Binding) Handles {
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
	if !read && !write {
		return Handles{Cluster: deniedCluster{}}
	}
	return Handles{Cluster: cluster{
		write: write,
		exec:  kubectlprobe.Exec,
		comb:  kubectlprobe.Combined,
	}}
}

// WithExec is For with the process seam replaced, for tests that must not shell
// out. It takes the same Binding so a test cannot accidentally grant itself more
// than the declaration allows.
func WithExec(b extension.Binding, exec func(string, ...string) ([]byte, error), comb func(string, ...string) string) Handles {
	h := For(b)
	c, ok := h.Cluster.(cluster)
	if !ok {
		return h // denied stays denied; a test cannot widen a binding by stubbing
	}
	c.exec, c.comb = exec, comb
	return Handles{Cluster: c}
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
