package capability_test

// THE SEAM GLOBALS ARE THE CAPABILITY LAYER'S BACK DOOR, and until this file
// nothing counted them.
//
// rawexec_test.go watches `exec.Command("kubectl", …)` — the hole a grant cannot
// see because no seam is involved. But the ORDINARY path is not that. It is
// `kubectlprobe.Exec("kubectl", …)`: a package-level func var, exported, callable
// by anything, and completely unconstrained by the caller's declared grants. A
// binding declaring `cluster-read` can reach it and run `kubectl delete`.
//
// That makes "the grant IS the handle" true of the front door only. The audit that
// found this put it plainly: a capability system that can be sidestepped by an
// import is the same shape as the problem it fixes.
//
// TWO TREES, TWO DIFFERENT RULES, and the difference is the point:
//
//   - internal/extensions DECLARES grants, so a seam call there is a bypass of
//     something real. It is ratcheted: the count may only fall, and the remedy is
//     to take a capability.Handles instead.
//   - internal/verbs declares NOTHING. There are no grants to bypass, so counting
//     would measure a rule that does not exist. What applies instead is a claim
//     about what a command IS: `llz doctor`, `llz argocd-diagnostics` and
//     `llz phase-timing` READ a platform and print it. A mutating kubectl verb in
//     that tree is a command doing something its name does not say.
//
// The second rule is the net the verbs lost when they moved out of
// internal/extensions and fell off the in-degree ratchet and rawexec's scope both.
// Replacing it was the condition the audit put on doing any further capability
// work.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
)

// seamCall matches a call to one of the exported process seams. These are the
// vars that hand out an unconstrained kubectl; `kube.Apply` is included because it
// applies a manifest, which is a write by any reading.
var seamCall = regexp.MustCompile(
	`\b(?:kubectlprobe\.(?:Exec|Combined)|kube\.(?:Exec|Combined|Apply))\b`)

// allowedSeamCalls is every remaining seam-global call under internal/extensions,
// by package. MEASURED, not chosen. Like allowedEdges and allowedRawKubectl it
// fails in both directions: a call that disappears must be deleted here, so the
// paydown is banked instead of left as room to regrow.
//
// EVERY LINE IS A CANDIDATE FOR A CAPABILITY HANDLE, not an endorsement. The
// conversion is mechanical where the caller already knows its binding — take
// `capability.For(b)` and call `Cluster.Run` — and the ones that resist are the
// interactive and long-lived cases the Writer's one-shot []byte shape cannot
// express, which are the same ones rawexec_test already lists.
var allowedSeamCalls = map[string]int{
	"assertobjstore":  3,
	"assertsecrets":   3,
	"branchpolicy":    3,
	"buildpreflight":  2,
	"chartguard":      1,
	"clusteraccess":   2,
	"healthsla":       2,
	"identityconfig":  3,
	"openbao":         4,
	"reachability":    3,
	"reconciler":      2,
	"seedspecial":     3,
	"statepassphrase": 2,
	"templatecommit":  2,
}

func TestNoNewSeamGlobalCalls(t *testing.T) {
	got := countByPackage(t, filepath.Join("..", "..", "extensions"), seamCall)

	for pkg, n := range got {
		want, ok := allowedSeamCalls[pkg]
		if !ok {
			t.Errorf("%s calls a process seam global directly (%d call(s)) — that hands it an "+
				"unconstrained kubectl regardless of what its binding declared. Take "+
				"capability.For(binding) and use Cluster.Run / the Writer's named operations, "+
				"or add it here with the reason it cannot.", pkg, n)
			continue
		}
		if n > want {
			t.Errorf("%s: %d seam calls, allowed %d — a NEW bypass appeared. The grant line "+
				"stops being true the moment one of these runs a verb the binding did not "+
				"declare.", pkg, n, want)
		}
		if n < want {
			t.Errorf("%s: %d seam calls but %d allowed — LOWER IT to %d in this commit, so the "+
				"paydown is banked instead of left as room to regrow", pkg, n, want, n)
		}
	}

	var gone []string
	for pkg := range allowedSeamCalls {
		if _, still := got[pkg]; !still {
			gone = append(gone, pkg)
		}
	}
	sort.Strings(gone)
	if len(gone) > 0 {
		t.Errorf("these packages no longer call a seam global — delete them from "+
			"allowedSeamCalls: %s", strings.Join(gone, ", "))
	}
}

// kubectlLiteralVerb finds `…("kubectl", "<verb>"` — the shape of every statically
// classifiable invocation, whether it goes through a seam or through os/exec.
var kubectlLiteralVerb = regexp.MustCompile(`"kubectl",\s*"([a-z][a-z-]*)"`)

// kubectlPassthrough finds `…("kubectl", args...)`, where the verb is not a
// literal and cannot be classified by reading the source.
var kubectlPassthrough = regexp.MustCompile(`"kubectl",\s*[a-zA-Z][a-zA-Z0-9]*\.\.\.`)

// allowedPassthrough lists the verb-tree callers whose argv is assembled by their
// caller. Each needs a human to have checked that every call site is a read.
var allowedPassthrough = map[string]string{
	"argodiag/diagnose.go": "kubectlNames() — every caller passes a `get … -o name` listing, " +
		"and the function's contract is to return names or nil",
}

