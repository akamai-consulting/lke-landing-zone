package capability_test

// THE IN-CLUSTER REST CLIENT IS A SECOND CLUSTER-MUTATION TRANSPORT, AND THE
// CAPABILITY LAYER CANNOT SEE IT.
//
// ────────────────────────────────────────────────────────────────────────────
// `capability.Writer` fences the KUBECTL-ARGV path: six named mutations, a
// refusing handle without `cluster-write`, and TestNoNewRawKubectlExec ratcheting
// what escapes it. That ratchet exists because raw `exec.Command("kubectl", …)`
// bypassed the seams — the argument was that a mutation nobody routed through a
// handle is a mutation the declaration does not constrain.
//
// internal/shared/kube.Client bypasses them the same way and nothing said so. It
// speaks the Kubernetes API directly — CreateJSON and MergePatch are its two
// mutating methods — and internal/shared/capability does not import kube at all.
// So a lane can hold a refusing Writer and still create and patch objects.
//
// WHICH PACKAGES USE IT IS THE POINT. Both are the in-cluster reconcile lanes:
// code that runs CONTINUOUSLY, unattended, under the cluster's own ServiceAccount,
// with no human present when it acts. That is the least supervised code in this
// tree, and it was the one path the capability model did not cover.
// `reconcile-actions` declares cluster-write, secret-custody AND cloud-mutate, and
// mutates through client.MergePatch — so all three grants are declaration-only.
//
// THIS IS A RATCHET, NOT A FIX, and the distinction is deliberate. Routing these
// through Writer needs a transport seam Writer does not have: it builds an argv
// and shells out, while kube.Client holds a REST client with an in-cluster token.
// Inventing that seam off nine call sites, in the code that must keep working
// unattended, is a larger change than it looks. What this does is stop the
// unfenced surface GROWING while the design question is open — the same thing
// allowedRawKubectl and allowedRawCloud did for their transports, and the same
// reason: an unmeasured surface grows quietly and a measured one has to be argued
// for.
//
// It fails in BOTH directions like its siblings, so a conversion must delete its
// line here and the paydown is banked rather than left as room to regrow.
// ────────────────────────────────────────────────────────────────────────────

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// allowedRawKubeMutations is every package under internal/extensions still
// mutating the cluster through kube.Client rather than through a capability
// handle, with the count.
//
// Both entries are the reconcile daemon's own lanes. They are not an oversight:
// the daemon runs in-pod and reaches the API with its ServiceAccount token, which
// is precisely the shape Writer's argv transport cannot express today.
var allowedRawKubeMutations = map[string]int{
	// reconcilelanes IS GONE FROM THIS LIST, which is the paydown this ratchet
	// exists to bank. All three of its lanes take capability.KubeAPI now, and the
	// four-method `Client` and `argoClient` interfaces they used to accept were
	// dead once the last caller narrowed — deleted rather than left as a shape the
	// next lane could be written against.
	// The daemon's own plumbing: reconcileClient is what runReconcile and
	// buildReconcilers pass around before any lane narrows it, and leaseClient is
	// leader election, which creates and patches the Lease that decides which
	// replica drives. Both are reconciler-runtime's own declaration rather than a
	// lane's, so narrowing them is a question about the daemon's shape.
	"reconciler": 3,
}

// rawKubeMutation matches a function that ACCEPTS an unfenced mutating client.
//
// ────────────────────────────────────────────────────────────────────────────
// IT COUNTS TYPES, NOT CALL SITES, AND THE FIRST VERSION GOT THAT WRONG.
//
// The original regex matched `.CreateJSON(` / `.MergePatch(` — every mutating
// call. That measures the wrong thing: once a lane takes capability.KubeAPI its
// calls are FENCED, and they still look identical to a regex. Routing three lanes
// through the handle changed the count by zero, which is how the mistake surfaced.
//
// What actually establishes the fence is the TYPE a function accepts. A parameter
// or field typed as a raw four-method client can mutate whatever it likes; one
// typed capability.KubeAPI cannot exceed its binding. So the subject is
// declarations of the raw shape, and converting a lane now moves the number.
//
// The pattern deliberately does NOT match `capability.KubeAPI`, which is the whole
// point of the distinction.
// ────────────────────────────────────────────────────────────────────────────
var rawKubeMutation = regexp.MustCompile(
	`(?:client|c) (?:\*kube\.Client|Client|reconcileClient|argoClient|leaseClient)\b`)

