package applydrift

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestRelevantCountsOnlyWhatAnApplyDelivers(t *testing.T) {
	got := Relevant([]string{
		"terraform-iac-bootstrap/cluster/main.tf",
		"landingzone.yaml",
		"environments/primary.yaml",
		// Everything below reaches the cluster WITHOUT an apply, so counting it
		// would report drift that is already live — and a check that cries wolf on
		// every apl-values commit is one people stop reading.
		"apl-values/primary/manifest/x.yaml",
		"apl-values/_shared/apl-overlay/apps.yaml",
		"docs/quickstart.md",
		"README.md",
		".github/workflows/llz-terraform.yml",
	})
	if len(got) != 3 {
		t.Errorf("expected the three apply-only paths, got %v", got)
	}
}

func TestRelevantDoesNotPrefixMatchTheSpecFile(t *testing.T) {
	// `landingzone.yaml` is an exact path; `environments/` is a subtree. A plain
	// HasPrefix would let landingzone.yaml.example — a template artifact no apply
	// reads — report the deployment as behind.
	if got := Relevant([]string{"landingzone.yaml.example"}); len(got) != 0 {
		t.Errorf("the example file is not the spec, got %v", got)
	}
	if got := Relevant([]string{"environments/lab.yaml"}); len(got) != 1 {
		t.Errorf("the environments subtree must match, got %v", got)
	}
}

func TestReportSaysUpToDateWithoutFailing(t *testing.T) {
	var b bytes.Buffer
	v := Verdict{Deployment: "primary", AppliedSHA: "abcdef1234"}
	if err := v.Report(&b, true); err != nil {
		t.Fatalf("no drift must never fail, even under --strict: %v", err)
	}
	if !strings.Contains(b.String(), "up to date") {
		t.Errorf("output must say so plainly, got: %s", b.String())
	}
}

func TestReportNamesTheRemedyAndTheFiles(t *testing.T) {
	// The whole value is that a reader knows what to do next. A drift report that
	// says only "behind" sends them to go and find the pipeline themselves.
	var b bytes.Buffer
	v := Verdict{
		Deployment: "prod",
		AppliedSHA: "abcdef1234",
		Behind:     []string{"terraform-iac-bootstrap/cluster/main.tf"},
	}
	_ = v.Report(&b, false)
	out := b.String()
	for _, want := range []string{"promote.yml", "llz build prod --yes", "terraform-iac-bootstrap/cluster/main.tf", "abcdef12"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestStrictDecidesWarningVersusFailure(t *testing.T) {
	v := Verdict{Deployment: "prod", AppliedSHA: "abcdef1234", Behind: []string{"landingzone.yaml"}}

	var warn bytes.Buffer
	if err := v.Report(&warn, false); err != nil {
		t.Errorf("without --strict a behind deployment warns rather than fails, got %v", err)
	}
	if !strings.Contains(warn.String(), "::warning") {
		t.Errorf("expected a warning annotation, got: %s", warn.String())
	}

	var strict bytes.Buffer
	if err := v.Report(&strict, true); err == nil {
		t.Error("--strict must fail the job")
	}
	if !strings.Contains(strict.String(), "::error") {
		t.Errorf("expected an error annotation, got: %s", strict.String())
	}
}

func TestEnvIsRequired(t *testing.T) {
	// A repo-wide answer would key off whichever deployment applied most recently
	// and call every other one up to date — wrong in the direction that matters.
	c := Cmd()
	c.SetArgs(nil)
	c.SetOut(&bytes.Buffer{})
	c.SetErr(&bytes.Buffer{})
	c.SilenceUsage, c.SilenceErrors = true, true
	if err := c.Execute(); err == nil || !strings.Contains(err.Error(), "--env is required") {
		t.Errorf("a missing --env must be refused, got %v", err)
	}
}

func TestNoApplyFoundIsAnErrorNotAPass(t *testing.T) {
	// The failure this whole verb exists to avoid, in miniature: "I could not find
	// an apply" must never render as "nothing to apply". A fresh instance and an
	// instance whose history has rolled past its last apply are both unanswered.
	orig := execSeam
	t.Cleanup(func() { execSeam = orig })
	execSeam = func(_ string, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "/jobs") {
			return []byte("Apply Cluster\nBootstrap OpenBao (somewhere-else)\n"), nil
		}
		return []byte(`{"id":1,"head_sha":"aaaa"}`), nil
	}
	_, _, err := lastApply("primary")
	if err == nil {
		t.Fatal("no matching apply must be an error")
	}
	if !strings.Contains(err.Error(), "NOT 'up to date'") {
		t.Errorf("the error must say what it does NOT mean, got: %v", err)
	}
}

func TestDeploymentMatchIsExact(t *testing.T) {
	// A deployment called `prod` must not match `Bootstrap OpenBao (prod-web)` —
	// that would report prod as applied when a different cluster was.
	orig := execSeam
	t.Cleanup(func() { execSeam = orig })
	execSeam = func(_ string, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "/jobs") {
			return []byte("Bootstrap OpenBao (prod-web)\n"), nil
		}
		return []byte(`{"id":1,"head_sha":"aaaa"}`), nil
	}
	if _, _, err := lastApply("prod"); err == nil {
		t.Error("a longer deployment name must not satisfy a shorter one")
	}
}

func TestUnreadableJobsIsAnErrorNotASkip(t *testing.T) {
	// A run whose jobs cannot be read might BE this deployment's last apply.
	// Skipping it would silently move the comparison to an older commit and
	// overstate drift, or run off the end and report a false "never applied".
	orig := execSeam
	t.Cleanup(func() { execSeam = orig })
	execSeam = func(_ string, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "/jobs") {
			return nil, fmt.Errorf("403")
		}
		return []byte(`{"id":1,"head_sha":"aaaa"}`), nil
	}
	_, _, err := lastApply("primary")
	if err == nil || !strings.Contains(err.Error(), "refusing to guess") {
		t.Errorf("an unreadable run must stop the check, got %v", err)
	}
}
