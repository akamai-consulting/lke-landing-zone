package objenc

// c07_review_test.go — the gates for the C07 findings of the 2026-08-13 review.

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
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

// TestAnAbsentConfigMapIsStillAMissingRewrite.
//
// The other half, and the one the first cut of the fix broke.
// kube-system/coredns-custom is an OPTIONAL CoreDNS extension point, and this
// repo's own coredns-rewrite.yaml records that no LLZ-managed cluster ships one —
// so on a cluster that never applied objProxy the get exits 1 with NotFound, not
// with an empty document. Treating every error as "could not read" told the one
// cluster genuinely writing plaintext that the answer was UNKNOWN and that it
// should NOT apply the rewrite: the check steered its only real target away from
// its own fix. kubectlprobe.ClassifyErr is the seam that already separates
// "asked and answered: not there" from "no answer".
func TestAnAbsentConfigMapIsStillAMissingRewrite(t *testing.T) {
	for _, stderr := range []string{
		`Error from server (NotFound): configmaps "coredns-custom" not found`,
		`error: the server doesn't have a resource type "configmap"`,
	} {
		d := withObjEncKubectl(t, "", errors.New(stderr))
		f := checkEndpointResolvesToProxy(d, "us-ord-10.linodeobjects.com")
		if len(f) != 1 {
			t.Fatalf("%q: want exactly one finding, got %+v", stderr, f)
		}
		if !strings.Contains(f[0].Problem, "going DIRECT") {
			t.Errorf("%q: a cluster with no coredns-custom at all is a cluster with no rewrite — "+
				"reporting UNKNOWN steers the one cluster actually writing plaintext away from the "+
				"fix. Got: %s", stderr, f[0].Problem)
		}
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
// prove the CALLER seeds it, and stays green while the seeding is reverted.
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
}

// TestTheChunkBoundSurvivesAnOverflowingHeader.
//
// `int64(out.Len()) + n > lim` OVERFLOWS. A chunk header of 0x7FFFFFFFFFFFFC17
// after a couple of kilobytes wraps the sum NEGATIVE, so the guard passes and
// io.CopyN buffers everything the client is willing to send — the 64 MiB worst
// case the bound was added to remove, reached from one crafted header. The
// subtraction form cannot wrap: lim is never negative and out.Len() never
// exceeds it, so lim-out.Len() stays in range.
func TestTheChunkBoundSurvivesAnOverflowingHeader(t *testing.T) {
	var body bytes.Buffer
	first := bytes.Repeat([]byte("a"), 2000)
	fmt.Fprintf(&body, "%x\r\n", len(first))
	body.Write(first)
	body.WriteString("\r\n")
	// A length that makes out.Len()+n overflow int64.
	fmt.Fprintf(&body, "%x\r\n", int64(0x7FFFFFFFFFFFFC17))
	body.Write(bytes.Repeat([]byte("b"), 5<<20))

	_, err := decodeAWSChunked(bytes.NewReader(body.Bytes()), 3000)
	if err == nil {
		t.Fatal("a chunk header that overflows the bound check must be refused")
	}
	// The distinguishing symptom: with the overflow, CopyN is entered and fails
	// on EOF having buffered the remainder. Rejected at the bound, the error
	// names the bound instead.
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("the overflow let the request past the bound and into io.CopyN — it should have been "+
			"refused by the limit check, got: %v", err)
	}
}

// TestANegativeChunkLengthIsRefused. strconv.ParseInt accepts a leading '-' in
// base 16, and io.CopyN with a negative n is a SILENT NO-OP: the chunk
// contributes nothing, out.Len() never advances, and the loop carries on reading
// framing that is already corrupt.
func TestANegativeChunkLengthIsRefused(t *testing.T) {
	body := "-10\r\n" + strings.Repeat("a", 16) + "\r\n0\r\n\r\n"
	_, err := decodeAWSChunked(strings.NewReader(body), 16)
	if err == nil {
		t.Fatal("a negative chunk length must be refused, not passed to io.CopyN as a no-op")
	}
	// The message matters: unrejected, CopyN is a no-op and the loop reads the
	// chunk DATA as the next header, so the failure surfaces as a bogus "not hex"
	// complaint about payload bytes — pointing the reader at the wrong end of the
	// request entirely.
	if !strings.Contains(err.Error(), "negative") {
		t.Errorf("the negative length must be named as such, got: %v", err)
	}
}

// TestTheDecodeDoesNotAllocateTheDECLAREDLength.
//
// `out.Grow(int(expected))` pre-allocated the CLIENT'S DECLARED size. expected is
// read off a header and resignForUpstream reads the raw body before calling this,
// so it is fully decoupled from the bytes actually sent: a 23-byte request
// declaring 32 MiB allocated 32 MiB before parsing its first chunk. About sixteen
// concurrent 200-byte requests then OOMKill the 512Mi DaemonSet — the exact
// failure the bound exists to prevent, at a millionth of the traffic.
func TestTheDecodeDoesNotAllocateTheDECLAREDLength(t *testing.T) {
	const declared = 32 << 20
	body := "b\r\nhello world\r\n0\r\n\r\n" // 11 bytes, declaring 32 MiB

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, _ = decodeAWSChunked(strings.NewReader(body), declared)
	runtime.ReadMemStats(&after)

	if grew := after.TotalAlloc - before.TotalAlloc; grew > declared/4 {
		t.Errorf("decoding a 23-byte body that DECLARED %d bytes allocated %d — the declaration is the "+
			"client's, not a measurement, and sizing the buffer from it hands any caller a %d-byte "+
			"allocation for free", declared, grew, declared)
	}
}
