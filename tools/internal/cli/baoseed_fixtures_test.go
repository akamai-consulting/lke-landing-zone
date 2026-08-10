package cli

// fixtures the moved tests need.

import (
	"errors"
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
