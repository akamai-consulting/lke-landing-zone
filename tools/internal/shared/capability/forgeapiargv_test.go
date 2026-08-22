package capability_test

// forgeapiargv_test.go — the classifier and pflag must agree about one argv.
//
// ClassifyForge decides whether a `gh api` invocation is a read or a write, and
// a cloud-read handle permits everything it calls a read. So every disagreement
// between this classifier and the parser gh ACTUALLY uses is a write that a
// read-only binding performs — the fence reporting a GET while a DELETE goes out.
//
// Three spellings set the same flag under pflag: `-X DELETE`, `-X=DELETE` and
// the ATTACHED `-XDELETE`. The classifier handled the first two and read the
// third as a bare positional, so `gh api -XDELETE repos/o/r` classified as a
// read. The same gap made `-ftitle=x` a read while gh sends POST, because a
// parameter flag it did not recognise was skipped rather than refused.
//
// The table is written as ARGV → what gh would SEND, so a row is checkable
// against `gh api --help` rather than against this package's opinion.

import (
	"errors"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

func TestEveryPflagSpellingOfTheMethodIsClassifiedTheSame(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv []string
		want capability.ForgeAction
	}{
		// ── the method flag, in all three pflag spellings ────────────────────
		{"spaced", []string{"api", "-X", "DELETE", "repos/o/r"}, capability.ForgeMutate},
		{"equals", []string{"api", "-X=DELETE", "repos/o/r"}, capability.ForgeMutate},
		{"attached", []string{"api", "-XDELETE", "repos/o/r"}, capability.ForgeMutate},
		{"attached lowercase", []string{"api", "-Xdelete", "repos/o/r"}, capability.ForgeMutate},
		{"attached long", []string{"api", "--method=DELETE", "repos/o/r"}, capability.ForgeMutate},
		{"spaced long", []string{"api", "--method", "PATCH", "repos/o/r"}, capability.ForgeMutate},
		{"attached read", []string{"api", "-XGET", "repos/o/r"}, capability.ForgeRead},

		// ── LAST WINS, in every spelling, because that is what pflag does ────
		{"repeat spaced then attached", []string{"api", "-X", "GET", "-XDELETE", "x"}, capability.ForgeMutate},
		{"repeat attached then spaced", []string{"api", "-XDELETE", "-X", "GET", "x"}, capability.ForgeRead},

		// ── parameters flip gh's default from GET to POST ────────────────────
		{"raw-field spaced", []string{"api", "repos/o/r/issues", "-f", "title=x"}, capability.ForgeMutate},
		{"raw-field attached", []string{"api", "repos/o/r/issues", "-ftitle=x"}, capability.ForgeMutate},
		{"field attached", []string{"api", "repos/o/r/issues", "-Ftitle=x"}, capability.ForgeMutate},
		{"raw-field long attached", []string{"api", "x", "--raw-field=title=y"}, capability.ForgeMutate},
		{"input", []string{"api", "x", "--input", "-"}, capability.ForgeMutate},
		{"explicit GET beats the parameter inference", []string{"api", "-X", "GET", "x", "-f", "a=b"}, capability.ForgeRead},

		// ── a flag VALUE is never read as a flag or a positional ─────────────
		{"a field named method", []string{"api", "x", "-f", "method=DELETE"}, capability.ForgeMutate},
		{"a jq filter spelled graphql", []string{"api", "x", "--jq", "graphql"}, capability.ForgeRead},
		{"a header value that looks like a flag", []string{"api", "x", "-H", "-XDELETE"}, capability.ForgeRead},

		// ── clustered booleans, which pflag allows ───────────────────────────
		{"bool cluster then value", []string{"api", "-iXDELETE", "x"}, capability.ForgeMutate},
		{"bool cluster alone", []string{"api", "-i", "x"}, capability.ForgeRead},

		// ── plain reads ──────────────────────────────────────────────────────
		{"bare", []string{"api", "repos/o/r"}, capability.ForgeRead},
		{"paginate slurp", []string{"api", "--paginate", "--slurp", "x"}, capability.ForgeRead},
		{"silent", []string{"api", "repos/o/r", "--silent"}, capability.ForgeRead},
		{"jq long", []string{"api", "users/o", "--jq", ".type"}, capability.ForgeRead},
		{"hostname", []string{"api", "--hostname", "ghes.example", "x"}, capability.ForgeRead},

		// ── a write is graded by WHAT it writes, not only by its verb ────────
		{"secret write via the API", []string{"api", "-X", "PUT", "repos/o/r/actions/secrets/FOO"}, capability.ForgeCustody},
		{"secret write, attached shorthand", []string{"api", "-XPUT", "repos/o/r/actions/secrets/FOO"}, capability.ForgeCustody},
		{"secret delete", []string{"api", "-X", "DELETE", "repos/o/r/actions/secrets/FOO"}, capability.ForgeCustody},
		{"org secret", []string{"api", "-X", "PUT", "orgs/o/actions/secrets/FOO"}, capability.ForgeCustody},
		{"environment secret", []string{"api", "-X", "PUT", "repos/o/r/environments/prod/secrets/FOO"}, capability.ForgeCustody},
		{"codespaces secret", []string{"api", "-X", "PUT", "repos/o/r/codespaces/secrets/FOO"}, capability.ForgeCustody},
		{"dependabot secret", []string{"api", "-X", "PUT", "repos/o/r/dependabot/secrets/FOO"}, capability.ForgeCustody},
		{"parameter-inferred POST to a secret path", []string{"api", "repos/o/r/actions/secrets/FOO", "-f", "encrypted_value=x"}, capability.ForgeCustody},
		// READS STAY READS. envreq lists this exact path to discover which
		// credentials are configured; knowing a secret exists is not holding it.
		{"secret list", []string{"api", "repos/o/r/actions/secrets"}, capability.ForgeRead},
		{"secret read, explicit GET", []string{"api", "-X", "GET", "repos/o/r/actions/secrets/FOO"}, capability.ForgeRead},
		{"public key read", []string{"api", "repos/o/r/actions/secrets/public-key"}, capability.ForgeRead},
		// And an ordinary mutation is still an ordinary mutation.
		{"branch policy write", []string{"api", "-X", "PUT", "repos/o/r/environments/prod/deployment-branch-policies"}, capability.ForgeMutate},
		{"a repo whose NAME contains secrets", []string{"api", "-X", "PUT", "repos/o/my-secrets-repo/actions/variables/FOO"}, capability.ForgeMutate},

		// ── refusals: unknowable, not guessable ──────────────────────────────
		{"graphql", []string{"api", "graphql", "-f", "query=mutation{}"}, capability.ForgeUnclassified},
		{"graphql after the terminator", []string{"api", "--", "graphql"}, capability.ForgeUnclassified},
		{"unknown method", []string{"api", "-XTRACE", "x"}, capability.ForgeUnclassified},
		{"dangling spaced", []string{"api", "-X"}, capability.ForgeUnclassified},
		{"dangling attached-cluster", []string{"api", "-iX"}, capability.ForgeUnclassified},
		{"unknown long flag", []string{"api", "--future-thing", "x"}, capability.ForgeUnclassified},
		{"unknown shorthand", []string{"api", "-Z", "x"}, capability.ForgeUnclassified},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := capability.ClassifyForge(tc.argv); got != tc.want {
				t.Errorf("ClassifyForge(%q) = %v, want %v", tc.argv, got, tc.want)
			}
		})
	}
}

