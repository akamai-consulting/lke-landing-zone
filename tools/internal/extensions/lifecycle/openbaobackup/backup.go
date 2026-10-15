package openbaobackup

// backup.go — `llz ci openbao-backup`, the openbaoBackup CronJob's verb.
//
// One run: log in under the openbao-backup role, read the bucket key from
// OpenBao, take a Raft snapshot from the ACTIVE node, verify it, upload it, read
// it back, then apply retention. It prints one JSON audit record as its last
// line; the restore drill reads the uploaded key from it.
//
// ORDER IS THE SAFETY ARGUMENT:
//
//   - verify BEFORE upload: a body that is not a snapshot (a proxy error page, a
//     truncated stream) fails the run at the source instead of becoming the
//     newest object in the bucket — which is the one a hurried restore picks.
//   - read back BEFORE prune: retention deletes old snapshots, and it may only do
//     that once the new one is proven to be in the bucket byte-for-byte. A PUT
//     that answered 200 is the producer's claim; the GET is the consumer's.
//   - prune is best-effort: a failed delete leaves an extra snapshot, which costs
//     storage, never a backup. It is reported in the record, not swallowed.
//
// WHY THE ACTIVE SERVICE. A snapshot is served by the active node; the plain
// Service load-balances across all three pods, so two runs in three would land on
// a standby and depend on request forwarding to stream the archive through. The
// chart's `-active` Service selects only the leader. Its name is not on the
// serving certificate (which names the plain Service and the per-pod raft hosts),
// so the TLS ServerName is pinned to the plain Service's name — the identity the
// certificate proves — while the connection goes to the active endpoint.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/baosnap"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cigate"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cli"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/openbao"
)

// Defaults, each a scar or a sizing decision recorded where it is made.
const (
	// DefaultActiveAddr is the chart's leader-only Service. See the file comment.
	DefaultActiveAddr = "https://platform-openbao-active.llz-openbao.svc.cluster.local:8200"
	// DefaultTLSServerName is the name the serving certificate carries for the
	// Service. Verifying against it while dialing the -active Service is what
	// keeps TLS verified rather than skipped.
	DefaultTLSServerName = "platform-openbao.llz-openbao.svc.cluster.local"
	// DefaultKeep is 28 snapshots: seven days at the CronJob's default six-hourly
	// schedule. Long enough to reach back past a bad change noticed after a
	// weekend; short enough that the bucket stays a few hundred megabytes at most.
	// It is the spec default (components.openbaoBackup.backupKeep), read from the
	// one constant render also uses, and it is PER OpenBao cluster id — see
	// baosnap.Prune.
	DefaultKeep = clusterspec.OpenBaoBackupDefaultKeep
)