func TestNoNewRawKubeMutations(t *testing.T) {
	root := filepath.FromSlash("../../extensions")
	got := map[string]int{}

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// Comments do not count, for the reason the sibling ratchets give: several
		// headers narrate this conversion, and a ratchet that failed on a
		// documentation change is one people delete.
		n := 0
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			n += len(rawKubeMutation.FindAllString(line, -1))
		}
		if n > 0 {
			rel, _ := filepath.Rel(root, path)
			seg := strings.Split(filepath.ToSlash(rel), "/")
			got[seg[len(seg)-2]] += n
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	// A scan that found nothing agrees with any list. The allowlist is non-empty,
	// so zero findings means the walk broke rather than the tree got clean — and
	// when this list finally reaches zero, that check stops protecting anything
	// (see rawcloud_test.go for the class), so the control below outlives it.
	if len(got) == 0 {
		t.Fatal("no unfenced kube clients found anywhere under internal/extensions — the walk " +
			"is looking at the wrong tree or the type names changed, and a ratchet that " +
			"cannot find its subject passes for the wrong reason")
	}

	for pkg, n := range got {
		want, ok := allowedRawKubeMutations[pkg]
		if !ok {
			t.Errorf("%s mutates the cluster through kube.Client (%d site(s)) — capability.Writer "+
				"fences the kubectl path and this one goes around it, so the package's declared "+
				"cluster-write constrains nothing. Route it through a capability handle, or add it "+
				"here with the reason it cannot be.", pkg, n)
			continue
		}
		if n > want {
			t.Errorf("%s: %d raw kube mutations, allowed %d — a NEW one appeared. This is the "+
				"transport the declaration cannot see; do not grow it.", pkg, n, want)
		}
		if n < want {
			t.Errorf("%s: %d raw kube mutations but %d allowed — LOWER IT to %d in this commit, "+
				"so the paydown is banked instead of left as room to regrow", pkg, n, want, n)
		}
	}

	var gone []string
	for pkg := range allowedRawKubeMutations {
		if _, still := got[pkg]; !still {
			gone = append(gone, pkg)
		}
	}
	sort.Strings(gone)
	if len(gone) > 0 {
		t.Errorf("these packages no longer mutate through kube.Client — delete them from "+
			"allowedRawKubeMutations: %s", strings.Join(gone, ", "))
	}
}

// AND THE FENCE MUST NOT QUIETLY ACQUIRE A THIRD WAY IN. kube.Client's mutating
// surface is two methods today; a third would be unratcheted from the moment it
// was added, because rawKubeMutation names them explicitly.
//
// Asserting on the SOURCE rather than on a list of names, so adding a method to
// kube.Client fails here instead of silently widening what this file cannot see.
func TestKubeClientMutatingSurfaceIsStillTwoMethods(t *testing.T) {
	b, err := os.ReadFile(filepath.FromSlash("../kube/kube.go"))
	if err != nil {
		t.Fatalf("reading kube.go: %v — this ratchet is scoped to its mutating methods and "+
			"cannot check that scope if the file moved", err)
	}
	// Exported methods on *Client whose name is not a known read.
	methodRe := regexp.MustCompile(`func \(c \*Client\) ([A-Z][A-Za-z]*)\(`)
	reads := map[string]bool{"GetJSON": true, "Watch": true}
	mutators := map[string]bool{"CreateJSON": true, "MergePatch": true}

	var unclassified []string
	for _, m := range methodRe.FindAllStringSubmatch(string(b), -1) {
		name := m[1]
		if reads[name] || mutators[name] {
			continue
		}
		unclassified = append(unclassified, name)
	}
	sort.Strings(unclassified)
	if len(unclassified) > 0 {
		t.Errorf("kube.Client grew exported method(s) this ratchet does not classify: %s\n"+
			"\tIf it mutates, add it to rawKubeMutation — otherwise it is a cluster write no "+
			"declaration constrains and nothing counts. If it reads, add it to `reads` here.",
			strings.Join(unclassified, ", "))
	}
}
