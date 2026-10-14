package onboard

// pat_guidance_drift_test.go — the five copies of OPENBAO_SECRETS_WRITE_TOKEN's
// permission advice, gated.
//
// THIS DRIFT HAS NOW BEEN CAUGHT THREE TIMES BY REVIEW AND ZERO TIMES BY CI. The
// PAT needs two GitHub grants whose endpoints and consumers differ, and the
// advice is restated in SEVEN carriers:
//
//	1. ghFineGrainedSecretsWriteURL — the pre-filled creation link  } asserted
//	2. catalog()'s Note                                            } in THIS
//	3. secretsWritePATLabel — the on-screen choice                 } file
//	6. llz-bootstrap-openbao.yml's require-secret --hint           } (read off
//	7. llz-terraform.yml's require-secret --hint                   }  disk)
//
//	4. tokenprobe's Environments hint   } asserted in tokenprobe's
//	5. tokenprobe's repo-Secrets hint   } capability_test.go
//
// A round of fixes corrected four and missed one hint; the next corrected that
// and missed the label. Each miss printed advice leaving an operator with half
// the permission set — the outage this branch is about.
//
// THE ACCOUNTING ITSELF DRIFTED, in a file whose subject is accounting. An
// earlier header said "five copies", listed the two tokenprobe hints among them,
// then partitioned them "three here, two on disk" — a different five that
// omitted the hints — and closed by claiming all five were asserted here. Three
// mutually inconsistent counts in one comment, none of which mentioned the
// creation URL, the carrier that decides what the token is actually scoped for.
// Coverage was complete across the two packages; the description of it was not.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// assertNamesBothGrants is the property every copy must carry: it must send the
// reader to Environments AND Secrets. Naming only one is exactly the failure.
//
// CASE-INSENSITIVE, DELIBERATELY. The copies do not agree on casing and should
// not have to — the wizard's Note shouts "ENVIRONMENTS: write + SECRETS: write"
// because it is read while pasting a token, the workflow hints are sentence
// case. Asserting the exact spelling would fail this on a difference that costs
// the operator nothing, and a gate that cries about formatting is a gate people
// learn to edit rather than obey.
func assertNamesBothGrants(t *testing.T, where, text string) {
	t.Helper()
	lower := strings.ToLower(text)
	if !strings.Contains(lower, "environments: write") {
		t.Errorf("%s: must name Environments: write — without it the infra-<env> writeback 403s; got %q", where, text)
	}
	if !strings.Contains(lower, "secrets: write") {
		t.Errorf("%s: must name Secrets: write — without it the harbor-robot-provisioner 403s every tick; got %q", where, text)
	}
}

// 1. The pre-filled PAT creation URL — what the operator's browser asks GitHub for.
func TestPATCreationURLRequestsBothGrants(t *testing.T) {
	u := ghFineGrainedSecretsWriteURL("llz-openbao-secrets-write", "acme")
	for _, q := range []string{"environments=write", "secrets=write"} {
		if !strings.Contains(u, q) {
			t.Errorf("pre-fill must request %s; got %q", q, u)
		}
	}
}

// 2. catalog()'s Note — the wizard's printed explanation.
func TestCatalogNoteNamesBothGrants(t *testing.T) {
	for _, s := range catalog() {
		if s.Name == "OPENBAO_SECRETS_WRITE_TOKEN" {
			assertNamesBothGrants(t, "catalog() Note", s.Note)
			return
		}
	}
	t.Fatal("OPENBAO_SECRETS_WRITE_TOKEN is not in catalog() — the wizard would never ask for it")
}

// 3. The on-screen label — what the operator reads while pasting the token, and
// therefore the worst of the five to leave stale.
func TestGatherLabelNamesBothGrants(t *testing.T) {
	assertNamesBothGrants(t, "gatherGH label", secretsWritePATLabel("acme/platform"))
}

// 4. The delivered workflows' require-secret hints. On disk, the way the docs and
// scaffold guards read instance-template/ — a hint that drifts here reaches every
// adopter's CI log and nothing else would notice.
//
// A LOCAL `go test` CAN SHOW A STALE PASS HERE, and CI cannot. Go's test cache
// keys on the package's own inputs; these files live outside the module (../..),
// so editing one does not invalidate a cached result and a bare `go test ./...`
// may print ok having read nothing. CI runs this through `make coverage`, whose
// -coverprofile makes the run uncacheable, so the gate always executes there —
// verified by mutation both ways. When checking this gate by hand, pass
// -count=1; a cached green on a workflow edit means nothing.
func TestWorkflowRequireSecretHintsNameBothGrants(t *testing.T) {
	root := repoRootFromTest(t)
	for _, rel := range []string{
		"instance-template/.github/workflows/llz-bootstrap-openbao.yml",
		"instance-template/.github/workflows/llz-terraform.yml",
	} {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		body := string(b)
		if !strings.Contains(body, "OPENBAO_SECRETS_WRITE_TOKEN") {
			t.Fatalf("%s no longer mentions the credential — this gate would pass having examined nothing", rel)
		}
		// KEYED ON THE CREDENTIAL, NOT ON PROSE. This matched the first `--hint`
		// line containing "Fine-grained PAT" — and the APL_VALUES_REPO_TOKEN hint two
		// lines above says "Fine-grained GitHub PAT", missing by exactly one word.
		// Rewording that neighbour would have silently repointed this gate at the
		// wrong credential, where it would keep passing while the hint it exists to
		// guard drifted. The require-secret invocation names its credential, so the
		// hint is the `--hint` line in that invocation's block.
		hint := requireSecretHintFor(t, rel, body, "OPENBAO_SECRETS_WRITE_TOKEN")
		assertNamesBothGrants(t, rel, hint)
	}
}

// requireSecretHintFor returns the `--hint` text belonging to the
// `llz ci require-secret <NAME>` invocation for credential — the line that
// follows it, since the verb spans two lines with the hint continuing the first.
// Fails loudly when the invocation or its hint is absent: a gate that cannot
// find its subject must not report success.
func requireSecretHintFor(t *testing.T, where, body, credential string) string {
	t.Helper()
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		if !strings.Contains(l, "require-secret") || !strings.Contains(l, credential) {
			continue
		}
		// The hint is on a continuation line within a few lines of the invocation.
		for j := i; j < len(lines) && j < i+4; j++ {
			if strings.Contains(lines[j], "--hint") {
				return lines[j]
			}
		}
		t.Fatalf("%s: `require-secret %s` has no --hint within 4 lines", where, credential)
	}
	t.Fatalf("%s: no `require-secret %s` invocation found — this gate examined nothing", where, credential)
	return ""
}

// repoRootFromTest walks up from the package dir to the repo root (the directory
// holding instance-template/). Fails loudly rather than silently skipping: a
// gate that cannot find its subject must not report success.
func repoRootFromTest(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("cwd: %v", err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "instance-template")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not locate the repo root (no instance-template/ above the test) — this gate examined nothing")
	return ""
}
