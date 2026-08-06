package tokeninv

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

// testDeps returns Deps whose implementations DO THE WORK.
//
// Summary appends to the real GITHUB_OUTPUT / GITHUB_STEP_SUMMARY files, because
// the rotation plan's entire output IS those files — a fixture that discarded
// them would leave every routing assertion running against nothing. Two earlier
// extractions shipped exactly that bug (teardown's Summary, objenc's
// SecretField), and both times the tests passed while asserting on emptiness.
func testDeps() Deps {
	return Deps{
		CloudToken: func() (string, error) { return os.Getenv("LINODE_API_TOKEN"), nil },
		Summary:    realAppend,
	}
}

func realAppend(envVar string, lines ...string) error {
	path := os.Getenv(envVar)
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(strings.Join(lines, "\n") + "\n")
	return err
}

func mustReadFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	return capture(t, &os.Stdout, fn)
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	return capture(t, &os.Stderr, fn)
}

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
	// Restore BEFORE reading: the copy goroutine finishes only on EOF, and EOF
	// arrives only once the write end is closed.
	*target = orig
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}
