package baosnap

// archive.go — proving a snapshot is intact WITHOUT being able to read it.
//
// A Raft snapshot from `GET /v1/sys/storage/raft/snapshot` is a gzipped tar:
// meta.json (the Raft index/term it was taken at), state.bin (the FSM — every
// storage entry, each value barrier-encrypted), SHA256SUMS over those two, and —
// from OpenBao/Vault — SHA256SUMS.sealed, the same sums encrypted under the
// barrier so a restore can tell a snapshot of THIS cluster from a foreign one.
//
// Verify checks what can be checked with no key material: the archive parses, the
// two required members are there, and every file SHA256SUMS lists hashes to what
// it says. It CANNOT check that state.bin decrypts — that needs the barrier, which
// is the point of it. So "verified" here means "this is the archive OpenBao wrote,
// byte-for-byte", which is what both the producer (before upload) and a restore
// (before handing it to OpenBao) need to know; whether OpenBao accepts it is the
// restore drill's question, not this function's.
//
// FAIL CLOSED ON VACUITY. An archive with no SHA256SUMS, or whose SHA256SUMS lists
// nothing we can check, is an error rather than a pass — a verifier that verified
// nothing is the shape the e2e doctrine refuses.

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// MaxSnapshotBytes bounds what the producer will buffer and what a restore will
// download. The store this platform keeps is small (KV secrets, policies, roles,
// leases: kilobytes to low megabytes), so 128 MiB is two orders of magnitude of
// headroom — and a bound, because the CronJob holds the snapshot in memory to hash
// it for SigV4, and an unbounded read is an OOMKill that looks like a flaky Job.
// The CronJob's memory limit (384Mi) is sized against this value; raise both
// together.
const MaxSnapshotBytes = 128 << 20

const (
	memberMeta    = "meta.json"
	memberState   = "state.bin"
	memberSums    = "SHA256SUMS"
	memberSumsSea = "SHA256SUMS.sealed"
)

// Manifest is what Verify learned.
type Manifest struct {
	Members []string
	// Index and Term are the Raft position the snapshot was taken at (meta.json).
	// A restore prints them so an operator can see two snapshots are the same
	// state without comparing bytes.
	Index, Term uint64
	// Sealed reports whether the archive carries SHA256SUMS.sealed — whether
	// OpenBao will be able to tell, at restore time, which cluster it came from.
	Sealed     bool
	StateBytes int64
}

// ErrNotSnapshot is the sentinel for "these bytes are not a Raft snapshot at all"
// — distinct from a corrupt one, because the remedies differ (wrong object vs
// damaged object).
var ErrNotSnapshot = errors.New("not a Raft snapshot archive")

// Verify proves data is an intact Raft snapshot archive. See the file comment for
// what that does and does not mean.
func Verify(data []byte) (Manifest, error) {
	var m Manifest
	if len(data) == 0 {
		return m, fmt.Errorf("%w: zero bytes", ErrNotSnapshot)
	}
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return m, fmt.Errorf("%w: not gzip (%v)", ErrNotSnapshot, err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)

	sums := map[string]string{}
	got := map[string]string{}
	var metaRaw []byte
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return m, fmt.Errorf("%w: tar stream: %v", ErrNotSnapshot, err)
		}
		name := strings.TrimPrefix(h.Name, "./")
		m.Members = append(m.Members, name)
		hasher := sha256.New()
		var buf bytes.Buffer
		w := io.Writer(hasher)
		if name == memberSums || name == memberMeta {
			w = io.MultiWriter(hasher, &buf)
		}
		n, err := io.Copy(w, io.LimitReader(tr, MaxSnapshotBytes+1))
		if err != nil {
			return m, fmt.Errorf("snapshot member %s is truncated: %v", name, err)
		}
		if n > MaxSnapshotBytes {
			return m, fmt.Errorf("snapshot member %s exceeds %d bytes", name, MaxSnapshotBytes)
		}
		got[name] = hex.EncodeToString(hasher.Sum(nil))
		switch name {
		case memberState:
			m.StateBytes = n
		case memberMeta:
			metaRaw = buf.Bytes()
		case memberSums:
			if sums, err = parseSums(buf.Bytes()); err != nil {
				return m, err
			}
		case memberSumsSea:
			m.Sealed = true
		}
	}

	for _, req := range []string{memberMeta, memberState, memberSums} {
		if _, ok := got[req]; !ok {
			return m, fmt.Errorf("%w: no %s member (have %v)", ErrNotSnapshot, req, m.Members)
		}
	}
	// The two members a restore depends on must be COVERED by the sums, or an
	// archive whose SHA256SUMS lists only itself-irrelevant files would pass.
	for _, req := range []string{memberMeta, memberState} {
		if _, ok := sums[req]; !ok {
			return m, fmt.Errorf("SHA256SUMS does not cover %s — the archive cannot be checked", req)
		}
	}
	for name, want := range sums {
		have, ok := got[name]
		if !ok {
			return m, fmt.Errorf("SHA256SUMS lists %s but the archive has no such member", name)
		}
		if !strings.EqualFold(have, want) {
			return m, fmt.Errorf("snapshot member %s is corrupt: sha256 %s, SHA256SUMS says %s", name, have, want)
		}
	}
	if m.StateBytes == 0 {
		return m, fmt.Errorf("snapshot state.bin is empty — OpenBao never writes an empty FSM")
	}

	var meta struct {
		Index uint64 `json:"Index"`
		Term  uint64 `json:"Term"`
	}
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		return m, fmt.Errorf("snapshot meta.json is not JSON: %v", err)
	}
	m.Index, m.Term = meta.Index, meta.Term
	return m, nil
}

// parseSums reads `<hex>  <name>` lines (the sha256sum format the snapshot writer
// emits). Blank lines are ignored; anything else malformed is an error rather
// than a skipped line, so a mangled file cannot shrink to "checks nothing".
func parseSums(b []byte) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 2 || len(f[0]) != sha256.Size*2 {
			return nil, fmt.Errorf("SHA256SUMS line %q is not `<sha256>  <file>`", line)
		}
		if _, err := hex.DecodeString(f[0]); err != nil {
			return nil, fmt.Errorf("SHA256SUMS line %q: %v", line, err)
		}
		out[strings.TrimPrefix(f[1], "*")] = f[0]
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("SHA256SUMS is empty — the archive cannot be checked")
	}
	return out, nil
}

// SHA256 is the hex digest of b — the producer records it in its audit line and
// compares it against what it reads back from the bucket.
func SHA256(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
