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
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
)

// seamCall matches a call to one of the exported process seams.
//
// `kubectlprobe.Exec` IS NOT A KUBECTL CLIENT, despite the package name, and this
// took a round to notice. Its signature is `(name string, args ...string)` — the
// BINARY is the first argument — so it is `exec.Command` with a swappable seam,
// and four of the packages counted below use it to run `gh`, not kubectl:
// branch-policy, build-preflight, credential-state-passphrase and template-commit.
// reachability runs `ssh-keyscan` through it.
//
// The count is still the right count: an unconstrained process runner is a bypass
// whatever it launches. But the REMEDY differs, and a guard that tells a package
// running `gh api` to "take a Cluster handle" is a guard whose advice does not
// apply — which is how an allowlist entry gets added to make a test quiet. So the
// message names both, and the forge half is honest that no handle exists for it
// yet.
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
	"assertsecrets":   2,
	"clusteraccess":   1,
	"healthsla":       1,
	"identityconfig":  1,
	"openbao":         4,
	"reachability":    1,
	"reconciler":      1,
	"seedspecial":     1,
	"statepassphrase": 1,
}

// baoSeamCall matches a direct call to one of OpenBao's process seams. KVPut and
// KVGetFieldOK are included even though they are STRUCTURED helpers, because they
// are exactly what capability.Custodian and capability.Secrets wrap: calling them
// directly is now the bypass, in the same way calling kubectlprobe.Exec is.
var baoSeamCall = regexp.MustCompile(
	`\bbaoread\.(?:Exec|ExecStdin|ExecPod|ExecFn|KVPut|KVGetFieldOK)\b`)

// allowedBaoSeamCalls is the measured bao surface under internal/extensions.
// MEASURED, not chosen, and it ratchets in both directions like its siblings.
//
// THE SHAPE OF THE REMAINING WORK IS IN THESE NUMBERS. openbao's 22 are the
// lifecycle itself — `operator init`, `operator generate-root`, the unseal waits —
// which are quorum flows and interactive prompts that no one-shot handle expresses,
// the same reason `kubectl exec` stayed raw. The other 25 are day-to-day KV
// traffic across six packages and are the conversion candidates: each holds a
// binding already, so the change is to take capability.For(b).Secrets / .Custodian
// instead of reaching for the package var.
var allowedBaoSeamCalls = map[string]int{
	"credrotate":   6,
	"healthsla":    1,
	"openbao":      21,
	"reachability": 1,
}

func TestNoNewBaoSeamCalls(t *testing.T) {
	got := countByPackage(t, filepath.Join("..", "..", "extensions"), baoSeamCall)
	ratchet(t, "bao seam", got, allowedBaoSeamCalls,
		"that reaches OpenBao with a root token regardless of what its binding declared. "+
			"Take capability.For(binding).Secrets / .Custodian, whose Get returns a VERDICT "+
			"so a refusal can never be mistaken for an absent path")
}

func TestNoNewSeamGlobalCalls(t *testing.T) {
	got := countByPackage(t, filepath.Join("..", "..", "extensions"), seamCall)
	ratchet(t, "seam", got, allowedSeamCalls,
		"that hands it an unconstrained PROCESS RUNNER regardless of what its binding "+
			"declared — the seam takes the binary as its first argument, so this is not "+
			"only about kubectl. If it runs kubectl, take capability.For(binding) and use "+
			"Cluster.Run / the Writer's named operations. If it runs gh or git, there is no "+
			"handle for the forge yet: say so here, and the entry becomes the measurement of "+
			"what that capability would have to cover")
}

// ratchet is the shared body of the three bypass counters: fail on a new package,
// on a count that grew, on a count that SHRANK without being banked, and on an
// allowance for a package that no longer matches. Factored out when the second
// counter arrived rather than copied, because two copies of a ratchet is two
// chances to fix one and not the other.
func ratchet(t *testing.T, what string, got, allowed map[string]int, remedy string) {
	t.Helper()
	for pkg, n := range got {
		want, ok := allowed[pkg]
		if !ok {
			t.Errorf("%s makes %d direct %s call(s) — %s, or add it to the allowlist with "+
				"the reason it cannot.", pkg, n, what, remedy)
			continue
		}
		if n > want {
			t.Errorf("%s: %d %s calls, allowed %d — a NEW bypass appeared. The grant line "+
				"stops being true the moment one of these does something the binding did "+
				"not declare.", pkg, n, what, want)
		}
		if n < want {
			t.Errorf("%s: %d %s calls but %d allowed — LOWER IT to %d in this commit, so the "+
				"paydown is banked instead of left as room to regrow", pkg, n, what, want, n)
		}
	}
	var gone []string
	for pkg := range allowed {
		if _, still := got[pkg]; !still {
			gone = append(gone, pkg)
		}
	}
	sort.Strings(gone)
	if len(gone) > 0 {
		t.Errorf("these packages no longer make a %s call — delete them from the allowlist: %s",
			what, strings.Join(gone, ", "))
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

// stripComments blanks out `//` comment text before matching.
//
// IT IS HERE BECAUSE THIS GUARD CAUGHT ITS OWN PROSE. objenc was converted to take
// capability handles, and the comment recording WHY — "it used to reach for
// baoread.KVGetFieldOK and baoread.KVPut directly" — kept the package at its old
// count. The guard would have demanded an allowlist entry for a package that had
// just paid its debt off, which is the fastest way to teach someone that the
// allowlist is where you put things to make a test quiet.
//
// Same rule this campaign already had on record from the other direction: a rename
// rewrote the English word "answered" in six prose sentences because it was also a
// method name. Comments are not code, in both directions.
//
// Line-level `//` only. Block comments are deliberately not handled, for the
// reason the core-surface counter gives: this repo documents with `//`, and `/*`
// appears almost exclusively inside glob and regex string literals, where a naive
// scanner would swallow the rest of the file.
func stripComments(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for _, line := range bytes.Split(b, []byte("\n")) {
		if i := bytes.Index(line, []byte("//")); i >= 0 {
			// Not a comment if the marker is inside a string literal — the only
			// case in this tree is a URL, and cutting there would hide real code
			// after it on the same line.
			if bytes.Count(line[:i], []byte(`"`))%2 == 0 {
				line = line[:i]
			}
		}
		out = append(out, line...)
		out = append(out, '\n')
	}
	return out
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
		if n := len(re.FindAll(stripComments(b), -1)); n > 0 {
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
