package cli

// InClusterToken arrived here at 0% covered, from internal/extensions/credrotate,
// and it dropped this package through its floor. The resolution ORDER is the part
// with a decision in it: env beats file, and a missing file is "" rather than an
// error — callers treat "" as "not synced yet" and no-op, so turning a missing
// mount into a hard failure would wedge the very bootstrap it exists to survive.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInClusterToken(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "token")
	// A trailing newline is what `kubectl create secret --from-file` and a mounted
	// Secret both produce, so not trimming would send "tok\n" as a bearer token.
	if err := os.WriteFile(present, []byte("  from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "nope")

	for _, tc := range []struct {
		name, env, file, want string
	}{
		{"env wins over file", "from-env", present, "from-env"},
		{"falls back to file", "", present, "from-file"},
		{"empty env is not a value", "", missing, ""},
		{"no env, no file", "", missing, ""},
		{"env alone", "from-env", missing, "from-env"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const key = "LLZ_TEST_IN_CLUSTER_TOKEN"
			if tc.env != "" {
				t.Setenv(key, tc.env)
			} else {
				t.Setenv(key, "")
			}
			if got := InClusterToken(key, tc.file); got != tc.want {
				t.Errorf("InClusterToken = %q, want %q", got, tc.want)
			}
		})
	}
}
