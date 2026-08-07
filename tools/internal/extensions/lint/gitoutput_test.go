package lint

// gitoutput_test.go — followed gitOutput here when hooks.go moved. There are
// four copies of this four-line helper across the tree; this pins the one that
// matters, that `-C <dir>` reaches git rather than being silently dropped.

import "testing"

func TestGitOutputPassesDirFlag(t *testing.T) {
	var gotArgs []string
	withExecOutput(t, func(_ string, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte("ok\n"), nil
	})
	out, err := gitOutput("/work/dir", "rev-parse", "--show-toplevel")
	if err != nil || out != "ok" {
		t.Fatalf("gitOutput = (%q, %v), want (ok, nil)", out, err)
	}
	if len(gotArgs) < 2 || gotArgs[0] != "-C" || gotArgs[1] != "/work/dir" {
		t.Errorf("gitOutput did not pass `-C /work/dir`: %v", gotArgs)
	}
}
