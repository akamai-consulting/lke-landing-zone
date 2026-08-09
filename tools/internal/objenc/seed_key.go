package objenc

// ci_seed_ssec_key.go implements `llz ci seed-ssec-key --region <env>` — the
// bootstrap seed for the obj-proxy's SSE-C encryption key.
//
// WHAT IT PROTECTS. `llz obj-proxy` (platform-apl/components/objProxy) injects
// SSE-C headers so Linode Object Storage holds Loki chunks and Harbor image layers
// encrypted. Linode implements no other server-side mode — SSE-S3 answers 400
// InvalidArgument and PutBucketEncryption answers 501 NotImplemented (measured;
// see the linode_object_storage_bucket entries in ci_at_rest_guard.go) — so this
// key is the ONLY thing standing between those buckets and plaintext.
//
// GENERATE-ONCE, NEVER ROTATE CASUALLY. This is the sharpest property of the whole
// design and the reason it gets a dedicated command rather than an entry in the
// generic bao-seed-all table:
//
//   - Linode DISCARDS SSE-C keys on receipt. An object written under a key nobody
//     holds is unrecoverable — by us and by Linode. Losing this is losing the
//     registry and the log store, not losing access to them.
//   - There is no server-side rekey. SSE-C keys are PER-OBJECT, so "rotation"
//     means rewriting every object under the new key (a copy carrying both the
//     copy-source and destination trios). Budget it against total bucket size.
//
// So the seed is strictly additive: an existing key is left ALONE. Overwriting one
// would not fail — it would silently orphan every object already written, and the
// damage would surface later as unreadable blobs, which is the worst possible time
// to learn about it. What makes re-runs safe is the CLASSIFIED read in
// seedSSECKeyInto: only OpenBao positively answering "absent" authorizes a write,
// and an unknown read refuses rather than guesses.
//
// GATED ON THE COMPONENT, like seed-broad-pat: a deployment that does not run the
// proxy has no use for the key, and a key that exists in clusters that do not need
// it is more copies of an unrecoverable secret than necessary.
//
// ESCROW IS PART OF THE JOB, NOT AN AFTERTHOUGHT. On a first-ever generation this
// prints the operator instruction to back the key up offline, exactly as
// bao-seed-seal-key does for OPENBAO_SEAL_KEY. That makes this the THIRD member of
// the "lose it and the data is gone" class alongside the seal key and
// TF_STATE_ENCRYPTION_PASSPHRASE — and, being newest, the one most likely to be
// missed in a DR rehearsal.

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
)

// objProxyComponent is the spec.components key that gates this seed.
const objProxyComponent = "objProxy"

// SSECKVPath is where the proxy's ExternalSecret reads the key from. Kept as a
// const so the ExternalSecret, the OpenBao read policy and this seed cannot drift.
const SSECKVPath = "secret/obj/ssec"

// ssecKeyRawBytes is AES-256, which is the only key size SSE-C accepts.
const ssecKeyRawBytes = 32

// The read seam is now Deps.KVGet. It exists so the refuse-to-write-on-unknown
// property below is unit-testable: that branch is the one that protects the whole
// object store, and it is unreachable in a test that has to talk to a real OpenBao.

// newSSECKeyMaterial is a seam for tests; production reads crypto/rand.
var newSSECKeyMaterial = func() ([]byte, error) {
	b := make([]byte, ssecKeyRawBytes)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("generate SSE-C key: %w", err)
	}
	return b, nil
}

// ssecSeedEnabled reports whether the objProxy component is on for region. A
// missing env is not enabled.
func ssecSeedEnabled(lz *clusterspec.LandingZone, region string) bool {
	e, ok := lz.Env(region)
	if !ok {
		return false
	}
	return clusterspec.ComponentEnabled(e.Components, objProxyComponent)
}

func RunSeedSSECKey(d Deps, region string) error {
	if region == "" {
		return fmt.Errorf("--region is required")
	}
	lz, err := clusterspec.LoadInstance(".")
	if err != nil {
		return err
	}
	if !ssecSeedEnabled(lz, region) {
		fmt.Printf("seed-ssec-key: spec.components.%s is not enabled for %q — nothing to seed.\n",
			objProxyComponent, region)
		return nil
	}
	return seedSSECKeyInto(d)
}