// Seams. Package vars so the run is testable with no cluster, no OpenBao and no
// bucket: the test drives a fake OpenBao and a fake S3 over httptest.
var (
	httpClientFn = func(serverName string) (*http.Client, error) {
		c, err := openbao.HTTPClientMTLSFromFiles(
			cigate.EnvOr("OPENBAO_CA_FILE", openbao.DefaultCAFile),
			cigate.EnvOr("OPENBAO_CLIENT_CERT_FILE", openbao.DefaultClientCertFile),
			cigate.EnvOr("OPENBAO_CLIENT_KEY_FILE", openbao.DefaultClientKeyFile),
			5*time.Minute)
		if err != nil {
			return nil, fmt.Errorf("%w — the backup pod needs its llz-client-ca Certificate and the openbao-ca bundle mounted; see docs/adr/0010-in-cluster-mtls.md", err)
		}
		if serverName != "" {
			if tr, ok := c.Transport.(*http.Transport); ok && tr.TLSClientConfig != nil {
				cfg := tr.TLSClientConfig.Clone()
				cfg.ServerName = serverName
				tr = tr.Clone()
				tr.TLSClientConfig = cfg
				c.Transport = tr
			}
		}
		return c, nil
	}
	saTokenFn = func() (string, error) {
		b, err := os.ReadFile(cigate.EnvOr("SA_TOKEN_FILE", "/var/run/secrets/kubernetes.io/serviceaccount/token"))
		if err != nil {
			return "", fmt.Errorf("read ServiceAccount token: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	storeHTTP *http.Client // nil in production; the fake S3's client in tests
	// virtualHosted is true in production; tests turn it off to address an
	// httptest server by path.
	virtualHosted = true
	nowFn         = time.Now
)

// Record is the audit line. Every field is something an operator or the drill
// reads; nothing secret is in it.
type Record struct {
	Event     string   `json:"event"`
	Action    string   `json:"action"`
	Bucket    string   `json:"bucket"`
	Key       string   `json:"key"`
	Bytes     int      `json:"bytes"`
	SHA256    string   `json:"sha256"`
	ClusterID string   `json:"cluster_id"`
	RaftIndex uint64   `json:"raft_index"`
	Retained  int      `json:"retained"`
	Pruned    []string `json:"pruned"`
	PruneErrs []string `json:"prune_errors,omitempty"`
}

// RunBackup is one CronJob tick.
func RunBackup(ctx context.Context, keep int) (Record, error) {
	rec := Record{Event: baosnap.AuditEvent, Action: "failed"}
	addr := cigate.EnvOr("OPENBAO_ADDR", DefaultActiveAddr)
	role := cigate.EnvOr("OPENBAO_KUBERNETES_ROLE", baosnap.Role)

	hc, err := httpClientFn(cigate.EnvOr("OPENBAO_TLS_SERVER_NAME", DefaultTLSServerName))
	if err != nil {
		return rec, err
	}
	jwt, err := saTokenFn()
	if err != nil {
		return rec, err
	}
	token, err := openbao.KubernetesLogin(ctx, hc, addr, "kubernetes", role, jwt)
	if err != nil {
		return rec, fmt.Errorf("kubernetes-auth login as %q: %w — the role is written by `llz ci bao-configure`; a deployment configured before it existed needs bootstrap-openbao.yml re-run", role, err)
	}
	bao := openbao.NewWithClient(addr, token, "", hc)

	// The bucket key, read fresh every run: the reconciler rotates it, and a key
	// cached from an earlier run is the one the rotator just revoked.
	raw, ok, err := bao.Read(ctx, openbao.DataPath(baosnap.KeyPath))
	if err != nil {
		return rec, fmt.Errorf("read %s: %w", baosnap.KeyPath, err)
	}
	if !ok {
		return rec, fmt.Errorf("%s does not exist — `llz ci mint-bootstrap-objkeys` seeds it at bootstrap and the reconciler's linode-creds lane re-mints it; "+
			"on an upgraded deployment, apply the object-storage root (it creates the backup bucket) and let the reconciler mint the key", baosnap.KeyPath)
	}
	st, err := baosnap.FieldsToStore(kvStrings(raw))
	if err != nil {
		return rec, err
	}
	st.HTTP = storeHTTP
	// Off the objProxy rewrite — see baosnap.Store.VirtualHosted for why that is
	// a correctness requirement, not a preference.
	st.VirtualHosted = virtualHosted
	rec.Bucket = st.Bucket

	h, err := baosnap.ReadHealth(ctx, hc, addr)
	if err != nil {
		return rec, err
	}
	if h.Sealed || !h.Initialized {
		return rec, fmt.Errorf("OpenBao is not serving (initialized=%v sealed=%v) — nothing to snapshot", h.Initialized, h.Sealed)
	}
	rec.ClusterID = h.ClusterID

	data, m, err := baosnap.Save(ctx, hc, addr, token)
	if err != nil {
		return rec, err
	}
	rec.Key = baosnap.ObjectKey(nowFn(), h.ClusterID)
	rec.Bytes = len(data)
	rec.SHA256 = baosnap.SHA256(data)
	rec.RaftIndex = m.Index

	if err := st.Put(ctx, rec.Key, data); err != nil {
		return rec, err
	}
	// The consumer's read, not the producer's word for it.
	got, n, err := st.HashObject(ctx, rec.Key)
	if err != nil {
		return rec, err
	}
	if got != rec.SHA256 || int(n) != rec.Bytes {
		return rec, fmt.Errorf("read-back of %s/%s does not match what was written (%d bytes sha256 %s, wrote %d bytes sha256 %s)",
			st.Bucket, rec.Key, n, got, rec.Bytes, rec.SHA256)
	}
	rec.Action = "uploaded"

	objs, err := st.List(ctx, baosnap.Prefix)
	if err != nil {
		// The backup is in and proven; a listing failure costs only retention.
		rec.PruneErrs = append(rec.PruneErrs, err.Error())
		return rec, nil
	}
	snaps, _ := baosnap.Catalog(objs)
	prune := baosnap.Prune(snaps, keep, rec.Key)
	for _, k := range prune {
		if err := st.Delete(ctx, k); err != nil {
			rec.PruneErrs = append(rec.PruneErrs, err.Error())
			continue
		}
		rec.Pruned = append(rec.Pruned, k)
	}
	rec.Retained = len(snaps) - len(rec.Pruned)
	if rec.Pruned == nil {
		rec.Pruned = []string{}
	}
	return rec, nil
}

// kvStrings flattens a KV v2 read (`data.data`) to string fields.
func kvStrings(raw map[string]any) map[string]string {
	out := map[string]string{}
	inner, _ := raw["data"].(map[string]any)
	for k, v := range inner {
		out[k] = cli.AsString(v)
	}
	return out
}

// printRecord emits the audit line, then any prune errors as warnings.
func printRecord(rec Record) error {
	m := map[string]any{
		"event": rec.Event, "action": rec.Action, "bucket": rec.Bucket, "key": rec.Key,
		"bytes": rec.Bytes, "sha256": rec.SHA256, "cluster_id": rec.ClusterID,
		"raft_index": rec.RaftIndex, "retained": rec.Retained, "pruned": rec.Pruned,
	}
	if len(rec.PruneErrs) > 0 {
		m["prune_errors"] = rec.PruneErrs
		for _, e := range rec.PruneErrs {
			fmt.Fprintf(os.Stderr, "warning: retention: %s\n", e)
		}
	}
	return cli.PrintRecord(m)
}
