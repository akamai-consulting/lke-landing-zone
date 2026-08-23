package objenc

// c07_review_test.go — the gates for the C07 findings of the 2026-08-13 review.

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// ── the decode bound ─────────────────────────────────────────────────────────

// TestDecodeAWSChunkedStopsAtTheDeclaredLength.
//
// The caller gates on x-amz-decoded-content-length — the client's own STATEMENT
// of the size — and nothing held the DECODE to it. The equality check at the
// bottom fires only after every chunk has been buffered, so a request declaring
// 1 MiB and sending chunks summing to 64 MiB was rejected having already
// allocated the 64 MiB. With the raw body held alongside it for the restore path,
// peak footprint ran to several times the documented 32 MiB cap, and a handful of
// concurrent repairs OOMKilled a DaemonSet with a 512Mi limit — taking object
// storage down for every pod on the node.
func TestDecodeAWSChunkedStopsAtTheDeclaredLength(t *testing.T) {
	// Declares 16 bytes, sends 64 KiB in one chunk.
	payload := bytes.Repeat([]byte("A"), 64*1024)
	var body bytes.Buffer
	fmt.Fprintf(&body, "%x;chunk-signature=deadbeef\r\n", len(payload))
	body.Write(payload)
	body.WriteString("\r\n0;chunk-signature=deadbeef\r\n\r\n")

	_, err := decodeAWSChunked(bytes.NewReader(body.Bytes()), 16)
	if err == nil {
		t.Fatal("a body far larger than its declared length must be refused")
	}
	if !strings.Contains(err.Error(), "refusing to buffer past the bound") {
		t.Errorf("the refusal must happen at the CHUNK HEADER, before the bytes are copied — otherwise "+
			"the memory is already gone by the time we object. got: %v", err)
	}
}

// TestDecodeAWSChunkedStillDecodesAnHonestBody pins the exclusion: a body that
// matches its declaration must decode exactly as before.
func TestDecodeAWSChunkedStillDecodesAnHonestBody(t *testing.T) {
	payload := []byte("hello world")
	var body bytes.Buffer
	fmt.Fprintf(&body, "%x;chunk-signature=x\r\n", len(payload))
	body.Write(payload)
	body.WriteString("\r\n0;chunk-signature=x\r\n\r\n")

	got, err := decodeAWSChunked(bytes.NewReader(body.Bytes()), int64(len(payload)))
	if err != nil || string(got) != string(payload) {
		t.Fatalf("decode = (%q, %v), want the payload back", got, err)
	}
}

// TestDecodeLimitIsNeverUnbounded. An absent or nonsensical declaration is
// exactly the case where trusting the stream is worst.
func TestDecodeLimitIsNeverUnbounded(t *testing.T) {
	for _, expected := range []int64{-1, objProxyResignMaxBody + 1, 1 << 62} {
		if got := decodeLimit(expected); got != objProxyResignMaxBody {
			t.Errorf("decodeLimit(%d) = %d, want the repair cap %d", expected, got, objProxyResignMaxBody)
		}
	}
	if got := decodeLimit(1024); got != 1024 {
		t.Errorf("decodeLimit(1024) = %d, want the declared length", got)
	}
}

// ── the SSE-C key the trim ate ───────────────────────────────────────────────

// TestParseSSECKeyFileAcceptsARawKeyEndingInANewlineByte.
//
// bytes.TrimRight(raw, "\r\n") strips trailing 0x0A/0x0D. AES key bytes are
// uniform, so roughly 1 in 128 raw keys ends in one of those — trimmed to 31
// bytes, matched by neither branch, and rejected. Every obj-proxy pod on the node
// then fails to start, for a key that is perfectly valid.
func TestParseSSECKeyFileAcceptsARawKeyEndingInANewlineByte(t *testing.T) {
	for name, last := range map[string]byte{"LF": 0x0A, "CR": 0x0D} {
		t.Run(name, func(t *testing.T) {
			key := bytes.Repeat([]byte{0x41}, sseCustomerKeyRawBytes)
			key[sseCustomerKeyRawBytes-1] = last

			got, err := parseSSECKeyFile(key)
			if err != nil {
				t.Fatalf("a valid %d-byte raw key ending 0x%02X was rejected — that is a CrashLoop on "+
					"~1 in 128 clusters: %v", sseCustomerKeyRawBytes, last, err)
			}
			if !bytes.Equal(got, key) {
				t.Errorf("the key came back altered: %x vs %x", got, key)
			}
		})
	}
}

