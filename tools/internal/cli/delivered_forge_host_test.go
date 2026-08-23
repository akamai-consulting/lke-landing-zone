package cli

// delivered_forge_host_test.go — a git URL rewrite in the delivered tree must
// derive its host from GITHUB_SERVER_URL, never name a forge literally.
//
// THE FAILURE MODE, AND WHY IT IS SILENT. `git config url.<X>.insteadOf <Y>`
// rewrites a fetch only when Y is a PREFIX of the URL git was about to use. Name
// the host literally and on GHES the prefix matches nothing: the rewrite is
// configured, exits 0, and does not fire. What the operator sees is the fetch
// failing unauthenticated — `llz drift`'s `git ls-remote` against a private
// template reads as "the template is unreachable", not as "the credential
// rewrite never applied". Nothing errors at the point of the mistake.
//
// It shipped. llz-scheduled-checks.yml's template-drift job hardcoded
// github.com while llz-template-upgrade.yml derived the host correctly — and
// llz-template-upgrade.yml's own comment asserted that the drift job "sets the
// same rewrite for the same reason", which was false for as long as both files
// existed. Two sites, one rule, each holding its own copy: the split-contract
// shape docs/e2e-gates.md names, and the reason this is a gate rather than a
// fixed comment.
//
// GHES IS NOT HYPOTHETICAL — release-e2e runs a github-enterprise-server lane, so
// a delivered workflow that only works on github.com is a supported-target bug.

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// deliveredGitHubDir is the vendored .github tree every instance carries.
const deliveredGitHubDir = "../../../instance-template/.github"

// rewriteOperand captures the two halves of a rewrite that can carry a host: the
// replacement URL in `url."<X>"` and the prefix in `insteadOf "<Y>"`. Both are
// checked, because getting either one literal breaks the same way.
var rewriteOperand = regexp.MustCompile(`url\."([^"]*)"|insteadOf\s+"([^"]*)"`)

// shellExpansion is `${...}` — including the parameter-expansion forms these
// sites use (`${GITHUB_SERVER_URL#https://}`), whose own `https://` must not be
// mistaken for a literal host.
var shellExpansion = regexp.MustCompile(`\$\{[^}]*\}`)

// hostLiteral is a dot-separated name left over after every expansion is
// removed — `github.com`, `ghe.example.org`. `x-access-token` has no dot and so
// is not one.
var hostLiteral = regexp.MustCompile(`[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)+`)

func TestDeliveredGitRewritesDeriveTheForgeHost(t *testing.T) {
	var scanned int
	err := filepath.WalkDir(deliveredGitHubDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		switch filepath.Ext(path) {
		case ".yml", ".yaml", ".sh":
		default:
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range rewriteOperand.FindAllStringSubmatch(string(raw), -1) {
			operand := m[1] + m[2] // exactly one of the two alternatives matched
			scanned++
			bare := shellExpansion.ReplaceAllString(operand, "")
			if h := hostLiteral.FindString(bare); h != "" {
				t.Errorf("%s: git URL rewrite names the forge host literally: %q (found %q)\n"+
					"    A literal host is not a PREFIX of any URL on another forge, so the rewrite is\n"+
					"    configured, exits 0, and never fires — the fetch then fails unauthenticated and\n"+
					"    reads as an unreachable remote rather than as a rewrite that did not apply.\n"+
					"    Derive it: url.\"https://x-access-token:${GH_TOKEN}@${GITHUB_SERVER_URL#https://}/\"\n"+
					"    .insteadOf \"${GITHUB_SERVER_URL}/\" — see .github/actions/_lib/git-auth.sh.",
					path, operand, h)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", deliveredGitHubDir, err)
	}
	// FAIL CLOSED ON VACUITY. Every delivered credential rewrite is the corpus
	// here, so finding none means the regex stopped matching the shape rather than
	// that the tree is clean — and a gate reporting success over nothing looks
	// exactly like the bug it exists to catch.
	if scanned == 0 {
		t.Fatalf("no git URL rewrite found anywhere under %s — this gate examined NOTHING.\n"+
			"    Either the rewrites were removed (then remove this gate) or `url.\"…\"`/`insteadOf \"…\"` "+
			"no longer matches how they are written (then fix the pattern).", deliveredGitHubDir)
	}
	t.Logf("checked %d rewrite operand(s) across %s", scanned, deliveredGitHubDir)
}

// TestForgeHostLiteralIsActuallyDetected is the negative arm: the gate above is
// a regex over prose-heavy YAML, and the way it fails is by quietly matching
// nothing. Feed it the exact string that shipped and require a finding.
func TestForgeHostLiteralIsActuallyDetected(t *testing.T) {
	for _, tc := range []struct {
		name    string
		operand string
		wantHit bool
	}{
		{"the string that shipped", `https://x-access-token:${GH_TOKEN}@github.com/`, true},
		{"its insteadOf prefix", `https://github.com/`, true},
		{"derived, with parameter expansion", `https://x-access-token:${GH_TOKEN}@${GITHUB_SERVER_URL#https://}/`, false},
		{"derived insteadOf prefix", `${GITHUB_SERVER_URL}/`, false},
		{"the shared lib's ssh form", `ssh://git@${host}/`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := hostLiteral.FindString(shellExpansion.ReplaceAllString(tc.operand, "")) != ""
			if got != tc.wantHit {
				t.Errorf("detected=%t, want %t for %q", got, tc.wantHit, tc.operand)
			}
		})
	}
}
