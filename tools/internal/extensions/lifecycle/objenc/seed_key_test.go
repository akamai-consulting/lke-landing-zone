package objenc

import (
	"encoding/base64"
	"strings"
	"testing"
)

// withSSECSeedStubs builds the Deps the seeding path is handed, and records what
// it writes. It RETURNS the Deps rather than swapping package-level seams — the
// custody pair is a parameter now, which is the whole point of declaring
// secret-custody: what holds the key is visible at the call site.
func withSSECSeedStubs(t *testing.T, read func(string, string) (string, KVVerdict)) (Deps, *map[string]string) {
	t.Helper()
	var wrote map[string]string
	prevGen := newSSECKeyMaterial
	newSSECKeyMaterial = func() ([]byte, error) { return []byte("0123456789abcdef0123456789abcdef"), nil }
	t.Cleanup(func() { newSSECKeyMaterial = prevGen })
	d := testDeps(t)
	d.KVGet = read
	d.KVPut = func(_ string, f map[string]string) error { wrote = f; return nil }
	return d, &wrote
}

// THE property this command exists for. An indeterminate read — sealed pod,
// rejected token, konnectivity drop — must NOT be read as "no key here". Seeding
// over a live SSE-C key does not rotate it: it orphans every object already
// written, unrecoverably, because Linode keeps no copy of the discarded key.
func TestSeedSSECKeyRefusesToWriteOnAnIndeterminateRead(t *testing.T) {
	d, wrote := withSSECSeedStubs(t, func(string, string) (string, KVVerdict) {
		return "", KVUnknown
	})
	err := seedSSECKeyInto(d)
	if err == nil {
		t.Fatal("an unknown read must fail closed, not generate a key")
	}
	if !strings.Contains(err.Error(), "REFUSING TO SEED") {
		t.Errorf("the error must say why it refused, got: %v", err)
	}
	if *wrote != nil {
		t.Fatal("NOTHING may be written when the current state is unknown")
	}
}

// An existing key is left alone: it is the live one and every object depends on it.
func TestSeedSSECKeyIsAdditiveOnly(t *testing.T) {
	d, wrote := withSSECSeedStubs(t, func(string, string) (string, KVVerdict) {
		return "an-existing-key", KVFound
	})
	if err := seedSSECKeyInto(d); err != nil {
		t.Fatalf("an existing key is a no-op, not an error: %v", err)
	}
	if *wrote != nil {
		t.Error("an existing key must never be overwritten")
	}
}

// Only a definite "absent" — OpenBao answering that the path is empty — authorizes
// generating a key, and it must be a 32-byte AES-256 key, base64-encoded.
func TestSeedSSECKeyGeneratesOnlyWhenDefinitelyAbsent(t *testing.T) {
	d, wrote := withSSECSeedStubs(t, func(string, string) (string, KVVerdict) {
		return "", KVAbsent
	})
	if err := seedSSECKeyInto(d); err != nil {
		t.Fatal(err)
	}
	if *wrote == nil {
		t.Fatal("a definitely-absent path must be seeded")
	}
	raw, err := base64.StdEncoding.DecodeString((*wrote)["key"])
	if err != nil {
		t.Fatalf("the seeded key must be base64: %v", err)
	}
	if len(raw) != ssecKeyRawBytes {
		t.Errorf("seeded a %d-byte key, want %d (SSE-C is AES-256)", len(raw), ssecKeyRawBytes)
	}
}

// The KV path is shared by the seed, the ExternalSecret and the OpenBao read
// policy. Pinning it here makes a rename break loudly in one place.
func TestSSECKVPathIsStable(t *testing.T) {
	if SSECKVPath != "secret/obj/ssec" {
		t.Errorf("SSECKVPath = %q — the ExternalSecret's remoteRef.key and the bao-configure "+
			"read policy must move with it", SSECKVPath)
	}
}
