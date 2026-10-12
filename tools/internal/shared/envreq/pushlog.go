// pushlog.go — what llz itself last sent, so drift can be a FACT rather than an
// inference from file mtimes.
//
// The mtime heuristic in drift.go is honest but blunt: mtime is per-FILE, so it
// can only order a whole file against a secret, and it cannot tell llz's own push
// from a rotation CronJob's. Both errors showed up the first time the check ran
// against a real instance — five secrets the operator had just pushed BY HAND
// were reported as "CI rotated these, your copy is stale", which is not merely
// noisy but points at the wrong fix.
//
// The missing fact is cheap to keep. When llz pushes a value it knows the value,
// so it records a SHA-256 of it and the time. Then:
//
//	local hash != recorded hash          → the local copy really did change  (BEHIND)
//	GitHub's updated_at > our push time  → somebody ELSE wrote it since      (AHEAD)
//	neither                              → in sync, and say nothing
//
// No secret material is stored — only a digest, which cannot be reversed into a
// PAT — and .llz/ is gitignored in full. A missing or unreadable log simply falls
// back to the mtime heuristic, so this is an accuracy upgrade, never a dependency.
package envreq

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"time"
)

// PushLogFile records what llz last pushed. Digests only, never values.
const PushLogFile = ".llz/.push-state.json"

// PushRecord is one credential's last known push by this llz.
type PushRecord struct {
	// SHA256 of the value as pushed. A digest, so the file is not a second copy
	// of the credentials — it cannot be turned back into one.
	SHA256   string    `json:"sha256"`
	PushedAt time.Time `json:"pushed_at"`
}

// Digest is the one-way fingerprint stored for a value.
func Digest(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

// LoadPushLog reads the record, yielding an empty map for any problem — a
// missing, truncated or hand-mangled log must degrade to the mtime heuristic
// rather than fail a command whose real job is elsewhere.
func LoadPushLog() map[string]PushRecord {
	out := map[string]PushRecord{}
	b, err := os.ReadFile(PushLogFile)
	if err != nil {
		return out
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return map[string]PushRecord{}
	}
	return out
}

// RecordPush merges the just-pushed values into the log.
//
// MERGES rather than replaces: a push carries only the secrets that run needed,
// and overwriting the file would erase what earlier pushes established about
// everything else — turning every untouched credential back into a guess.
func RecordPush(pushed map[string]string, at time.Time) error {
	log := LoadPushLog()
	for k, v := range pushed {
		log[k] = PushRecord{SHA256: Digest(v), PushedAt: at.UTC()}
	}
	b, err := json.MarshalIndent(log, "", "  ")
	if err != nil {
		return err
	}
	// 0600 to match the credential cache beside it, even though this holds only
	// digests: the SET of names llz manages is itself worth not publishing.
	return os.WriteFile(PushLogFile, append(b, '\n'), 0o600)
}
