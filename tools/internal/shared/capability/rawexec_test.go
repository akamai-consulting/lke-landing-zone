package capability_test

// RAW `exec.Command("kubectl", ...)` IS THE HOLE THE CAPABILITY LAYER CANNOT SEE,
// and it is the reason the first census undercounted. That census listed the Deps
// seams — 26 of them — and concluded the write surface was 17 call sites. It
// missed every mutation that reached for os/exec directly, including
// assert-network CREATING A NAMESPACE, because no seam was involved and nothing
// was looking anywhere else.
//
// So this guard watches the thing a grant cannot: it counts the raw calls and
// fails when a new one appears. The allowlist below is measured, not chosen, and
// like the in-degree ratchet it fails in BOTH directions — a call that disappears
// must be deleted from the list, so the paydown is banked rather than left as room
// to regrow.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// allowedRawKubectl is every remaining `exec.Command("kubectl", ...)` under
// internal/extensions, by package, with why it is still raw.
var allowedRawKubectl = map[string]int{
	// SEAM IMPLEMENTATIONS. Someone has to actually exec; these are package-level
	// vars a test replaces, and their callers' verbs were measured. They are the
	// next conversion candidates, not accidents.
	"assertnetwork":    2, // dryRunManifest (a dry run is a read) + the admission seam
	"bootstrapcluster": 2, // the bridge-apply kubectl runner
	"clusteraccess":    1, // runnerACLKubectlFn (apply/patch, stdin)
	"converge":         1, // kubectlWaitStream (`wait`, streams to the operator)
	"firewall":         1, // KubectlFn (apply/patch/rollout, stdin)

	// INTERACTIVE, and the Writer's one-shot []byte shape cannot express them.
	// stdin/stdout/stderr are wired to the operator's terminal.
	"openbao": 1, // `kubectl exec` into the OpenBao pod — now declares cluster-write

	// LONG-LIVED TUNNELS. port-forward returns a process the caller manages; it is
	// not a one-shot operation and does not fit Writer either.
	"assertobs":      2,
	"identityconfig": 1,

	// NOT A CLUSTER CALL AT ALL. `kubectl kustomize` renders local files.
	"configreadiness": 1,
}

func TestNoNewRawKubectlExec(t *testing.T) {
	root := filepath.FromSlash("../../extensions")
	got := map[string]int{}
	re := regexp.MustCompile(`exec\.Command\("kubectl"`)

	// THE PATTERN MUST STILL MATCH A CONTROL. See rawcloud_test.go for the full
	// argument: a pattern-scanning ratchet is guarded against a stale regex only by
	// its own outstanding debt, and this list is down to nine. When it reaches zero
	// — which is the goal — a renamed construct would make this scan find nothing
	// and report a clean tree for the wrong reason, exactly as rawcloud's did.
	//
	// capability.go's own execStdin shells out to kubectl and must, because it is
	// the stdin path Writer needs and the seam cannot provide. If the pattern stops
	// matching there, the pattern moved.
	if control, err := os.ReadFile(filepath.FromSlash("capability.go")); err != nil {
		t.Fatalf("reading the control file: %v", err)
	} else if !re.Match(control) {
		t.Fatal("the raw-kubectl pattern matches nothing in capability.go, where execStdin " +
			"shells out to kubectl — the construct was renamed, so this scan now finds nothing " +
			"anywhere and would pass over a tree full of raw exec")
	}

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if n := len(re.FindAll(b, -1)); n > 0 {
			// .../extensions/<bucket>/<pkg>/file.go — the PACKAGE is the unit this
			// guard counts, so take the second-to-last segment rather than the
			// first. It used to take the first, which was the package while the
			// tree was flat; sub-dividing into capabilities/ lifecycle/ assertions/
			// guards/ silently made it count per BUCKET, collapsing nine packages
			// into two rows and reporting every allowlist entry as gone. A path
			// index is only as stable as the layout it assumes.
			rel, _ := filepath.Rel(root, path)
			seg := strings.Split(filepath.ToSlash(rel), "/")
			got[seg[len(seg)-2]] += n
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	for pkg, n := range got {
		want, ok := allowedRawKubectl[pkg]
		if !ok {
			t.Errorf("%s now shells out to kubectl directly (%d call(s)) — that bypasses the "+
				"capability layer entirely, so whatever it does is invisible to its declared "+
				"grants. Route it through the Writer's named operations, or add it here with "+
				"the reason it cannot be.", pkg, n)
			continue
		}
		if n > want {
			t.Errorf("%s: %d raw kubectl calls, allowed %d — a NEW one appeared. The last time "+
				"this count grew unnoticed, an extension declaring cluster-read was creating "+
				"and deleting a namespace.", pkg, n, want)
		}
		if n < want {
			t.Errorf("%s: %d raw kubectl calls but %d allowed — LOWER IT to %d in this commit, "+
				"so the paydown is banked instead of left as room to regrow", pkg, n, want, n)
		}
	}
	var gone []string
	for pkg := range allowedRawKubectl {
		if _, still := got[pkg]; !still {
			gone = append(gone, pkg)
		}
	}
	sort.Strings(gone)
	if len(gone) > 0 {
		t.Errorf("these packages no longer shell out to kubectl — delete them from "+
			"allowedRawKubectl: %s", strings.Join(gone, ", "))
	}
}