// AND THE CLASSIFICATION IS WHAT THE HANDLE ACTS ON. The table above is a pure
// function; this is the fence itself refusing the argv that started it, because
// a correct classifier wired to nothing is the shape this repo keeps finding.
func TestACloudReadHandleRefusesAnAttachedShorthandWrite(t *testing.T) {
	h := capability.For(binding(extension.CloudRead))
	for _, argv := range [][]string{
		{"api", "-XDELETE", "repos/o/r"},
		{"api", "-ftitle=x", "repos/o/r/issues"},
	} {
		err := h.Forge.Permits(argv...)
		if !errors.Is(err, capability.ErrNoForgeMutate) {
			t.Errorf("a cloud-read handle answered %q with %v, want ErrNoForgeMutate", argv, err)
		}
	}
	// The spaced form was always refused; it is here so a regression that
	// loosens BOTH spellings cannot pass by loosening them together.
	if err := h.Forge.Permits("api", "-X", "DELETE", "repos/o/r"); !errors.Is(err, capability.ErrNoForgeMutate) {
		t.Errorf("spaced form = %v, want ErrNoForgeMutate", err)
	}
	// And an ordinary read still works, or the fence gets widened back.
	if err := h.Forge.Permits("api", "repos/o/r", "--silent"); err != nil {
		t.Errorf("a plain read through a cloud-read handle = %v, want nil", err)
	}
}

// THE FENCE ACTS ON IT. branchpolicy declares cloud-mutate and NOT
// secret-custody, and its own header cites that as the reason a `gh secret set`
// from there would be refused. The API spelling of the same operation was not —
// so the claim held for one way of writing a secret and not the other.
func TestACloudMutateHandleCannotWriteASecretThroughTheAPI(t *testing.T) {
	h := capability.For(binding(extension.CloudMutate))
	if err := h.Forge.Permits("api", "-X", "PUT", "repos/o/r/actions/secrets/FOO"); !errors.Is(err, capability.ErrNoForgeCustody) {
		t.Errorf("a cloud-mutate handle wrote a GitHub secret through `gh api`: %v", err)
	}
	// The mutation it IS entitled to still goes through, or the grant means
	// nothing in the other direction.
	if err := h.Forge.Permits("api", "-X", "PUT", "repos/o/r/environments/prod/deployment-branch-policies"); err != nil {
		t.Errorf("a cloud-mutate handle must still write non-credential resources: %v", err)
	}
	// And it can still LIST secrets: envreq's discovery depends on it.
	if err := h.Forge.Permits("api", "repos/o/r/actions/secrets"); err != nil {
		t.Errorf("listing secret NAMES is a read: %v", err)
	}
}

// AN UNKNOWN FLAG IS REFUSED WITH ITS OWN NAME IN THE MESSAGE. Failing closed is
// only usable if the reader can tell WHY, and "unclassified" over an argv of ten
// tokens sends them to gh's manual to guess which one.
func TestARefusedArgvNamesItself(t *testing.T) {
	err := capability.For(binding(extension.CloudRead)).Forge.Permits("api", "--future-thing", "x")
	if err == nil {
		t.Fatal("an unknown flag was permitted")
	}
	if !strings.Contains(err.Error(), "--future-thing") {
		t.Errorf("refusal does not echo the argv that caused it: %v", err)
	}
	// And it points at the table that actually needs the edit. The generic
	// remedy names the COMMAND table, which is not what fell short here.
	if !strings.Contains(err.Error(), "ghAPIFlags") {
		t.Errorf("refusal does not say which table to fix: %v", err)
	}
}