// A COMMAND THAT ONLY LOOKS MUST ONLY LOOK. internal/verbs holds no declaration,
// so nothing else in this repo constrains what its packages may do to a cluster.
func TestVerbsDoNotMutateTheCluster(t *testing.T) {
	root := filepath.Join("..", "..", "verbs")
	_, writes := capability.ClassifiedVerbs()
	write := map[string]bool{}
	for _, v := range writes {
		write[v] = true
	}

	var scanned int
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") ||
			strings.HasSuffix(path, "_test.go") {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scanned++
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)

		for _, m := range kubectlLiteralVerb.FindAllSubmatch(b, -1) {
			if v := string(m[1]); write[v] {
				t.Errorf("%s runs `kubectl %s` — a MUTATING verb from a tree that declares "+
					"nothing. These commands read a platform and print it; a write here is a "+
					"command doing something its name does not say. If it genuinely must "+
					"mutate, it is not a verb — move it to internal/extensions and declare "+
					"the grant.", rel, v)
			}
		}
		if kubectlPassthrough.Match(b) {
			if _, ok := allowedPassthrough[rel]; !ok {
				t.Errorf("%s passes kubectl a variadic argv, so its verb cannot be classified "+
					"by reading the source. Either take literal verbs, or add it to "+
					"allowedPassthrough with the reason every call site is a read.", rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if scanned == 0 {
		t.Fatal("scanned no sources under internal/verbs — the tree moved and this guard " +
			"has been vacuous since it did")
	}

	for rel := range allowedPassthrough {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil || !kubectlPassthrough.Match(b) {
			t.Errorf("allowedPassthrough names %s, which no longer passes a variadic argv — "+
				"delete the line rather than leaving an exemption nothing uses", rel)
		}
	}
}

// countByPackage counts regex matches per PACKAGE directory under root, skipping
// tests. Second-to-last path segment, for the reason rawexec_test records: taking
// the first silently counted per bucket once the tree gained a level.
func countByPackage(t *testing.T, root string, re *regexp.Regexp) map[string]int {
	t.Helper()
	got := map[string]int{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") ||
			strings.HasSuffix(path, "_test.go") {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if n := len(re.FindAll(b, -1)); n > 0 {
			rel, _ := filepath.Rel(root, path)
			seg := strings.Split(filepath.ToSlash(rel), "/")
			got[seg[len(seg)-2]] += n
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(got) == 0 {
		t.Fatal("matched nothing — the pattern has drifted from the code and this guard is " +
			"no longer checking anything")
	}
	return got
}

// A REGEX GUARD THAT HAS NEVER FIRED IS A GUARD NOBODY HAS TESTED, and both rules
// above pass on a clean tree by construction. So prove the detectors discriminate,
// against synthetic sources rather than by breaking the real ones — the same
// reason layering_test points its rule at the registry.
func TestDetectorsActuallyFire(t *testing.T) {
	_, writes := capability.ClassifiedVerbs()
	write := map[string]bool{}
	for _, v := range writes {
		write[v] = true
	}

	for _, tc := range []struct {
		name string
		src  string
		want bool
	}{
		{"seam via kubectlprobe", `x, _ := kubectlprobe.Exec("kubectl", "get", "pods")`, true},
		{"seam via kube.Apply", `err := kube.Apply(manifest)`, true},
		{"seam via kube.Exec", `b, _ := kube.Exec("kubectl", "get", "secret")`, true},
		{"a capability handle is not a seam", `h.Cluster.Run("get", "pods")`, false},
		{"the Writer is not a seam", `h.Writer.Delete(ns, "pod", name)`, false},
		// kubectlprobe.Sleep is not a process seam; the pattern must not be so loose
		// that any reference to the package counts.
		{"unrelated symbol in the same package", `kubectlprobe.Sleep(time.Second)`, false},
	} {
		if got := seamCall.MatchString(tc.src); got != tc.want {
			t.Errorf("seamCall(%s): matched=%v want %v — src %q", tc.name, got, tc.want, tc.src)
		}
	}

	// The verbs rule: a write verb must be caught, a read verb must not.
	for _, tc := range []struct {
		src      string
		wantVerb string
		wantHit  bool
	}{
		{`kubectlprobe.Exec("kubectl", "delete", "ns", n)`, "delete", true},
		{`exec.Command("kubectl", "apply", "-f", p)`, "apply", true},
		{`kubectlprobe.Exec("kubectl", "exec", "-n", ns)`, "exec", true},
		{`kubectlprobe.Exec("kubectl", "get", "pods")`, "get", false},
		{`kubectlprobe.Exec("kubectl", "describe", "nodes")`, "describe", false},
	} {
		m := kubectlLiteralVerb.FindStringSubmatch(tc.src)
		if m == nil {
			t.Errorf("kubectlLiteralVerb did not match %q — the shape it looks for has drifted", tc.src)
			continue
		}
		if m[1] != tc.wantVerb {
			t.Errorf("extracted verb %q from %q, want %q", m[1], tc.src, tc.wantVerb)
		}
		if hit := write[m[1]]; hit != tc.wantHit {
			t.Errorf("%q: classified write=%v want %v — the guard and the capability "+
				"layer disagree about what mutates", tc.src, hit, tc.wantHit)
		}
	}

	if !kubectlPassthrough.MatchString(`kubectlprobe.Exec("kubectl", args...)`) {
		t.Error("kubectlPassthrough no longer matches a variadic argv — unclassifiable calls " +
			"would pass silently, which is the case the allowlist exists for")
	}
	if kubectlPassthrough.MatchString(`kubectlprobe.Exec("kubectl", "get", "pods")`) {
		t.Error("kubectlPassthrough matches a literal argv — it would demand an allowlist " +
			"entry for calls the other rule already classifies")
	}
}
