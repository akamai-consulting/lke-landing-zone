package main

import (
	"os"
	"strings"
	"testing"
)

// The image-skew REPORT test, which stays in main: reportCIImageSkew is
// commands.go's. The computation it reports on is templatecommit's and its tests
// went there. Two subjects, one old file.

// TestReportCIImageSkew pins the WARNING, which is the entire deliverable of the
// `llz upgrade` half: the command cannot fix these (they are GitHub repo
// variables and it pushes nothing), so what it prints is all the operator gets
// before CI tells them the same thing 20 minutes later.
func TestReportCIImageSkew(t *testing.T) {
	const oldSHA = "0000000111111112222222333333344444445555"

	setup := func(t *testing.T, tfImage string) {
		t.Helper()
		pinnedAt(t, "v0.0.41")
		if err := os.MkdirAll(".llz", 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(".llz/vars.env", []byte("TF_IMAGE="+tfImage+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(".copier-answers.yml",
			[]byte("_src_path: gh:acme/tmpl\nllz_version: v0.0.41\ninstance_repo: ch-org/ch-instance\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("names both routes back, and the instance repo", func(t *testing.T) {
		setup(t, "ghcr.io/akamai-consulting/ci-tofu:sha-"+oldSHA)
		_, errOut := captureStdoutStderr(t, func() { reportCIImageSkew("v0.0.41") })

		// The remediation is useless without the repo it targets, and `<owner>/<name>`
		// left as a placeholder is the difference between copy-paste and a lookup.
		for _, want := range []string{
			"TF_IMAGE",
			oldSHA,               // what they have
			repinSHA,             // what they need
			"ch-org/ch-instance", // resolved, not a placeholder
			"llz tokens --env <env> --yes",
			"gh variable set TF_IMAGE",
			"assert-image-fresh", // names the guard that will otherwise say it in CI
		} {
			if !strings.Contains(errOut, want) {
				t.Errorf("warning is missing %q:\n%s", want, errOut)
			}
		}
	})

	// Silence is load-bearing: this runs on EVERY upgrade, and a warning that fires
	// when nothing is wrong is one operators learn to scroll past.
	t.Run("prints nothing when the images already match the new pin", func(t *testing.T) {
		setup(t, "ghcr.io/akamai-consulting/ci-tofu:sha-"+repinSHA)
		out, errOut := captureStdoutStderr(t, func() { reportCIImageSkew("v0.0.41") })
		if out != "" || errOut != "" {
			t.Errorf("warned with nothing to warn about:\nstdout=%q\nstderr=%q", out, errOut)
		}
	})

	t.Run("prints nothing when there is no vars.env to read", func(t *testing.T) {
		pinnedAt(t, "v0.0.41")
		out, errOut := captureStdoutStderr(t, func() { reportCIImageSkew("v0.0.41") })
		if out != "" || errOut != "" {
			t.Errorf("warned outside a provisioned instance:\nstdout=%q\nstderr=%q", out, errOut)
		}
	})
}

// TestPrintNextSteps guards the post-scaffold list, which is the quickstart an
// adopter actually reads — it is on their screen when the doc is not. It had
// drifted from docs/quickstart.md in both directions the drift can go: a step
// the doc gained (#405's `git push`) and a step the doc dropped (the deprecated
// `llz validate --env`). Neither gate that watches the other copy can see this
// one — docs-guard reads Markdown, and these are Go string literals — so the
// assertions live here.
