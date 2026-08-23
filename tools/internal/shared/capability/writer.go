package capability

// Writer is what `cluster-write` actually entitles a binding to: eight named
// operations, not "any kubectl verb that mutates".
//
// THE SHAPES ARE MEASURED, NOT INVENTED. Every mutating call site in
// internal/extensions was listed before this file was written — seventeen of them
// across eight packages — and they collapsed into six shapes:
//
//	annotate <kind> <name> <k=v> --overwrite        5   argo refresh ×4, kyverno stamp ×1
//	delete <kind> <target> --ignore-not-found       4   ephemeral job/workflow/namespace
//	patch <kind> <name> --type merge -p <json>      3   argo sync, gameday wedge ×2
//	rollout restart <target>                        2   argocd-redis, kyverno retrofit
//	create token <sa> --duration=<d>                1   login smoke
//	apply --server-side -f <manifest>               1   kyverno policy
//
// TWO MORE ARRIVED AFTER IT, AND THE CENSUS IS WHY THEY WERE MISSING. It counted
// Deps SEAMS, and both of these reach exec.Command directly to pipe a manifest to
// stdin, so neither appeared in a seam-based count at all:
//
//	apply -f - (manifest on stdin)                  2   assert-network's probe ns, broad-pat drill
//	create -f - (manifest on stdin)                 1   assert-platform's health Workflow
//
// KEEP THE COUNT AND THE INTERFACE IN STEP. This header said "six" for both
// additions, and so did three sentences in capability.go and the refusal message
// Permits() hands a caller — which told a developer needing ApplyStdin to use one
// of six operations that did not include it. A count restated in five places and
// checked in none is the shape TestWriterOperationCountMatchesTheProse now closes.
//
// WHY THIS IS TIGHTER THAN A VERB CHECK. `cluster-write` used to mean any
// mutating kubectl subcommand: `drain`, `taint`, `exec ... -- sh -c`, `delete
// namespace` on anything. It now means these eight shapes with these arguments.
// Four of the eight writers are `assert-*` extensions whose entire mutation is
// "refresh an Argo app" or "delete the fixture I just created", and after this
// they are structurally incapable of anything else.
//
// ApplyServerSide IS THE ESCAPE HATCH AND IS NAMED SO IT LOOKS LIKE ONE. Applying
// an arbitrary manifest is, in permission terms, close to unrestricted — a
// manifest can create a ClusterRoleBinding. It is not weaker than what shipped
// before, but it is now the one operation a reviewer can grep for, rather than an
// argv indistinguishable from a `get`. One caller has it (kyverno's policy
// install), and a second should be argued rather than assumed.
//
// EVERY OPERATION APPLIES ITS OWN SAFETY FLAG rather than trusting the caller to.
// Delete always passes --ignore-not-found, because every measured caller wanted it
// and the one that forgets turns "the fixture was already gone" into a failed
// assertion. Annotate always passes --overwrite, because every measured caller
// passed it and an un-overwritable annotate fails on the second run.

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

