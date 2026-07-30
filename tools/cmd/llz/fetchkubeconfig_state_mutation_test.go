package main

// Gap-closing tests for fetchkubeconfig_state.go: the init retry's backoff and
// the "why did kubeconfig_raw read empty?" diagnostics. The diagnostics are the
// ONLY thing a run log carries when the composite fails, so a branch that
// silently prints the fallback instead of what it actually read turns a
// diagnosable failure back into the opaque one this command exists to explain.
//
// No network and no terraform: every read goes through the execOutput seam.

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// tfInitBackoff is what makes the init retry a RETRY rather than three attempts
// fired inside the same instant. init reaches the registry, git and the S3
// backend — a blip needs a beat to clear — so the wait must actually grow with
// the attempt number.
func TestTFInitBackoffWaitsAndGrows(t *testing.T) {
	for _, tc := range []struct {
		attempt int
		want    time.Duration
	}{
		{1, 5 * time.Second},
		{2, 10 * time.Second},
		{3, 15 * time.Second},
	} {
		if got := tfInitBackoff(tc.attempt); got != tc.want {
			t.Errorf("tfInitBackoff(%d) = %v, want %v (a collapsed backoff retries into the same blip)",
				tc.attempt, got, tc.want)
		}
	}
}

// tfDiagStub answers the three read-only terraform calls the diagnostics make,
// keyed on the joined args. A nil entry makes that call fail.
func tfDiagStub(t *testing.T, replies map[string]string) {
	t.Helper()
	withExecOutput(t, func(_ string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if reply, ok := replies[joined]; ok {
			return []byte(reply), nil
		}
		return nil, errors.New("terraform " + joined + ": exit status 1")
	})
}

// Every successful probe must be REPORTED. Each of these lines is a distinct
// diagnosis: the version tells you which binary read the state, the captured
// stderr says why `output -raw` came back empty, the output keys say whether
// the root even declares kubeconfig_raw, and the state resources say whether
// init landed on the state cluster-bootstrap wrote.
func TestFetchKubeconfigStateDiagnosticsReportsWhatItRead(t *testing.T) {
	tfDiagStub(t, map[string]string{
		"version":        "OpenTofu v1.12.5\non darwin_arm64\n+ provider registry.opentofu.org/linode/linode v3.0.0",
		"output -json":   `{"kubeconfig_raw":"","cluster_id":"123"}`,
		"state list":     "module.cluster.linode_lke_cluster.this\nrandom_password.loki",
		"unused-command": "",
	})
	out := captureStdout(t, func() {
		fetchKubeconfigStateDiagnostics("primary", "cluster/primary/terraform.tfstate", "llz-state",
			"Warning: Output \"kubeconfig_raw\" not found")
	})

	for _, want := range []string{
		"OpenTofu v1.12.5",                       // the version probe ran and printed
		"on darwin_arm64",                        // ...both of the two lines it keeps
		`Output "kubeconfig_raw" not found`,      // the captured stderr, verbatim
		"kubeconfig_raw",                         // an enumerated root output key
		"cluster_id",                             // ...and the rest of them
		"module.cluster.linode_lke_cluster.this", // the matching state resource
	} {
		if !strings.Contains(out, want) {
			t.Errorf("diagnostics dropped %q:\n%s", want, out)
		}
	}
	// The fallbacks are what a FAILED probe prints. Printing them while the probe
	// succeeded is the specific lie that sends an operator hunting the wrong state.
	for _, never := range []string{
		"(no stderr captured)",
		"(could not enumerate output keys)",
		"(no matching resources in state",
	} {
		if strings.Contains(out, never) {
			t.Errorf("printed the failure fallback %q despite a successful probe:\n%s", never, out)
		}
	}
	// Only the first two lines of `terraform version` are kept.
	if strings.Contains(out, "provider registry.opentofu.org") {
		t.Errorf("version output should be trimmed to two lines:\n%s", out)
	}
	// A non-matching state resource is not echoed.
	if strings.Contains(out, "random_password.loki") {
		t.Errorf("state list should be filtered to lke/kubeconfig rows:\n%s", out)
	}
}

// The mirror image: when every probe fails (or reads nothing) the diagnostics
// must SAY so rather than print an empty section that reads as "nothing wrong".
func TestFetchKubeconfigStateDiagnosticsFallsBackWhenNothingCanBeRead(t *testing.T) {
	tfDiagStub(t, nil) // every terraform call fails
	out := captureStdout(t, func() {
		fetchKubeconfigStateDiagnostics("primary", "cluster/primary/terraform.tfstate", "llz-state", "  \n\t ")
	})
	for _, want := range []string{
		"(no stderr captured)",
		"(could not enumerate output keys)",
		"(no matching resources in state",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing the %q fallback:\n%s", want, out)
		}
	}
}

// Unparseable `output -json` must land on the fallback, not on a half-read map.
func TestFetchKubeconfigStateDiagnosticsUnparseableOutputJSON(t *testing.T) {
	tfDiagStub(t, map[string]string{"output -json": "not json at all"})
	out := captureStdout(t, func() {
		fetchKubeconfigStateDiagnostics("primary", "k", "b", "boom")
	})
	if !strings.Contains(out, "(could not enumerate output keys)") {
		t.Errorf("unparseable output JSON should report as un-enumerable:\n%s", out)
	}
}

// Guard the recursive-re-entry hazard rather than relying on it never firing:
// renderRootsFn execs os.Executable(), which under `go test` IS the test binary,
// so an instance root found from the package dir would re-run the whole suite.
func TestRenderRootsIsANoOpWithoutAnInstanceRoot(t *testing.T) {
	chdir(t, t.TempDir())
	if err := renderRootsFn("primary"); err != nil {
		t.Errorf("no landingzone.yaml up-tree must be a silent no-op, got: %v", err)
	}
}
