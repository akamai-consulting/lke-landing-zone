package ghsecret

// erroroutput_test.go — a failure that produced no output must still say what
// went wrong.
//
// THE DEFECT: Set formatted only the command's OUTPUT and dropped `err`. When gh
// is missing entirely, exec returns an error and NO output, so the message came
// out as "gh secret set X --env Y: " — a colon and nothing after it. That is what
// the e2e broad-pat lane printed, and it is why the real cause (the in-cluster
// rotator runs the distroless llz image, which has no gh binary) took a cluster
// round to find instead of a glance.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withEmptyPATH makes `gh` unresolvable, reproducing the in-cluster image.
func withEmptyPATH(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", filepath.Join(t.TempDir(), "empty"))
}

func TestSetNamesTheErrorWhenGhProducesNoOutput(t *testing.T) {
	withEmptyPATH(t)
	err := Set("LINODE_API_TOKEN", "infra-e2e", "v")
	if err == nil {
		t.Fatal("a missing gh must be an error")
	}
	msg := err.Error()
	if strings.HasSuffix(strings.TrimSpace(msg), ":") {
		t.Fatalf("the message ends at a colon with nothing after it — the failure is unactionable: %q", msg)
	}
	if !strings.Contains(msg, "executable file not found") && !strings.Contains(msg, "no such file") {
		t.Errorf("the message should carry exec's reason, got %q", msg)
	}
	// The label must survive too, or the reader cannot tell WHICH secret failed.
	if !strings.Contains(msg, "LINODE_API_TOKEN") || !strings.Contains(msg, "infra-e2e") {
		t.Errorf("the message must name the secret and env, got %q", msg)
	}
}

func TestDeleteNamesTheErrorWhenGhProducesNoOutput(t *testing.T) {
	withEmptyPATH(t)
	err := Delete("LINODE_API_TOKEN", "infra-e2e")
	if err == nil {
		t.Fatal("a missing gh must be an error")
	}
	if strings.HasSuffix(strings.TrimSpace(err.Error()), ":") {
		t.Fatalf("empty reason after the colon: %q", err)
	}
}

var _ = os.Getenv
