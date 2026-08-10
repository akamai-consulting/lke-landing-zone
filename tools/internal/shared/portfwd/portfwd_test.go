package portfwd

// The parser test followed the parser. It drives readForwardPort, which is
// unexported -- so once the function moved, this test could not stay behind even
// if someone wanted it to. That is the useful property of an unexported seam: it
// makes a test that is filed under the wrong owner fail to compile rather than
// quietly attribute coverage to a package that no longer contains the code.

import (
	"io"
	"strings"
	"testing"
	"time"
)

func TestReadForwardPort(t *testing.T) {
	out := "Forwarding from 127.0.0.1:54321 -> 9090\nForwarding from [::1]:54321 -> 9090\n"
	got, err := readForwardPort(strings.NewReader(out))
	if err != nil || got != "54321" {
		t.Fatalf("readForwardPort = %q, %v; want 54321", got, err)
	}
	if _, err := readForwardPort(strings.NewReader("no port line here\n")); err == nil {
		t.Error("readForwardPort should error when no Forwarding line is present")
	}
}

// The timeout path is the reason this wrapper exists at all: kubectl may never
// announce a port (RBAC refusal, pod not ready), and the reader goroutine only
// unblocks when the caller kills the process and closes stdout. Without the
// timeout the caller waits forever with no output to explain why.
func TestReadForwardPortTimeout(t *testing.T) {
	// A reader that never yields a port line and never EOFs.
	blocked, _ := io.Pipe()
	got, err := ReadForwardPortTimeout(blocked, 20*time.Millisecond)
	if err == nil {
		t.Fatalf("want a timeout error, got port %q", got)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error %q should say it timed out — the caller has no other signal", err)
	}

	ok, err := ReadForwardPortTimeout(strings.NewReader("Forwarding from 127.0.0.1:54321 -> 8200\n"), time.Second)
	if err != nil || ok != "54321" {
		t.Errorf("ReadForwardPortTimeout = %q, %v; want 54321", ok, err)
	}
}
