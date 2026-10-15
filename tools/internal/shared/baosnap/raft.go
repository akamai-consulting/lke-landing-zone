package baosnap

// raft.go — taking and restoring a snapshot through OpenBao's own API.
//
// The API, not the in-pod `bao operator raft snapshot` CLI, for both directions:
// the producer runs in its own namespace with no exec rights into OpenBao's pods
// (and must not have them), and a restore needs to stream a file INTO the active
// node, which `kubectl exec` can only do through a shell and a writable /tmp that
// the OpenBao image does not promise.
//
//	GET  /v1/sys/storage/raft/snapshot        save   (policy: read)
//	POST /v1/sys/storage/raft/snapshot        restore, same cluster's keyring
//	POST /v1/sys/storage/raft/snapshot-force  restore, foreign keyring
//	GET  /v1/sys/health                       cluster_id / sealed / standby (no token)

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Health is the subset of /v1/sys/health a backup or restore acts on.
type Health struct {
	Initialized bool   `json:"initialized"`
	Sealed      bool   `json:"sealed"`
	Standby     bool   `json:"standby"`
	ClusterID   string `json:"cluster_id"`
	ClusterName string `json:"cluster_name"`
	Version     string `json:"version"`
}

// healthQuery makes /sys/health answer 200 in every state, so the body — not the
// status code — carries the verdict, and a sealed or standby node is a readable
// answer instead of an HTTP error that looks like an outage.
const healthQuery = "?standbyok=true&perfstandbyok=true&sealedcode=200&uninitcode=200&drsecondarycode=200"

// ReadHealth reads /v1/sys/health. It needs no token.
func ReadHealth(ctx context.Context, hc *http.Client, addr string) (Health, error) {
	var h Health
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(addr, "/")+"/v1/sys/health"+healthQuery, nil)
	if err != nil {
		return h, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return h, fmt.Errorf("OpenBao health: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return h, fmt.Errorf("OpenBao health: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if err := json.Unmarshal(b, &h); err != nil {
		return h, fmt.Errorf("OpenBao health: unparseable response: %v", err)
	}
	return h, nil
}

// Save streams a Raft snapshot, bounded by MaxSnapshotBytes. It is VERIFIED here,
// before any caller can upload it: a snapshot that does not parse is an error at
// the source, not a corrupt object discovered on the day it is needed.
func Save(ctx context.Context, hc *http.Client, addr, token string) ([]byte, Manifest, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(addr, "/")+"/v1/sys/storage/raft/snapshot", nil)
	if err != nil {
		return nil, Manifest{}, err
	}
	req.Header.Set("X-Vault-Token", token)
	resp, err := hc.Do(req)
	if err != nil {
		return nil, Manifest{}, fmt.Errorf("take Raft snapshot: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return nil, Manifest{}, fmt.Errorf("take Raft snapshot: HTTP %d: %s%s", resp.StatusCode,
			strings.TrimSpace(string(b)), snapshotDeniedHint(resp.StatusCode))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxSnapshotBytes+1))
	if err != nil {
		return nil, Manifest{}, fmt.Errorf("take Raft snapshot: read: %w", err)
	}
	if len(data) > MaxSnapshotBytes {
		return nil, Manifest{}, fmt.Errorf("take Raft snapshot: larger than %d bytes — raise MaxSnapshotBytes and the CronJob memory limit together", MaxSnapshotBytes)
	}
	m, err := Verify(data)
	if err != nil {
		return nil, Manifest{}, fmt.Errorf("OpenBao returned a snapshot that does not verify: %w", err)
	}
	return data, m, nil
}

func snapshotDeniedHint(code int) string {
	if code == http.StatusForbidden {
		return " — the token lacks `read` on sys/storage/raft/snapshot; the openbao-backup policy grants it, and is written by `llz ci bao-configure` (re-run bootstrap-openbao.yml on a deployment configured before this policy existed)"
	}
	return ""
}

// ErrForeignSnapshot means OpenBao refused a non-forced restore because the
// snapshot's sealed hashes do not open under this cluster's keyring — it came
// from a different cluster (a rebuilt one, or the HA peer).
var ErrForeignSnapshot = errors.New("the snapshot was taken by a different OpenBao cluster (its sealed hashes do not open under this cluster's keyring)")

// Restore posts data to the restore endpoint. force selects snapshot-force.
//
// It does not verify the archive first; callers do, and must, because they also
// need the Manifest to show the operator. A 400 on a non-forced restore whose
// message is about the sealed hashes is classified as ErrForeignSnapshot so the
// caller can explain --force instead of printing OpenBao's internals.
func Restore(ctx context.Context, hc *http.Client, addr, token string, data []byte, force bool) error {
	path := "/v1/sys/storage/raft/snapshot"
	if force {
		path += "-force"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(addr, "/")+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("X-Vault-Token", token)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = int64(len(data))
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("restore Raft snapshot: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 == 2 {
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	msg := strings.TrimSpace(string(b))
	if !force && resp.StatusCode == http.StatusBadRequest && looksForeign(msg) {
		return fmt.Errorf("%w: %s", ErrForeignSnapshot, msg)
	}
	return fmt.Errorf("restore Raft snapshot: HTTP %d: %s", resp.StatusCode, msg)
}

// looksForeign recognises OpenBao's refusal of a snapshot it cannot authenticate.
// The exact wording has moved between releases, so this keys on the stable nouns
// rather than a sentence.
func looksForeign(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "sealed hash") || strings.Contains(m, "unseal hash") ||
		strings.Contains(m, "hash file") || strings.Contains(m, "message authentication failed")
}