// WriterOperations names the mutations this handle offers, DERIVED FROM THE
// INTERFACE rather than written down again.
//
// ─────────────────────────────────────────────────────────────────────────────
// A RESTATED COUNT DRIFTED BY TWO AND NOTHING LOOKED. `ApplyStdin` and
// `CreateStdin` landed after the census above, and five sentences went on saying
// "six" — including the refusal Permits() hands a caller, which named six of the
// eight and so told a developer needing ApplyStdin to use an operation that was
// not on the list. The advice was wrong in the direction that sends someone back
// to the raw seam the whole layer exists to close.
//
// So the message is built from reflect.Type.Method, which cobra-free Go gives us
// for nothing: adding an operation adds it to the error, and there is no second
// copy to forget. PermitsWrite is excluded because it is the interrogation, not a
// mutation — a caller told to "call PermitsWrite instead of assembling an argv"
// would be told to ask a question rather than do the work.
// ─────────────────────────────────────────────────────────────────────────────
func WriterOperations() []string {
	t := reflect.TypeOf((*Writer)(nil)).Elem()
	out := make([]string, 0, t.NumMethod())
	for i := 0; i < t.NumMethod(); i++ {
		if name := t.Method(i).Name; name != "PermitsWrite" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Writer is the mutating half of a cluster grant. A binding that did not declare
// cluster-write receives one whose every method refuses.
type Writer interface {
	// Annotate sets one key=value on a resource, with --overwrite.
	Annotate(ns, kind, name, keyValue string) ([]byte, error)
	// Delete removes a resource, with --ignore-not-found. target may be a name or
	// a `-l selector` pair supplied as two arguments.
	Delete(ns, kind string, target ...string) ([]byte, error)
	// PatchMerge applies a JSON MERGE patch (`--type merge`), not a strategic one.
	//
	// THE DISTINCTION IS NOT COSMETIC, and the example this comment used to cite
	// has since turned around, which makes the point better than the original did.
	// identity-plane patched a StatefulSet's `hostAliases` with `--type=strategic`
	// — where the API server merges LIST ENTRIES by key — and that was recorded
	// here as a reason the call could not be squeezed through this operation,
	// because a JSON merge patch REPLACES the whole list.
	//
	// It turned out the strategic semantics were the BUG. HostAliases carries
	// `patchStrategy:"merge" patchMergeKey:"ip"`, so a changed gateway ClusterIP
	// was a new key: the entry was APPENDED and the stale one kept, the pod got two
	// /etc/hosts lines for one hostname and resolved the dead one, and the branch
	// that patch exists for could never converge. That call is `--type=merge` now
	// and would route through here cleanly.
	//
	// The rule the example was illustrating still stands: a caller that genuinely
	// needs strategic merge is not a caller to convert mechanically, and would need
	// a PatchStrategic with a test pinning the list semantics. What has changed is
	// that there is currently no such caller — so the next person to look does not
	// have to work out whether the one cited case is still real.
	PatchMerge(ns, kind, name, patchJSON string) ([]byte, error)
	// RolloutRestart rolls a workload (target is e.g. "deploy/argocd-redis").
	RolloutRestart(ns, target string) ([]byte, error)
	// CreateToken mints a short-lived ServiceAccount token.
	CreateToken(ns, serviceAccount, duration string) ([]byte, error)
	// ApplyServerSide applies a manifest FROM A PATH with server-side apply. See
	// the header: this is the escape hatch, deliberately named.
	ApplyServerSide(manifestPath, fieldManager string) ([]byte, error)
	// ApplyStdin applies a manifest supplied as text (`kubectl apply -f -`).
	//
	// IT IS THE SAME ESCAPE HATCH BY ANOTHER ROUTE and is listed separately because
	// that is how it was FOUND: the first census counted Deps seams and missed this
	// entirely, because every caller reached for exec.Command directly and piped a
	// manifest to stdin. assert-network creating its probe namespace never appeared
	// in a seam-based count for exactly that reason.
	ApplyStdin(manifest, fieldManager string) ([]byte, error)
	// CreateStdin creates from a manifest supplied as text, returning the created
	// object. Distinct from ApplyStdin because `create` FAILS on an existing object
	// where `apply` reconciles it, and the one caller (a health Workflow submission)
	// depends on that failure to detect a duplicate submission.
	CreateStdin(ns, manifest string) ([]byte, error)
	// PermitsWrite reports whether this handle may mutate at all, so a caller can
	// fail early with its own message.
	PermitsWrite() error
}

type writer struct {
	exec  func(string, ...string) ([]byte, error)
	stdin func(in string, args ...string) ([]byte, error)
}

func ns(namespace string) []string {
	if namespace == "" {
		return nil
	}
	return []string{"-n", namespace}
}

// ─────────────────────────────────────────────────────────────────────────────
// A NAMED OPERATION MUST NOT BE AN ARGV IN DISGUISE.
//
// The whole argument for these six is that "a reviewer sees Annotate/Delete/
// PatchMerge in the diff rather than an argv they have to parse". Every parameter
// below lands in that argv as its own element, so a parameter that starts with `-`
// is a FLAG, and the review property is gone. Probed:
//
//	Delete("kube-system", "pod", "--all")  -> kubectl -n kube-system delete pod --all
//	Delete("", "namespace", "--all")       -> kubectl delete namespace --all
//
// The second removes every namespace in the cluster, through the handle whose
// purpose is to make what a lane may do legible. Delete already guards the bare
// `delete <kind>` case, with a comment saying why — "a bare `delete pod` in a
// namespace would remove every one of them" — and `--all` reaches that exact
// outcome past the guard.
//
// NOTE WHAT IS **NOT** A HOLE, because it shaped the fix. These run through
// exec.Command with an argv SLICE, so a parameter containing spaces is one
// argument: `RolloutRestart("ns", "deploy/x --namespace kube-system")` is a single
// absurd resource name that kubectl rejects, not two tokens. Only parameters that
// occupy their own element can smuggle a flag, which is why the rule is about a
// leading `-` and not about quoting.
//
// A KUBERNETES NAME CANNOT BEGIN WITH `-` — DNS label rules — so refusing one costs
// nothing and no shipping caller passes one.
// ─────────────────────────────────────────────────────────────────────────────

// safeArg rejects a positional that would be read as a flag.
func safeArg(what, v string) error {
	if strings.HasPrefix(v, "-") {
		return fmt.Errorf("capability: %s %q begins with '-', so kubectl would read it as a FLAG "+
			"rather than as a name. A named operation exists so a reviewer can see what it does "+
			"without parsing an argv; a flag in a parameter takes that back (`--all` is the case "+
			"that made this a rule); Kubernetes names cannot begin with '-'", what, v)
	}
	return nil
}

// deleteFlags are the flags a caller may pass in Delete's variadic target, valued
// forms first. EARNED, not enumerated: `--wait=` is the one shipping caller
// (assert-network's namespace teardown) and the selector forms are what Delete's
// own error message already promises ("needs a name or selector"). Everything else
// beginning with `-` is refused, `--all` and `-A` most of all.
var (
	deleteValuedFlags  = map[string]bool{"-l": true, "--selector": true}
	deleteFlagPrefixes = []string{"--selector=", "--wait=", "--timeout="}
)

// checkSelector refuses the selector values that are not narrowing.
//
// AN EMPTY SELECTOR MATCHES EVERY OBJECT, which is `--all` spelled differently and
// was reachable straight through the allowlist that had just been added to stop
// `--all`. `kubectl delete namespace -l ""` removes every namespace in the
// cluster. Permitting a FLAG means permitting its VALUE, and the value is where
// the narrowing was supposed to live.
//
// A value beginning with `-` is refused too: it is a flag that arrived in the
// value position, either because the caller forgot the selector or because the
// flag is trying to reach the argv sideways.
func checkSelector(kind, sel string) error {
	if strings.TrimSpace(sel) == "" {
		return fmt.Errorf("capability: Delete got an EMPTY selector for %s, which matches every "+
			"one of them — that is `--all` in another spelling, and it is the outcome both the "+
			"empty-target guard and the flag allowlist exist to refuse", kind)
	}
	if strings.HasPrefix(sel, "-") {
		return fmt.Errorf("capability: Delete got %q where a selector expression belongs — a "+
			"flag in the value position is not a narrowing", sel)
	}
	return nil
}

// checkDeleteTargets walks the variadic, allowing the earned flags and their
// values and requiring everything else to be a plain name.
func checkDeleteTargets(kind string, target []string) error {
	for i := 0; i < len(target); i++ {
		t := target[i]
		if !strings.HasPrefix(t, "-") {
			continue
		}
		if deleteValuedFlags[t] {
			if i+1 >= len(target) {
				return fmt.Errorf("capability: Delete got %q with no value", t)
			}
			if err := checkSelector(kind, target[i+1]); err != nil {
				return err
			}
			i++
			continue
		}
		if hasAnyPrefix(t, deleteFlagPrefixes) {
			if sel, ok := strings.CutPrefix(t, "--selector="); ok {
				if err := checkSelector(kind, sel); err != nil {
					return err
				}
			}
			continue
		}
		return fmt.Errorf("capability: Delete refuses the flag %q. It permits a name, a selector "+
			"(-l/--selector) and --wait=/--timeout=; anything else in this position is a flag "+
			"nobody reviewing the call site would see. `delete %s --all` removes every one of "+
			"them, which is the outcome the empty-target guard above already refuses", t, kind)
	}
	return nil
}

func (w writer) PermitsWrite() error { return nil }

func (w writer) Annotate(namespace, kind, name, keyValue string) ([]byte, error) {
	if err := firstErr(safeArg("namespace", namespace), safeArg("kind", kind), safeArg("name", name)); err != nil {
		return nil, err
	}
	if !strings.Contains(keyValue, "=") {
		return nil, fmt.Errorf("capability: Annotate needs key=value, got %q", keyValue)
	}
	a := append(ns(namespace), "annotate", kind, name, keyValue, "--overwrite")
	return w.exec("kubectl", a...)
}

func (w writer) Delete(namespace, kind string, target ...string) ([]byte, error) {
	if len(target) == 0 {
		return nil, fmt.Errorf("capability: Delete needs a name or selector — a bare `delete %s` "+
			"in a namespace would remove every one of them", kind)
	}
	if err := firstErr(safeArg("namespace", namespace), safeArg("kind", kind),
		checkDeleteTargets(kind, target)); err != nil {
		return nil, err
	}
	a := append(ns(namespace), "delete", kind)
	a = append(a, target...)
	return w.exec("kubectl", append(a, "--ignore-not-found")...)
}

func (w writer) PatchMerge(namespace, kind, name, patchJSON string) ([]byte, error) {
	if err := firstErr(safeArg("namespace", namespace), safeArg("kind", kind), safeArg("name", name)); err != nil {
		return nil, err
	}
	a := append(ns(namespace), "patch", kind, name, "--type", "merge", "-p", patchJSON)
	return w.exec("kubectl", a...)
}

func (w writer) RolloutRestart(namespace, target string) ([]byte, error) {
	if err := firstErr(safeArg("namespace", namespace), safeArg("target", target)); err != nil {
		return nil, err
	}
	a := append(ns(namespace), "rollout", "restart", target)
	return w.exec("kubectl", a...)
}

func (w writer) CreateToken(namespace, serviceAccount, duration string) ([]byte, error) {
	if err := firstErr(safeArg("namespace", namespace), safeArg("serviceaccount", serviceAccount)); err != nil {
		return nil, err
	}
	a := append([]string{"create", "token", serviceAccount}, ns(namespace)...)
	if duration != "" {
		a = append(a, "--duration="+duration)
	}
	return w.exec("kubectl", a...)
}

func (w writer) ApplyServerSide(manifestPath, fieldManager string) ([]byte, error) {
	a := []string{"apply", "--server-side", "--force-conflicts"}
	if fieldManager != "" {
		a = append(a, "--field-manager="+fieldManager)
	}
	return w.exec("kubectl", append(a, "-f", manifestPath)...)
}

func (w writer) ApplyStdin(manifest, fieldManager string) ([]byte, error) {
	a := []string{"apply", "--server-side", "--force-conflicts"}
	if fieldManager != "" {
		a = append(a, "--field-manager="+fieldManager)
	}
	return w.stdin(manifest, append(a, "-f", "-")...)
}

func (w writer) CreateStdin(namespace, manifest string) ([]byte, error) {
	if err := safeArg("namespace", namespace); err != nil {
		return nil, err
	}
	a := append(ns(namespace), "create", "-f", "-", "-o", "json")
	return w.stdin(manifest, a...)
}

// deniedWriter is what a binding without cluster-write receives. Non-nil and
// refusing, for the same reason deniedCluster is.
type deniedWriter struct{}

func (deniedWriter) PermitsWrite() error {
	return fmt.Errorf("capability: this binding did not declare %q, so it cannot mutate the cluster",
		extension.ClusterWrite)
}
func (d deniedWriter) Annotate(_, _, _, _ string) ([]byte, error) { return nil, d.PermitsWrite() }
func (d deniedWriter) Delete(_, _ string, _ ...string) ([]byte, error) {
	return nil, d.PermitsWrite()
}
func (d deniedWriter) PatchMerge(_, _, _, _ string) ([]byte, error) { return nil, d.PermitsWrite() }
func (d deniedWriter) RolloutRestart(_, _ string) ([]byte, error)   { return nil, d.PermitsWrite() }
func (d deniedWriter) CreateToken(_, _, _ string) ([]byte, error)   { return nil, d.PermitsWrite() }
func (d deniedWriter) ApplyServerSide(_, _ string) ([]byte, error)  { return nil, d.PermitsWrite() }
func (d deniedWriter) ApplyStdin(_, _ string) ([]byte, error)       { return nil, d.PermitsWrite() }
func (d deniedWriter) CreateStdin(_, _ string) ([]byte, error)      { return nil, d.PermitsWrite() }

// Denied is the refusing Writer. Exported so a caller holding a struct whose
// Writer field was never populated — a test building a Deps literal, say — can
// degrade to a refusal instead of a nil-pointer panic. "Ungranted arrives non-nil
// and refusing" is only true if it is true for zero values too, and a struct
// literal bypasses every constructor that would otherwise guarantee it.
func Denied() Writer { return deniedWriter{} }

// firstErr returns the first non-nil error, so a guard can check several
// parameters on one line and still name the one that failed.
func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}
