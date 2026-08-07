package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
)

// seedroots_test.go — every literal `secret/<ns>/…` root in the WHOLE tools tree
// must have its top segment reserved in clusterspec.SystemSecretNamespaces.
//
// IT WALKS THE TREE NOW, AND THAT IS THE POINT. This guard used to read
// `os.ReadDir(".")` from cmd/llz, which was correct exactly as long as every seed
// root lived in cmd/llz. The identity-plane extraction moved its FILE into
// internal/identityconfig and the guard silently went inert — it kept passing
// while scanning a directory that declares no seed roots at all. Only its own
// `seen == 0` check caught it, which is the one reason it is not still green and
// meaningless today.
//
// A guard scoped to a DIRECTORY follows the file it is written in, not the code
// it guards. Walking tools/ costs a few milliseconds and cannot be hollowed out
// by the next extraction.
//
// THE ROOT IS "../.." AND THAT IS NOT A DETAIL. The first repair of this guard
// wrote ".." — the parent of cmd/llz is cmd/, not tools/ — and it passed anyway,
// because seed roots still happened to be declared in cmd/llz itself. The very
// next extraction moved them out and the seen==0 check fired again. A relative
// walk root is the same class of bug as the ReadDir(".") it replaced.

// The denylist guard above derives the protected set from the platform POLICIES,
// which leaves a blind spot: a namespace that code WRITES but no policy names is
// invisible to it. `secret/infra/db-admin/*` sat in exactly that gap — the
// db-admin seeder wrote there from the start, yet `platform` was claimable by a
// team (and so self-grantable to write) until a policy happened to reference it.
//
// This closes the class rather than the instance: every literal secret/ root
// declared anywhere under tools/ must have its top segment reserved, whether or not any
// policy mentions it.
func TestSeedTargetsAreReservedNamespaces(t *testing.T) {
	// `const x = "secret/<ns>/…"` — the shape every seed/sample root uses.
	re := regexp.MustCompile(`(?m)^const [A-Za-z]+ = "secret/([a-z0-9-]+)/`)
	seen := 0
	walkErr := filepath.Walk("../..", func(name string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		for _, m := range re.FindAllStringSubmatch(string(src), -1) {
			seen++
			if !clusterspec.SystemSecretNamespaces[m[1]] {
				t.Errorf("%s declares a secret root under secret/%s/ but %q is NOT in clusterspec.SystemSecretNamespaces — a team could claim secret/%s and self-grant write on it; add it to the denylist in clusterspec/validate.go",
					name, m[1], m[1], m[1])
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk tools tree: %v", walkErr)
	}
	// A regex that silently matches nothing would make this test vacuously color.Green.
	if seen == 0 {
		t.Error("matched no `const … = \"secret/<ns>/…\"` declarations; the pattern has drifted from the code and this guard is inert")
	}
}
