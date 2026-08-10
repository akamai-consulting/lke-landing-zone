package extension_test

// A PACKAGE'S BUCKET MUST AGREE WITH ITS DECLARATION.
//
// internal/extensions is filed by what a thing IS — guards/ blocks a change,
// assertions/ produces a verdict, lifecycle/ moves or holds the platform. That
// index is only worth having if it is true, and the first version of it was not:
// `assert-objstore` sat in assertions/ while declaring a single
// `transition:converged[cloud-mutate]` and nothing else. It was filed by its NAME
// prefix, which is the same mistake `health-sla` made when it swept in
// incluster.go and the catalog made when it grouped a row by filename. Three times
// now, so this stops being a thing anyone has to remember.
//
// WHAT THIS RULE IS NOT. An earlier attempt keyed the bucket off whether a package
// holds a MUTATING grant, on the theory that mutation means lifecycle. It produces
// nonsense in both directions and is recorded here so nobody re-derives it:
//
//   - `assert-network` would move to lifecycle/. It CREATES A NAMESPACE — to test
//     whether admission policy rejects a bad manifest. The mutation exists to
//     produce the verdict; it is not what the package is for.
//   - `render` would move to assertions/, holding no mutating grant, because its
//     file write lives in internal/cli to keep the declaration honest. It is the
//     value-producer the whole product is downstream of.
//
// PURPOSE IS NOT DERIVABLE FROM GRANTS. So the rule below is mechanical only where
// the declaration is unambiguous, and an explicit list everywhere else — the same
// shape as grantStates ("judgement transcribed, not a derived fact") and
// allowedEdges ("measured, not chosen"). The mechanical half is what would have
// caught assert-objstore; the list is what a reviewer argues with.

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// mixedBucket pins where each package whose declaration spans several kinds is
// filed. Every line is a judgement about what the package is FOR, and the reason
// it can be argued with is that it had to be typed.
var mixedBucket = map[string]string{
	// The assert-* family: each mutates only to reach its verdict.
	"assertidentity": "assertions",
	"assertnetwork":  "assertions",
	"assertobs":      "assertions",
	"assertplatform": "assertions",
	"assertsecrets":  "assertions",
	"volumes":        "assertions", // assert-storage: reconciler lanes serve the assertion
	"manifestguard":  "assertions",
	"sustain":        "assertions",
	"tokeninv":       "assertions", // a predicate at three states; the invariant samples

	// These carry an assertion, but the assertion exists to confirm what the
	// package just DID. The transition is the point.
	"chartpublish": "lifecycle",
	"converge":     "lifecycle",
	"database":     "lifecycle",
	"environments": "lifecycle",
	"objenc":       "lifecycle",
	"teardown":     "lifecycle",
	"tofudriver":   "lifecycle",
}

func TestBucketAgreesWithDeclaration(t *testing.T) {
	root := filepath.Join("..", "..", "extensions")
	buckets, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read %s: %v", root, err)
	}

	var checked int
	seenMixed := map[string]bool{}
	for _, b := range buckets {
		if !b.IsDir() {
			continue
		}
		pkgs, err := os.ReadDir(filepath.Join(root, b.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range pkgs {
			if !p.IsDir() {
				continue
			}
			decl := filepath.Join(root, b.Name(), p.Name(), "extension.go")
			src, err := os.ReadFile(decl)
			if err != nil {
				continue // not every package under a bucket has to declare
			}
			checked++
			kinds := declaredKinds(string(src))
			want, mixed := bucketFor(kinds, p.Name())
			if mixed {
				seenMixed[p.Name()] = true
			}
			if want != "" && want != b.Name() {
				t.Errorf("%s is in %s/ but its declaration says %s/ (kinds: %s)\n"+
					"\tFile by what the package IS. If this is a judgement call, its "+
					"declaration spans several kinds and it belongs in mixedBucket with "+
					"the reason — not in whichever directory its name suggests.",
					p.Name(), b.Name(), want, strings.Join(kinds, "+"))
			}
		}
	}

	// The ratchet half: a mixedBucket line for a package that no longer spans kinds
	// is a judgement nobody needs any more, and a stale one reads as endorsement.
	var stale []string
	for name := range mixedBucket {
		if !seenMixed[name] {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("%d mixedBucket entry(ies) no longer span several kinds — DELETE them, so the "+
			"mechanical rule covers them instead of a hand-written line:\n\t%s",
			len(stale), strings.Join(stale, "\n\t"))
	}

	if checked == 0 {
		t.Fatal("scanned no declarations — the tree moved and this guard has been vacuous since")
	}
}

// bucketFor returns the bucket a declaration implies, and whether the answer came
// from mixedBucket rather than from the kinds themselves.
func bucketFor(kinds []string, pkg string) (string, bool) {
	has := map[string]bool{}
	for _, k := range kinds {
		has[k] = true
	}
	switch {
	case len(kinds) == 0:
		return "", false
	// A gate is file-in, findings-out and may hold read-repo alone. Nothing else
	// looks like that, so an all-gate declaration needs no judgement.
	case len(kinds) == 1 && has["Gate"]:
		return "guards", false
	// Observes and nothing else.
	case len(kinds) == 1 && has["Assertion"]:
		return "assertions", false
	// Acts or holds, and makes no claim about a state. assert-objstore is this
	// case: a lone transition wearing an assert- prefix.
	case !has["Assertion"] && !has["Gate"]:
		return "lifecycle", false
	default:
		return mixedBucket[pkg], true
	}
}

func declaredKinds(src string) []string {
	var out []string
	for _, k := range []string{"Transition", "Assertion", "Invariant", "Gate"} {
		if strings.Contains(src, "extension."+k+",") {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
