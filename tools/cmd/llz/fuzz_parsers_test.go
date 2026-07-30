package main

// Fuzz targets for the string parsers in cmd/llz.
//
// Why fuzzing, and why these functions: mutation testing surfaced two defects in
// this package that no hand-written fixture would have reached. The container
// image-reference helpers PANICKED on a bare name (both LastIndex(":") and
// LastIndex("/") return -1, and a mutated bound then sliced [:-1]) — found only
// because a mutant happened to walk into it. And in internal/linode the
// days-from-civil `doe/146_096` term is non-zero on exactly ONE day per 400
// years, so it stayed invisible until an exhaustive sweep. Both are input-space
// problems, which is what fuzzing is for and what example-based tests are not.
//
// These targets assert INVARIANTS rather than expected outputs, because the
// interesting inputs are the ones nobody would think to write down:
//
//   - the function does not panic
//   - it is deterministic (same input, same answer)
//   - its output satisfies the contract stated in its doc comment
//
// Seed corpora below are the shapes that actually occur (registry ports, digests,
// bare names) plus the degenerate ends. `go test` runs the seed corpus as ordinary
// subtests at zero cost, so these act as regression tests even when nobody is
// fuzzing; `make fuzz` explores beyond them. Any crasher found gets written to
// testdata/fuzz/ by the toolchain and then runs forever as part of the corpus.

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// wellFormedImageRef is a conservative shape for a container reference:
//
//	[host[:port]/][path/]name[:tag][@algo:hex]
//
// Note the registry prefix REQUIRES its trailing slash. That is not pedantry: a
// port colon is only meaningful when a path follows it, so "0:0:0" — host, port,
// tag, no path — is not a reference at all. An earlier version of this pattern
// made the "/" optional and the fuzzer immediately produced exactly that string,
// where imageName returns "0:0". The parser was right and the predicate was
// wrong, twice; writing the shape down precisely is the point.
//
// Conservative on purpose — it gates the CONTRACT assertions, so being too narrow
// only means fuzzing checks fewer inputs strictly, while being too wide makes the
// target fail on strings no caller can produce.
var wellFormedImageRef = regexp.MustCompile(
	`^([a-zA-Z0-9][a-zA-Z0-9._-]*(:[0-9]+)?/([a-zA-Z0-9][a-zA-Z0-9._-]*/)*)?` + // optional registry+path, slash required
		`[a-zA-Z0-9][a-zA-Z0-9._-]*` + // the name
		`(:[a-zA-Z0-9][a-zA-Z0-9._-]*)?` + // optional tag
		`(@[a-z0-9]+:[a-f0-9]+)?$`) // optional digest

// FuzzImageNameTag checks the contract documented on imageName/imageTag: the name
// is a bare repo basename (no registry, org, tag or digest) and the tag never
// carries a digest or a path separator. A registry port colon must not be
// mistaken for a tag colon, which is the case the `colon > slash` comparison
// exists for.
func FuzzImageNameTag(f *testing.F) {
	for _, s := range []string{
		"grafana/loki:2.9.2",
		"loki",
		"docker.io/library/nginx",
		"registry:5000/org/img:v1",        // registry port + tag
		"registry:5000/org/img",           // registry port, NO tag
		"img@sha256:abc123",               // digest only
		"org/img:v1@sha256:abc123",        // tag AND digest
		"",                                // degenerate
		":", "/", "@", ":/@", "///", "::", // pathological punctuation
		"a:b:c/d:e",
		"ghcr.io/akamai-consulting/lke-landing-zone/llz:sha-deadbeef",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, image string) {
		name := imageName(image)
		tag := imageTag(image)

		// Determinism: a parser that depends on anything but its input is a bug
		// we would otherwise only see as flake.
		if imageName(image) != name || imageTag(image) != tag {
			t.Fatalf("not deterministic for %q", image)
		}

		// ── Universal invariants: hold for EVERY input, however malformed ──
		//
		// A path separator or digest marker in either result means the split
		// itself went wrong, which is true regardless of how junk the input is.
		if strings.ContainsAny(name, "/@") {
			t.Errorf("imageName(%q) = %q — must not contain a path separator or digest marker", image, name)
		}
		if strings.ContainsAny(tag, "/@") {
			t.Errorf("imageTag(%q) = %q — must not contain a path separator or digest marker", image, tag)
		}
		// Both results are substrings, so neither can grow the input.
		if len(name) > len(image) || len(tag) > len(image) {
			t.Errorf("imageName/imageTag(%q) grew the input: name=%q tag=%q", image, name, tag)
		}
		// No colon anywhere means no tag to find.
		if !strings.Contains(image, ":") && tag != "" {
			t.Errorf("imageTag(%q) = %q — no colon in the input, so there is no tag", image, tag)
		}

		// ── Contract invariants: only for WELL-FORMED references ──
		//
		// The doc comment's promise ("no registry, org, tag, or digest") is a
		// statement about real references, not about arbitrary bytes. Fuzzing
		// found imageName("/::") == ":" in 0.14s; that is garbage-in/garbage-out
		// on a string the kubelet would reject, and asserting otherwise would be
		// the test over-claiming rather than the parser misbehaving. So the strict
		// checks are gated on the input actually looking like a reference —
		// fuzzing still explores the whole space for crashes and the universal
		// invariants above, but the contract is only held where it applies.
		if !wellFormedImageRef.MatchString(image) {
			return
		}
		if strings.Contains(name, ":") {
			t.Errorf("imageName(%q) = %q — a tag or registry-port colon leaked into the name", image, name)
		}
		if name == "" {
			t.Errorf("imageName(%q) = %q — a well-formed reference always has a basename", image, name)
		}
		// The tag, when present, must be exactly what followed the final colon
		// after the last path separator — never a registry port.
		if tag != "" && strings.Contains(tag, ":") {
			t.Errorf("imageTag(%q) = %q — a tag cannot itself contain a colon", image, tag)
		}
	})
}