// seedSSECKeyInto is the read-classify-write core, split from the spec-loading
// wrapper so the branch that refuses to write on an indeterminate read — the one
// standing between a transient OpenBao blip and an unrecoverable object store — is
// unit-testable.
func seedSSECKeyInto(d Deps) error {
	// Read first, and CLASSIFY the read — this is the whole safety property, and it
	// is why bao_read.go's three-way verdict exists rather than a "" return.
	//
	// The old "" -swallowing spelling would report a sealed pod, a rejected token or
	// a konnectivity drop as "nothing there". Here that is not a stale-credential
	// bug, it is the unrecoverable one: this command would then generate a SECOND key
	// and overwrite the live one, and every object already written — every image
	// layer, every log chunk — would become permanently unreadable, with Linode
	// holding no copy of the discarded key to fall back on.
	//
	// So only KVAbsent, which is OpenBao ANSWERING that the path is empty,
	// authorizes a write. Unknown fails closed: the cost of failing is a re-run, and
	// the cost of guessing is the whole object store.
	existing, verdict := d.KVGet(SSECKVPath, "key")
	switch verdict {
	case KVFound:
		_ = existing
		fmt.Printf("seed-ssec-key: %s already holds a key — left untouched.\n", SSECKVPath)
		fmt.Println("  Replacing it would NOT rotate anything: SSE-C keys are per-object, so every")
		fmt.Println("  object already written would become unreadable. Rekeying is a bucket-wide")
		fmt.Println("  rewrite, not a reseed.")
		return nil
	case KVAbsent:
		// The only state that authorizes generating a key.
	default:
		return fmt.Errorf(
			"could not determine whether %s already holds a key (OpenBao gave no usable answer: "+
				"sealed, unreachable, or the token was rejected).\n"+
				"REFUSING TO SEED. If a key IS present and this wrote a new one, every object already "+
				"encrypted under the old key would be unrecoverable — Linode keeps no copy. Fix the "+
				"OpenBao read path and re-run; a re-run is free, a wrong guess is not",
			SSECKVPath)
	}

	raw, err := newSSECKeyMaterial()
	if err != nil {
		return err
	}
	b64 := base64.StdEncoding.EncodeToString(raw)

	// The path is spelled as a LITERAL here, not as SSECKVPath, because
	// ci_extsecret_paths.go's seed detector matches the put call on the SOURCE TEXT
	// and a const reference is invisible to it. An undetected seed reads as
	// "nothing seeds this path", which is exactly the gate that would otherwise
	// ship a proxy whose key never arrives. TestSSECKVPathIsStable pins the const
	// to the same value so the two cannot drift silently.
	//
	// (Deliberately no example of the matched form in this comment: the detector
	// reads comments too, and a sample call here registers as a seeded path.)
	if err := d.KVPut("secret/obj/ssec", map[string]string{
		"key": b64,
	}); err != nil {
		return fmt.Errorf("seed %s: %w", SSECKVPath, err)
	}

	d.MaskGHALines(b64)
	fmt.Printf("seed-ssec-key: generated a new %d-byte SSE-C key and seeded %s.\n", ssecKeyRawBytes, SSECKVPath)
	fmt.Println()
	fmt.Println("  ┌─ BACK THIS UP OFFLINE, NOW ─────────────────────────────────────────────┐")
	fmt.Println("  │ Linode DISCARDS SSE-C keys on receipt. If this value is lost, every      │")
	fmt.Println("  │ object written through obj-proxy — every Harbor image layer and every    │")
	fmt.Println("  │ Loki chunk — is unrecoverable, by us and by Linode alike.                │")
	fmt.Println("  │                                                                         │")
	fmt.Println("  │ Escrow it exactly as you escrow OPENBAO_SEAL_KEY and                     │")
	fmt.Println("  │ TF_STATE_ENCRYPTION_PASSPHRASE. It is the newest member of that class    │")
	fmt.Println("  │ and the one a DR rehearsal is most likely to forget.                     │")
	fmt.Println("  └─────────────────────────────────────────────────────────────────────────┘")
	return nil
}
