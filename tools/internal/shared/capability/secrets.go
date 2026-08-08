package capability

// secrets.go — the handles a `secret-read` or `secret-custody` binding receives.
//
// SECOND CAPABILITY, and it is the one the grant vocabulary most needed. Sixteen
// bindings declare `secret-custody`, eleven declare `secret-read`, and until now
// both were review metadata: a binding claiming read-only was handed
// `baoread.Exec` and could `kv put` with a root token exactly as easily as the
// seeder next to it.
//
// THE SURFACE IS NARROWER THAN THE GRANT COUNT, the same way the kubectl census
// found. Measured across internal/extensions and internal/verbs before any of this
// was designed: 47 bao call sites, of which 21 already go through the two
// STRUCTURED helpers (KVGetFieldOK 9, KVPut 12) and 27 are raw argv. Of the raw
// ones the literal first words are `operator` 9, `status` 5, `token` 4 — the
// privileged quorum flows and the liveness probes, not day-to-day secret traffic.
// So the handle covers the structured majority and the raw minority stays raw and
// counted, which is the same split kubectl's Writer landed on.
//
// ────────────────────────────────────────────────────────────────────────────
// WHAT MAKES THIS HANDLE DIFFERENT FROM THE CLUSTER ONE, and it is the whole
// design: A DENIED READ MUST NOT LOOK LIKE AN ABSENT ONE.
//
// `baoread.Verdict` exists because a seeder that cannot READ a path must not
// overwrite it. `Absent` means "bao answered: nothing there" and permits a write;
// `Unknown` means "nothing answered" and forbids one. That distinction is the
// reason the seeders are safe, and it is documented as the failure it prevents: a
// seeder treating an unreachable OpenBao as an empty one, overwriting a live
// credential.
//
// A capability layer is a new way to fail to read. If `Denied().Get()` returned
// `Absent` — the obvious zero value, and what a bool-returning API would have
// forced — then handing a binding LESS permission would make a seeder MORE likely
// to overwrite. The capability layer would have introduced the exact bug baoread
// was built to prevent, and it would have done it silently, in the direction of
// data loss. So `deniedSecrets.Get` returns `Unknown`, and a test pins it.
// ────────────────────────────────────────────────────────────────────────────

import (
	"errors"
	"fmt"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/baoread"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

// Secrets is the handle a `secret-read` binding receives — and a `secret-custody`
// one too, because custody always reads back what it placed.
type Secrets interface {
	// Get reads one field of a KV path and reports WHAT IT LEARNED, not merely
	// whether it worked. Callers must branch on the verdict: only `Absent`
	// justifies a write.
	Get(path, field string) (string, baoread.Verdict)
	// Permits reports whether this handle would allow a read, without doing one,
	// so a caller can fail early with its own message.
	Permits() error
}

// Custodian is what `secret-custody` adds: placing material, not just reading it.
type Custodian interface {
	// Put writes fields to a KV path.
	Put(path string, fields map[string]string) error
	// PermitsCustody reports whether this handle would allow a write.
	PermitsCustody() error
}

type secrets struct {
	get func(path, field string) (string, baoread.Verdict)
}

func (s secrets) Get(path, field string) (string, baoread.Verdict) { return s.get(path, field) }
func (s secrets) Permits() error                                   { return nil }

type custodian struct {
	put func(path string, fields map[string]string) error
}

func (c custodian) Put(path string, fields map[string]string) error { return c.put(path, fields) }
func (c custodian) PermitsCustody() error                           { return nil }

// ErrNoSecretRead and ErrNoSecretCustody are the refusals. They are distinct
// values rather than one message so a caller can tell "I may not look" from "I may
// not place", which are different bugs in a seeder.
var (
	ErrNoSecretRead    = errors.New("this binding declares neither secret-read nor secret-custody, so it may not read OpenBao")
	ErrNoSecretCustody = errors.New("this binding does not declare secret-custody, so it may not place secret material")
)

type deniedSecrets struct{}

// Get REFUSES AS UNKNOWN, NEVER AS ABSENT. See the header: returning Absent here
// would tell a seeder the path is free and invite it to overwrite a live
// credential, which would make a narrower grant more dangerous than a wider one.
func (deniedSecrets) Get(string, string) (string, baoread.Verdict) {
	return "", baoread.Unknown
}
func (deniedSecrets) Permits() error { return ErrNoSecretRead }

type deniedCustodian struct{}

func (deniedCustodian) Put(path string, _ map[string]string) error {
	return fmt.Errorf("refusing to write %s: %w", path, ErrNoSecretCustody)
}
func (deniedCustodian) PermitsCustody() error { return ErrNoSecretCustody }

// DeniedSecrets and DeniedCustodian are the refusing handles, exported so a caller
// building its own Deps can default to them rather than to nil. Same reason
// teardown's Confirm had to default to refusing: a zero value that does nothing is
// a zero value that panics or, worse, succeeds.
func DeniedSecrets() Secrets     { return deniedSecrets{} }
func DeniedCustodian() Custodian { return deniedCustodian{} }

// secretHandles builds the pair a binding's grants entitle it to.
//
// `secret-custody` IMPLIES `secret-read`, for the reason cluster-write implies
// cluster-read: every path that places material reads it back to verify, and
// forcing both onto the grant line would make it noisier without making it more
// informative. The converse does not hold — that is the whole point.
func secretHandles(b extension.Binding) (Secrets, Custodian) {
	var read, custody bool
	for _, g := range b.Grants {
		switch g {
		case extension.SecretRead:
			read = true
		case extension.SecretCustody:
			custody = true
		}
	}

	var c Custodian = deniedCustodian{}
	if custody {
		c = custodian{put: baoread.KVPut}
	}
	if !read && !custody {
		return deniedSecrets{}, c
	}
	return secrets{get: baoread.KVGetFieldOK}, c
}
