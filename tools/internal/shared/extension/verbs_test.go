package extension_test

// internal/verbs IS NOT internal/extensions, and nothing in it may declare.
//
// The verbs are cobra commands and dev tooling — onboard, lint, upgrade,
// newinstance, selfupgrade, doctor, mutate, phase-timing, argocd-diagnostics.
// They were extensions once, and the reason they are not any more is that an
// extension is a thing an instance can HAVE or NOT HAVE. Nobody ships an instance
// with `lint` disabled; nobody enables `llz new`. Declaring them made
// `llz extension list` a list of two different kinds of thing, and made the
// in-degree ratchet treat a composite command (onboard imported six peers, lint
// five) as the same defect as substrate wearing a declaration.
//
// THIS GUARD IS WHAT KEEPS THE DISTINCTION FROM ERODING, because the erosion is
// one-directional and cheap: adding an extension.go to a verb package is a
// three-line change that makes it register, and nothing else would notice. The
// expensive direction — moving a real capability into verbs/ — is loud, because
// its verbs stop being reachability-checked.
//
// IT ALSO CARRIES A VERDICT THAT USED TO LIVE IN THREE DECLARATIONS. The refusal
// of a fifth `diagnostic` binding kind rested on the observed shapes of
// doctor-probes, phase-timing and argocd-diagnostics: one attached to a state with
// no note, one attached to no state, one was marked deliberately wrong. Those
// declarations are gone with this move, and the argument they supported is now
// carried structurally instead: the diagnostic family holds no binding kind
// because it holds no binding. See internal/verbs/argodiag/diagnostic_verdict_test.go.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerbsDoNotDeclareExtensions(t *testing.T) {
	root := filepath.Join("..", "..", "verbs")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read %s: %v", root, err)
	}

	var checked int
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		files, err := filepath.Glob(filepath.Join(root, e.Name(), "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			checked++
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			// The declaration's one required shape: a func returning the model's
			// Extension value. Matching the signature rather than the filename,
			// because `extension.go` is a convention and this rule is not.
			if strings.Contains(string(b), ") extension.Extension {") {
				t.Errorf("%s declares an extension — internal/verbs is for cobra commands and "+
					"dev tooling, which an instance cannot enable or disable. If this really is "+
					"a capability, move the package under internal/extensions/<bucket>/ so the "+
					"in-degree ratchet and the command-reachability check apply to it too",
					filepath.ToSlash(f))
			}
		}
	}

	// A guard that scans nothing passes forever. This is the same failure the
	// this tree has already paid for once, when a corpus walk found no files and
	// reported clean.
	if checked == 0 {
		t.Fatal("scanned no non-test sources under internal/verbs — the tree moved or the " +
			"glob is wrong, and this guard has been vacuous since it did")
	}
}
