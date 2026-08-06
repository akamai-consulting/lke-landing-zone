package assertobs

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/kubectlprobe"
)

// TestMain zeroes the probe retry delay — without it every stubbed-error test
// pays 6 real seconds (3 retries x 3s). internal/converge lost this line in
// extraction and its suite went 4s -> 568s.
func TestMain(m *testing.M) {
	kubectlprobe.Delay = 0
	os.Exit(m.Run())
}

// withExecOutput swaps this package's Exec seam AND kubectlprobe's. Both, because
// these lanes read the cluster two ways; stubbing one leaves the other reaching
// for a real cluster, which earlier extractions established is a hang rather than
// a failure.
func withExecOutput(t *testing.T, fn func(name string, args ...string) ([]byte, error)) {
	t.Helper()
	origExec, origProbe := caps.Exec, kubectlprobe.Exec
	caps.Exec = fn
	kubectlprobe.Exec = fn
	t.Cleanup(func() { caps.Exec, kubectlprobe.Exec = origExec, origProbe })
}

// withKubectlOut swaps the string-returning kubectl seam.
func withKubectlOut(t *testing.T, fn func(args ...string) (string, error)) {
	t.Helper()
	orig := caps.KubectlOut
	caps.KubectlOut = fn
	t.Cleanup(func() { caps.KubectlOut = orig })
}

func captureStdout(t *testing.T, fn func()) string { t.Helper(); return capture(t, &os.Stdout, fn) }
func captureStderr(t *testing.T, fn func()) string { t.Helper(); return capture(t, &os.Stderr, fn) }

func capture(t *testing.T, target **os.File, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := *target
	*target = w
	done := make(chan string)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	fn()
	*target = orig
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// withKubectl stubs the Exec seam to answer kubectl invocations via a handler
// keyed on the joined args; non-kubectl shell-outs error.
// It stubs THREE seams, not one: Exec, kubectlprobe's, and KubectlOut. This
// package reads the cluster three ways and the readiness lanes use the
// string-returning one — stubbing only Exec left deploymentRolledOut reading a
// real cluster, which showed up as "a clean 2/2 read must count as rolled out".
func withKubectl(t *testing.T, h func(args string) ([]byte, error)) {
	t.Helper()
	withExecOutput(t, func(name string, args ...string) ([]byte, error) {
		if name != "kubectl" {
			return nil, fmt.Errorf("unexpected command %q", name)
		}
		return h(strings.Join(args, " "))
	})
	origOut := caps.KubectlOut
	caps.KubectlOut = func(args ...string) (string, error) {
		b, err := h(strings.Join(args, " "))
		return string(b), err
	}
	t.Cleanup(func() { caps.KubectlOut = origOut })
}

// items wraps item JSON blobs into a kubectl list response.
func items(blobs ...string) []byte {
	return []byte(`{"items":[` + strings.Join(blobs, ",") + `]}`)
}

// errRetrofitNotFound stands in for kubectl's "NotFound" exit — a local copy of
// package main's sentinel, exactly as internal/objenc's errNotFound is. A one-line
// sentinel does not justify an export, and a test fixture cannot cross a package
// boundary.
var errRetrofitNotFound = errors.New("Error from server (NotFound)")

// reconcilerRuleCRD is the PrometheusRule under test, repo-relative.
//
// THE PATH CHANGED WITH THE PACKAGE. It was "../../../platform-apl/..." from
// cmd/llz and is the same depth from internal/assertobs, which is luck rather than
// design — a relative path in a test is a dependency on where the test file sits,
// and moving the file silently re-points it. Checked rather than assumed.
const reconcilerRuleCRD = "../../../platform-apl/components/llzReconciler/llz-reconciler/prometheusrule.yaml"
