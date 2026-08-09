package lint

// gitoutput_test.go — followed gitcmd.Output here when hooks.go moved. There are
// four copies of this four-line helper across the tree; this pins the one that
// matters, that `-C <dir>` reaches git rather than being silently dropped.

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/gitcmd"
)

func TestGitOutputPassesDirFlag(t *testing.T) {
	var gotArgs []string
	withExecOutput(t, func(_ string, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte("ok\n"), nil
	})
	out, err := gitcmd.Output("/work/dir", "rev-parse", "--show-toplevel")
	if err != nil || out != "ok" {
		t.Fatalf("gitcmd.Output = (%q, %v), want (ok, nil)", out, err)
	}
	if len(gotArgs) < 2 || gotArgs[0] != "-C" || gotArgs[1] != "/work/dir" {
		t.Errorf("gitcmd.Output did not pass `-C /work/dir`: %v", gotArgs)
	}
}
