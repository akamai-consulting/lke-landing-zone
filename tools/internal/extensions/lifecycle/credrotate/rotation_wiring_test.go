package credrotate

// rotation_wiring_test.go — the delivered rotation stopped passing two
// identifiers, and llz has to be the thing that supplies them.
//
// This is a SPLIT CONTRACT with the halves in different languages. The producer
// is instance-template/.github/workflows/llz-secret-rotation.yml, which omits
// `label:` (and `bucket-cluster:`) under comments reading "omitted on purpose —
// llz derives the instance-scoped label". The consumer is the four `llz
// credentials {pat,obj-key} {create,revoke-old}` verbs. Both halves shipped;
// neither did the deriving. rotation_identity.go had the resolvers, with tests,
// and no call sites, so every verb ran with label "".
//
// What that cost, per verb: `pat create` minted the account's shared PAT with no
// label; `pat revoke-old` matched nothing and became a permanent no-op, so
// superseded PATs accumulate toward the 100-token cap while the daily job reports
// success; and `obj-key create` — armed monthly by routeRotation — minted a key
// against cluster "" and wrote it over TF_STATE_ACCESS_KEY/SECRET_KEY in every
// infra-<deployment> environment.
//
// So the gate reads the REAL delivered workflow to establish what the producer
// omits, and drives the REAL cobra commands with the argv the composite action
// builds from those omissions. Neither side's rule is restated here.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

const wiringSpec = `apiVersion: llz.akamai.com/v1alpha1
kind: LandingZone
metadata:
  name: acme
spec:
  instance:
    objLabelPrefix: acme
`

// deliveredRotationWorkflow is the producer half, read rather than described.
func deliveredRotationWorkflow(t *testing.T) string {
	t.Helper()
	p := filepath.Join("..", "..", "..", "..", "..",
		"instance-template", ".github", "workflows", "llz-secret-rotation.yml")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Skipf("delivered workflow not readable from this checkout (%v)", err)
	}
	return string(b)
}

var credStepRe = regexp.MustCompile(`(?m)^\s*uses:\s*\./\.github/actions/linode-credentials\s*$`)

// stepBody returns the `with:` block following each linode-credentials step.
func credentialSteps(t *testing.T, wf string) []string {
	t.Helper()
	lines := strings.Split(wf, "\n")
	var out []string
	for i, l := range lines {
		if !credStepRe.MatchString(l) {
			continue
		}
		// The step's remaining lines, to the next `- name:` at the same or
		// shallower indent. Good enough to read a `with:` block, and it fails
		// loudly (empty body) rather than quietly if the shape changes.
		var b strings.Builder
		for _, n := range lines[i+1:] {
			if strings.HasPrefix(strings.TrimSpace(n), "- name:") {
				break
			}
			b.WriteString(n)
			b.WriteString("\n")
		}
		out = append(out, b.String())
	}
	return out
}

// THE PRODUCER HALF. If a future edit puts the literals back, the consumer-side
// assertions below stop describing what actually happens — so the omission is
// pinned rather than assumed.
func TestTheDeliveredRotationPassesNoCredentialIdentity(t *testing.T) {
	steps := credentialSteps(t, deliveredRotationWorkflow(t))
	if len(steps) < 4 {
		t.Fatalf("found %d linode-credentials step(s), want the four rotation verbs — "+
			"this gate can no longer see what the workflow passes", len(steps))
	}
	for i, s := range steps {
		if regexp.MustCompile(`(?m)^\s+label:\s*\S`).MatchString(s) {
			t.Errorf("step %d passes an explicit `label:`; llz's derivation is no longer what "+
				"decides the label, and the tests below are testing something the workflow does not do", i)
		}
		if regexp.MustCompile(`(?m)^\s+bucket-cluster:\s*\S`).MatchString(s) {
			t.Errorf("step %d passes an explicit `bucket-cluster:`; see above", i)
		}
	}
}

// ── the consumer half ────────────────────────────────────────────────────────

// recordedPAT / recordedObjKey capture what the verb asked the Linode API for.
type recordedPAT struct {
	createdLabel string
	listed       bool
}