// TestParseSSECKeyFileStillAcceptsTheDocumentedForms pins the exclusions — the
// base64 in-cluster form and a raw key with a trailing newline both still work,
// and a genuinely wrong length is still refused.
func TestParseSSECKeyFileStillAcceptsTheDocumentedForms(t *testing.T) {
	raw := bytes.Repeat([]byte{0x42}, sseCustomerKeyRawBytes)
	b64 := []byte("QkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkI=\n") // base64 of 32 'B's
	if got, err := parseSSECKeyFile(b64); err != nil || !bytes.Equal(got, raw) {
		t.Errorf("the base64 form (what `llz ci seed-ssec-key` writes) must still decode: %v", err)
	}
	if got, err := parseSSECKeyFile(append(append([]byte{}, raw...), '\n')); err != nil || !bytes.Equal(got, raw) {
		t.Errorf("a raw key with a trailing newline must still parse: %v", err)
	}
	if _, err := parseSSECKeyFile([]byte("too short")); err == nil {
		t.Error("a wrong-length file must still be refused")
	}
}

// ── the DNS check that reported a verdict it had not reached ─────────────────

// TestDNSCheckDoesNotReportAnUnreadableConfigMapAsPlaintextTraffic.
//
// Folding `err != nil` into the absent branch reported an RBAC denial or an
// apiserver blip as "object-storage traffic is going DIRECT to Linode,
// unencrypted" — a specific, alarming claim about live traffic, made on the
// strength of not having looked. The remedy it printed was to APPLY the rewrite,
// which on a cluster that already has one is at best a no-op and at worst the
// ordering the Fix text itself warns against.
func TestDNSCheckDoesNotReportAnUnreadableConfigMapAsPlaintextTraffic(t *testing.T) {
	d := withObjEncKubectl(t, "", errors.New(`configmaps "coredns-custom" is forbidden`))
	f := checkEndpointResolvesToProxy(d, "us-ord-10.linodeobjects.com")
	if len(f) != 1 {
		t.Fatalf("an unreadable ConfigMap is still a finding — it just is not THAT finding: %+v", f)
	}
	if strings.Contains(f[0].Problem, "going DIRECT") {
		t.Error("an RBAC denial was reported as live unencrypted traffic")
	}
	if !strings.Contains(f[0].Problem, "UNKNOWN") {
		t.Errorf("the finding must say the answer is unknown, got: %s", f[0].Problem)
	}
	if !strings.Contains(f[0].Fix, "Do NOT apply the rewrite") {
		t.Errorf("the fix must not tell the operator to apply a rewrite on the strength of a failed read: %s", f[0].Fix)
	}
}

// ── the loader that had nothing to fall back to ──────────────────────────────

// TestCredsLoaderKeepsTheSeededCredentialOnAReadError.
//
// credsLoader was constructed with a zero `cur`, so its three "keep what works"
// error paths all returned an unusable credential on the FIRST call. One
// transient stat() failure therefore turned the #397 re-signing repair off for
// that request — silently, with every counter still at zero.
// It drives loadResignCreds — the REAL construction — rather than building a
// seeded loader by hand. A hand-built one proves the loader behaves; it does not
// prove the caller seeds it, and the first cut of this test did exactly that and
// stayed green while the seeding was reverted.
func TestCredsLoaderKeepsTheSeededCredentialOnAReadError(t *testing.T) {
	path := t.TempDir() + "/creds"
	if err := os.WriteFile(path, []byte("AWS_ACCESS_KEY_ID=AKIASEEDED\nAWS_SECRET_ACCESS_KEY=s3cr3t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	creds, fn, err := loadResignCreds(path)
	if err != nil || !creds.usable() {
		t.Fatalf("loadResignCreds = (%+v, %v)", creds, err)
	}

	// The first call the proxy makes, and the file is momentarily unreadable.
	prevStat := objProxyStat
	t.Cleanup(func() { objProxyStat = prevStat })
	objProxyStat = func(string) (os.FileInfo, error) { return nil, errors.New("transient: interrupted system call") }

	got := fn()
	if !got.usable() || got.AccessKeyID != "AKIASEEDED" {
		t.Errorf("a stat failure on the FIRST call must leave the credential just read in place, got "+
			"%+v — an unusable one silently disables the #397 re-signing repair for that request, with "+
			"every counter still at zero", got)
	}
	_ = time.Now
}
