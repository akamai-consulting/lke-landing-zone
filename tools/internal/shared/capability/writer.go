package capability

// Writer is what `cluster-write` actually entitles a binding to: six named
// operations, not "any kubectl verb that mutates".
//
// THE SIX ARE MEASURED, NOT INVENTED. Every mutating call site in
// internal/extensions was listed before this file was written — seventeen of them
// across eight packages — and they collapse into six shapes:
//
//	annotate <kind> <name> <k=v> --overwrite        5   argo refresh ×4, kyverno stamp ×1
//	delete <kind> <target> --ignore-not-found       4   ephemeral job/workflow/namespace
//	patch <kind> <name> --type merge -p <json>      3   argo sync, gameday wedge ×2
//	rollout restart <target>                        2   argocd-redis, kyverno retrofit
//	create token <sa> --duration=<d>                1   login smoke
//	apply --server-side -f <manifest>               1   kyverno policy
//
// WHY THIS IS TIGHTER THAN A VERB CHECK. `cluster-write` used to mean any
// mutating kubectl subcommand: `drain`, `taint`, `exec ... -- sh -c`, `delete
// namespace` on anything. It now means these six shapes with these arguments. Four
// of the eight writers are `assert-*` extensions whose entire mutation is
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
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

// Writer is the mutating half of a cluster grant. A binding that did not declare
// cluster-write receives one whose every method refuses.
type Writer interface {
	// Annotate sets one key=value on a resource, with --overwrite.
	Annotate(ns, kind, name, keyValue string) ([]byte, error)
	// Delete removes a resource, with --ignore-not-found. target may be a name or
	// a `-l selector` pair supplied as two arguments.
	Delete(ns, kind string, target ...string) ([]byte, error)
	// PatchMerge applies a merge patch.
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

func (w writer) PermitsWrite() error { return nil }

func (w writer) Annotate(namespace, kind, name, keyValue string) ([]byte, error) {
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
	a := append(ns(namespace), "delete", kind)
	a = append(a, target...)
	return w.exec("kubectl", append(a, "--ignore-not-found")...)
}

func (w writer) PatchMerge(namespace, kind, name, patchJSON string) ([]byte, error) {
	a := append(ns(namespace), "patch", kind, name, "--type", "merge", "-p", patchJSON)
	return w.exec("kubectl", a...)
}

func (w writer) RolloutRestart(namespace, target string) ([]byte, error) {
	a := append(ns(namespace), "rollout", "restart", target)
	return w.exec("kubectl", a...)
}

func (w writer) CreateToken(namespace, serviceAccount, duration string) ([]byte, error) {
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