func (r *recordedPAT) CreateProfileToken(_ context.Context, label, _, _ string) (map[string]any, error) {
	r.createdLabel = label
	return map[string]any{"id": json.Number("1"), "token": "t", "label": label}, nil
}
func (r *recordedPAT) ListProfileTokens(context.Context) ([]map[string]any, error) {
	r.listed = true
	return nil, nil
}
func (r *recordedPAT) DeleteProfileToken(context.Context, uint64) error { return nil }

type recordedObjKey struct {
	createdLabel, createdCluster string
	listed                       bool
}

func (r *recordedObjKey) CreateObjectStorageKey(_ context.Context, label, cluster, _, _ string) (map[string]any, error) {
	r.createdLabel, r.createdCluster = label, cluster
	return map[string]any{"id": json.Number("1"), "access_key": "a", "secret_key": "s", "label": label}, nil
}
func (r *recordedObjKey) ListObjectStorageKeys(context.Context) ([]map[string]any, error) {
	r.listed = true
	return nil, nil
}
func (r *recordedObjKey) DeleteObjectStorageKey(context.Context, uint64) error { return nil }

// inInstanceRepo runs fn with the process rooted in a throwaway instance repo
// carrying a spec, which is where the derivation reads its prefix from.
func inInstanceRepo(t *testing.T, fn func()) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "landingzone.yaml"), []byte(wiringSpec), 0o644); err != nil {
		t.Fatal(err)
	}
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
	fn()
}

// runVerb executes one real cobra command with the argv the composite action
// builds. The action interpolates an EMPTY input into a quoted flag, so what
// reaches llz is a present-but-empty `--label ""` — not an absent flag. Passing
// it explicitly is the whole point: an absent flag would exercise the flag
// default, which is a different code path from the one CI takes.
func runVerb(t *testing.T, cmd string, extra ...string) {
	t.Helper()
	t.Setenv("LINODE_TOKEN", "fake-token-for-a-dry-run")
	// --apply is a flag on the PARENT `credentials` command, so a leaf argv
	// cannot carry it; the action arms the rotation through ROTATION_APPLY/Opts,
	// which is what this mirrors. Armed on purpose: a dry run never reaches the
	// API, and what reaches the API is the whole subject here.
	o := &Opts{Apply: true}
	var c *cobra.Command
	switch cmd {
	case "pat create":
		c = CredentialsPATCreateCmd(o)
	case "pat revoke-old":
		c = CredentialsPATRevokeOldCmd(o)
	case "obj-key create":
		c = CredentialsObjKeyCreateCmd(o)
	case "obj-key revoke-old":
		c = CredentialsObjKeyRevokeOldCmd(o)
	default:
		t.Fatalf("unknown verb %q", cmd)
	}
	c.SetArgs(append([]string{"--label", ""}, extra...))
	if err := c.Execute(); err != nil {
		t.Fatalf("llz credentials %s: %v", cmd, err)
	}
}

// EVERY VERB DERIVES. One test per verb rather than a loop, because the thing
// each one must produce differs and a shared assertion would hide which.
func TestPATCreateDerivesTheInstanceScopedLabel(t *testing.T) {
	rec := &recordedPAT{}
	restore := NewPATClient
	NewPATClient = func(string) PATAPI { return rec }
	t.Cleanup(func() { NewPATClient = restore })

	inInstanceRepo(t, func() { runVerb(t, "pat create") })

	if rec.createdLabel != "llz-acme-linode-api-token" {
		t.Errorf("minted the account's shared PAT with label %q, want the instance-scoped one — "+
			"an unlabeled PAT is one the daily reaper can never find", rec.createdLabel)
	}
}

