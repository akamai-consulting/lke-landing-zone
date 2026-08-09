package credrotate

// cobra_helpers_test.go — helper for the test that came with a moved command.
// A local copy; internal/database carries the same one.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/baoread"
)

func withGHASummaryFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "summary")
	t.Setenv("GITHUB_STEP_SUMMARY", p)
	return p
}

func writeTFVars(t *testing.T, dir, sub, region, content string) {
	t.Helper()
	p := filepath.Join(dir, "terraform-iac-bootstrap", sub)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, region+".tfvars"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// insecureClient trusts the httptest server's self-signed certificate. The stub
// speaks TLS because the real OpenBao does, and a client that refused it would
// make every forward test fail for a reason unrelated to what it checks.
func stubBaoSeedKV(t *testing.T, presentField, presentValue string) *[][]string {
	t.Helper()
	var puts [][]string
	prev := baoread.ExecFn
	baoread.ExecFn = func(_, _, _ string, args ...string) (string, string, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(joined, "kv get"):
			if presentField != "" && strings.Contains(joined, "-field="+presentField) {
				return presentValue + "\n", "", nil
			}
			// bao's own words for an absent path. A bare error with no stderr
			// would now mean "the read never got an answer" — which fails the
			// seed closed instead of overwriting a possibly-live credential.
			return "", "No value found at " + lastArg(args), errors.New("exit 2")
		case strings.HasPrefix(joined, "kv put"):
			puts = append(puts, args)
			return "", "", nil
		}
		return "", "unexpected: " + joined, errors.New("unexpected")
	}
	t.Cleanup(func() { baoread.ExecFn = prev })
	return &puts
}

func lastArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[len(args)-1]
}

// withBaoExec swaps the RESILIENT entry point — the one every caller in this
// package reaches for. Stubbing baoread.ExecRaw instead would leave the retry
// wrapper live and silently multiply each stubbed call by the backoff count.
func withBaoExec(t *testing.T, fn func(pod, token, stdin string, args ...string) (string, string, error)) {
	t.Helper()
	orig := baoread.ExecFn
	baoread.ExecFn = fn
	t.Cleanup(func() { baoread.ExecFn = orig })
}
