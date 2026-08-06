package main

// kubectlstub_test.go — withKubectl and items stayed.
//
// They were defined in ci_health_test.go, which moved to internal/converge, but
// a dozen package main test files use them. Fixtures travel with the FILE like
// every other test; when the file's helpers have callers on both sides, the
// helper has to be copied rather than moved.

import (
	"fmt"
	"strings"
	"testing"
)

// withKubectl stubs the execOutput seam to answer kubectl invocations via a
// handler keyed on the joined args; non-kubectl shell-outs error. An unstubbed
// kubectl call returns an error, which the section helpers treat as "empty".
func withKubectl(t *testing.T, h func(args string) ([]byte, error)) {
	t.Helper()
	withExecOutput(t, func(name string, args ...string) ([]byte, error) {
		if name != "kubectl" {
			return nil, fmt.Errorf("unexpected command %q", name)
		}
		return h(strings.Join(args, " "))
	})
}

// items wraps item JSON blobs into a kubectl list response.
func items(blobs ...string) []byte {
	return []byte(`{"items":[` + strings.Join(blobs, ",") + `]}`)
}
