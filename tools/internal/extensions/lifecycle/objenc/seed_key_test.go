package objenc

import (
	"encoding/base64"
	"os"
	"path/filepath"
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
	if VeleroSSECKVPath != "secret/obj/velero-ssec" {
		t.Errorf("VeleroSSECKVPath = %q — the velero-ssec-key ExternalSecret and the bao-configure "+
			"read policy must move with it", VeleroSSECKVPath)
	}
}

// Each SSE-C key is seeded only where its own component is enabled, at its own
// path, and the two keys are independent: enabling Velero must not mint the
// objProxy key (more copies of an unrecoverable secret than needed) or the reverse.
// The velero ExternalSecret decodes the value base64 → 32 raw bytes, which is what
// Velero's AWS plugin demands of customerKeyEncryptionSecret.
func TestSeedSSECKeySeedsEachKeyOnlyForItsComponent(t *testing.T) {
	for _, tc := range []struct {
		objProxy, velero bool
		want             []string
	}{
		{false, false, nil},
		{false, true, []string{VeleroSSECKVPath}},
		{true, false, []string{SSECKVPath}},
		{true, true, []string{SSECKVPath, VeleroSSECKVPath}},
	} {
		dir := t.TempDir()
		t.Chdir(dir)
		lz := "apiVersion: llz.akamai-consulting.io/v1alpha1\nkind: LandingZone\nmetadata:\n  name: acme\nspec:\n  instance:\n    repo: o/acme\n"
		env := "apiVersion: llz.akamai-consulting.io/v1alpha1\nkind: ClusterDefinition\nmetadata:\n  name: prod\nspec:\n  cluster:\n    region: us-ord\n  components:\n" +
			"    objProxy: {enabled: " + boolStr(tc.objProxy) + "}\n    velero: {enabled: " + boolStr(tc.velero) + "}\n"
		if err := os.WriteFile(filepath.Join(dir, "landingzone.yaml"), []byte(lz), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(dir, "environments"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "environments", "prod.yaml"), []byte(env), 0o644); err != nil {
			t.Fatal(err)
		}
		d, _ := withSSECSeedStubs(t, func(string, string) (string, KVVerdict) { return "", KVAbsent })
		var paths []string
		d.KVPut = func(p string, f map[string]string) error {
			raw, err := base64.StdEncoding.DecodeString(f["key"])
			if err != nil || len(raw) != ssecKeyRawBytes {
				t.Errorf("%s: seeded %q, want base64 of %d bytes", p, f["key"], ssecKeyRawBytes)
			}
			paths = append(paths, p)
			return nil
		}
		if err := RunSeedSSECKey(d, "prod"); err != nil {
			t.Fatal(err)
		}
		if strings.Join(paths, ",") != strings.Join(tc.want, ",") {
			t.Errorf("objProxy=%v velero=%v seeded %v, want %v", tc.objProxy, tc.velero, paths, tc.want)
		}
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