func TestPATRevokeOldDerivesTheLabelItDrains(t *testing.T) {
	rec := &recordedPAT{}
	restore := NewPATClient
	NewPATClient = func(string) PATAPI { return rec }
	t.Cleanup(func() { NewPATClient = restore })

	// The label is not visible on a call here (revoke-old only LISTS, then
	// filters in Go), so the assertion is on the resolver the verb reaches
	// through — driven by the same argv, in the same working directory.
	inInstanceRepo(t, func() {
		runVerb(t, "pat revoke-old")
		got, err := resolveRotationLabel("", rotationKindPAT, "test")
		if err != nil || got == "" {
			t.Fatalf("resolve = (%q, %v), want the instance-scoped label", got, err)
		}
	})
	if !rec.listed {
		t.Error("revoke-old never listed the account's PATs")
	}
}

func TestObjKeyCreateDerivesBothTheLabelAndTheCluster(t *testing.T) {
	rec := &recordedObjKey{}
	restore := NewObjKeyClient
	NewObjKeyClient = func(string) ObjKeyAPI { return rec }
	t.Cleanup(func() { NewObjKeyClient = restore })

	// TF_STATE_ENDPOINT is what the action forwards as `state-endpoint`, and it
	// is the only thing that can say which cluster the state bucket lives in.
	t.Setenv("TF_STATE_ENDPOINT", "https://us-sea-1.linodeobjects.com")
	inInstanceRepo(t, func() {
		runVerb(t, "obj-key create", "--bucket-cluster", "", "--bucket-name", "acme-tfstate")
	})

	if rec.createdLabel != "llz-acme-tf-state-key" {
		t.Errorf("label = %q, want the instance-scoped one", rec.createdLabel)
	}
	if rec.createdCluster != "us-sea-1" {
		t.Errorf("minted the TF-state key against cluster %q, want us-sea-1 from TF_STATE_ENDPOINT — "+
			"a bucket is reachable only at the endpoint it was created against, so any other value "+
			"produces a key that cannot read the state it just overwrote the credentials for",
			rec.createdCluster)
	}
}

// AND IT REFUSES RATHER THAN GUESSING. With no endpoint there is no honest
// answer, and the monthly cron ARMS this verb — so stopping is the only safe
// outcome. A default here would be the literal `us-ord-10` all over again.
func TestObjKeyCreateRefusesWhenTheClusterCannotBeEstablished(t *testing.T) {
	rec := &recordedObjKey{}
	restore := NewObjKeyClient
	NewObjKeyClient = func(string) ObjKeyAPI { return rec }
	t.Cleanup(func() { NewObjKeyClient = restore })

	t.Setenv("TF_STATE_ENDPOINT", "")
	t.Setenv("LINODE_TOKEN", "fake-token-for-a-dry-run")
	inInstanceRepo(t, func() {
		o := &Opts{Apply: true}
		c := CredentialsObjKeyCreateCmd(o)
		c.SetArgs([]string{"--label", "", "--bucket-cluster", ""})
		err := c.Execute()
		if err == nil {
			t.Fatal("obj-key create proceeded with no way to know the cluster")
		}
		if !strings.Contains(err.Error(), "TF_STATE_ENDPOINT") {
			t.Errorf("refusal should name what to set: %v", err)
		}
	})
	if rec.createdLabel != "" || rec.createdCluster != "" {
		t.Errorf("a key was minted anyway: label=%q cluster=%q", rec.createdLabel, rec.createdCluster)
	}
}

// AN EXPLICIT --label STILL WINS, so an operator can drain a legacy label by
// hand without editing the workflow. The derivation is a default, not a seizure.
func TestAnExplicitLabelIsNotOverriddenByTheDerivation(t *testing.T) {
	rec := &recordedPAT{}
	restore := NewPATClient
	NewPATClient = func(string) PATAPI { return rec }
	t.Cleanup(func() { NewPATClient = restore })

	inInstanceRepo(t, func() {
		t.Setenv("LINODE_TOKEN", "fake-token-for-a-dry-run")
		o := &Opts{Apply: true}
		c := CredentialsPATCreateCmd(o)
		c.SetArgs([]string{"--label", "gha-platform-platform_LINODE_API_TOKEN"})
		if err := c.Execute(); err != nil {
			t.Fatal(err)
		}
	})
	if rec.createdLabel != "gha-platform-platform_LINODE_API_TOKEN" {
		t.Errorf("label = %q, want the explicit one the operator passed", rec.createdLabel)
	}
}
