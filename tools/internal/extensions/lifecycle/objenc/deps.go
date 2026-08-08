package objenc

// deps.go — what SSE-C encryption is handed.
//
// NINTH EXTRACTION, AND THE FIRST TO REACH `seeded`. Everything here exists
// because Linode Object Storage implements exactly one server-side encryption mode
// — SSE-C, where the CLIENT holds the key — so the platform has to mint one, keep
// it, and hand it to every writer. That is custody in the literal sense, and it is
// why this is the extension that finally binds `transition:seeded[secret-custody]`.

// KVVerdict distinguishes "OpenBao answered: absent" from "OpenBao could not be
// asked", which is the distinction the whole seeding path turns on.
//
// A NARROW MIRROR of package main's baoReadVerdict, and the reason is worth
// stating: only a definite ABSENT may be treated as "not seeded yet". An unknown —
// OpenBao unreachable, token rejected — that got read as absent would mint a SECOND
// SSE-C key, and every object written under the first one becomes unreadable. The
// three-valued answer is load-bearing, so this package declares the three values it
// acts on rather than accepting a bool.
type KVVerdict int

const (
	KVUnknown KVVerdict = iota // could not ask — never treat as absent
	KVFound                    // a value came back
	KVAbsent                   // OpenBao answered: no such path or field
)

// Deps carries what this package cannot reach for itself.
type Deps struct {
	// KVGet reads one field of an OpenBao KV path. The verdict matters more than
	// the error — see KVVerdict.
	KVGet func(path, field string) (string, KVVerdict)

	// KVPut writes fields to an OpenBao KV path. THIS IS THE CUSTODY: the SSE-C
	// key is minted here and never leaves OpenBao except into the proxy that uses
	// it, which is exactly what `secret-custody` is meant to name.
	KVPut func(path string, fields map[string]string) error

	// KubectlOut reads the cluster — the Secrets that carry object-storage
	// credentials, and the pods that should be mounting the CA.
	KubectlOut func(args ...string) (string, error)

	// SecretField decodes one base64 field out of a kubectl-fetched Secret.
	SecretField func(raw []byte, field string) (string, error)

	// MaskGHALines emits ::add-mask:: for values that must not appear in a log.
	// Injected rather than copied because forgetting it is silent: the value still
	// works, and only a leaked log shows the mistake.
	MaskGHALines func(vals ...string)
}