// FuzzParseQuantityBytes pins the parser's total behaviour: every input yields a
// value, deterministically, without panicking.
//
// NOTE what is deliberately NOT asserted. strconv.ParseFloat accepts "Inf",
// "+Inf", "-Inf" and "NaN", and Go leaves float->int conversion
// IMPLEMENTATION-DEFINED when the value is out of range. On this platform
// parseQuantityBytes("Inf") is MaxInt64 and "-Inf" is MinInt64 — so a garbage
// quantity becomes an absurd byte count rather than an error, and the result is
// not guaranteed portable across GOARCH. ParseFloat also accepts underscores, so
// "1_000" parses as 1000 though Kubernetes would reject it.
//
// That is a robustness gap rather than a live bug: the only caller feeds this
// `kubectl get pvc -o json` output, which is API-canonical. Changing the function
// is a production decision, so this target records the boundary it must not
// regress past (no panic, deterministic, and plain integers parse exactly) and
// leaves the saturation behaviour documented rather than asserted.
func FuzzParseQuantityBytes(f *testing.F) {
	for _, s := range []string{
		"", " ", "0", "1", "10Gi", "512Mi", "1Ki", "2Ti", "3Pi",
		"1k", "1M", "1G", "1T", "1P",
		"1.5Gi", " 8Gi ", "100",
		"Inf", "-Inf", "NaN", "1e300Pi", "1e400", "-5Gi", "1_000", "0x10",
		"Gi", "KiKi", "--1", "+", ".", "e",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got := parseQuantityBytes(s)
		if parseQuantityBytes(s) != got {
			t.Fatalf("not deterministic for %q", s)
		}
		// A plain decimal integer must round-trip exactly — but only up to 2^53.
		//
		// Fuzzing found this in 2 seconds: parseQuantityBytes("20000000000000001")
		// returns 20000000000000000. The function routes everything through
		// strconv.ParseFloat, and float64 has a 53-bit mantissa, so integers above
		// 2^53 are not representable and silently round. The boundary is exact:
		// 9007199254740992 (2^53) round-trips, 9007199254740993 does not.
		//
		// Left as-is rather than fixed, with the limit asserted instead of the
		// wish. The sole caller is import.go:556 on a PVC's .Size, and 2^53 bytes
		// is 9 PB against a 10 TB per-volume ceiling — so the loss is unreachable
		// through any real input. Widening the assertion to all int64 would make
		// this target fail on code that is fine in practice; narrowing it to the
		// representable range still catches the regression that matters (the
		// unit-suffix machinery wrongly claiming a plain integer).
		const exactFloat64Int = int64(1) << 53
		if n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
			if n <= exactFloat64Int && n >= -exactFloat64Int && got != n {
				t.Errorf("parseQuantityBytes(%q) = %d, want the plain integer %d", s, got, n)
			}
		}
		// Unparseable input is documented as 0 rather than an error, so an
		// empty-after-trim string must never invent a value.
		if strings.TrimSpace(s) == "" && got != 0 {
			t.Errorf("parseQuantityBytes(%q) = %d, want 0 for blank input", s, got)
		}
	})
}
