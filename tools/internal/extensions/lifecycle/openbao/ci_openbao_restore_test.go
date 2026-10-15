package openbao

// ci_openbao_restore_test.go — `llz ci openbao-restore` and `llz ci
// bao-revoke-root`, end to end against a fake OpenBao whose restore really rolls
// its KV state AND its token table back to the snapshot's.
//
// That second half is what makes the root-token tests mean something: a real
// restore forgets every token minted after the snapshot, including the one that
// performed it. A fake that kept its tokens would let "revoke root after the
// restore" pass by revoking a token the real store no longer has — and would
// never exercise the second mint, which is the only reason the post-restore steps
// can run at all.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/baosnap"
)

// restoreBao is a live OpenBao plus the state its one snapshot captured.
type restoreBao struct {
	mu         sync.Mutex
	liveID     string
	kv         map[string]map[string]string // keyed by the KV path under secret/
	tokens     map[string]bool
	snapKV     map[string]map[string]string
	snapTokens map[string]bool
	restored   int
	minted     int
	execs      int
	http       int
	// quorumBroken makes generate-root reject every share — what happens after a
	// rebuild, when infra-<region> holds the rebuild's shares, not the snapshot's.
	quorumBroken func() bool
	// lookupBroken makes token lookups get no answer at all (an exec failure).
	lookupBroken bool
	nonce        string
	shares       int
}

func copyKV(in map[string]map[string]string) map[string]map[string]string {
	out := map[string]map[string]string{}
	for k, v := range in {
		m := map[string]string{}
		for f, x := range v {
			m[f] = x
		}
		out[k] = m
	}
	return out
}

func copyTokens(in map[string]bool) map[string]bool {
	out := map[string]bool{}
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (b *restoreBao) serveHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.http++
	tok := r.Header.Get("X-Vault-Token")
	switch {
	case r.URL.Path == "/v1/sys/health":
		_ = json.NewEncoder(w).Encode(map[string]any{"initialized": true, "cluster_id": b.liveID})
	case strings.HasPrefix(r.URL.Path, "/v1/sys/storage/raft/snapshot"):
		if !b.tokens[tok] {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		b.restored++
		b.kv, b.tokens = copyKV(b.snapKV), copyTokens(b.snapTokens)
		w.WriteHeader(http.StatusNoContent)
	case strings.HasPrefix(r.URL.Path, "/v1/secret/data/"):
		if !b.tokens[tok] {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"errors":["permission denied"]}`)
			return
		}
		p := strings.TrimPrefix(r.URL.Path, "/v1/secret/data/")
		if r.Method == http.MethodPost {
			var body struct {
				Data map[string]string `json:"data"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			b.kv[p] = body.Data
			_, _ = io.WriteString(w, `{"data":{"version":2}}`)
			return
		}
		v, ok := b.kv[p]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": v}})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (b *restoreBao) exec(pod, token, stdin string, args ...string) (string, string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.execs++
	cmd := strings.Join(args, " ")
	denied := func() (string, string, error) {
		return "", "Code: 403. * permission denied", errors.New("exit status 2")
	}
	switch {
	case args[0] == "status":
		return `{"initialized":true,"sealed":false}`, "", nil
	case strings.HasPrefix(cmd, "token lookup"):
		if b.lookupBroken {
			return "", "error dialing backend: No agent available", errors.New("exit status 1")
		}
		if !b.tokens[token] {
			return denied()
		}
		return fmt.Sprintf(`{"data":{"policies":["root"],"accessor":"acc-%s"}}`, token), "", nil
	case cmd == "token revoke -self":
		if !b.tokens[token] {
			return denied()
		}
		delete(b.tokens, token)
		return "", "", nil
	case strings.Contains(cmd, "generate-root -cancel"):
		b.nonce, b.shares = "", 0
		return "", "", nil
	case strings.Contains(cmd, "generate-root -init"):
		b.nonce = fmt.Sprintf("n-%d", b.minted+1)
		return fmt.Sprintf(`{"nonce":%q,"otp":"otp"}`, b.nonce), "", nil
	case strings.Contains(cmd, "-nonce="+b.nonce) && b.nonce != "":
		if b.quorumBroken != nil && b.quorumBroken() {
			return "", "Error: invalid key", errors.New("exit status 2")
		}
		b.shares++
		if b.shares == 3 {
			return fmt.Sprintf(`{"complete":true,"progress":3,"required":3,"encoded_token":"enc-%s"}`, b.nonce), "", nil
		}
		return fmt.Sprintf(`{"complete":false,"progress":%d,"required":3}`, b.shares), "", nil
	case strings.Contains(cmd, "-decode=enc-"):
		b.minted++
		tok := fmt.Sprintf("s.minted-%d", b.minted)
		b.tokens[tok] = true
		return fmt.Sprintf(`{"token":%q}`, tok), "", nil
	}
	return "", "unexpected exec " + cmd, errors.New("unexpected")
}

func (b *restoreBao) live(tok string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tokens[tok]
}

// withCIRestore is the restore fixture (spec, bucket, kube-context, Linode
// lookups) with the fake OpenBao above behind the port-forward and the exec seam.
func withCIRestore(t *testing.T, liveID string) (*restoreBao, string, string) {
	t.Helper()
	_, key := withRestoreFixture(t, liveID)
	b := &restoreBao{
		liveID:     liveID,
		kv:         map[string]map[string]string{},
		tokens:     map[string]bool{},
		snapKV:     map[string]map[string]string{},
		snapTokens: map[string]bool{},
	}
	srv := httptest.NewServer(http.HandlerFunc(b.serveHTTP))
	t.Cleanup(srv.Close)
	restorePF = func(pod string) (string, func(), error) {
		if pod != "platform-openbao-2" {
			t.Errorf("port-forwarded to %s, not the active pod", pod)
		}
		return srv.URL, func() {}, nil
	}
	withBaoExec(t, b.exec)
	t.Setenv("RECOVERY_K1", "k1")
	t.Setenv("RECOVERY_K2", "k2")
	t.Setenv("RECOVERY_K3", "k3")
	// The workflow never hands this verb a root token; the fixture's default one
	// must not leak in and be mistaken for it.
	t.Setenv("OPENBAO_ROOT_TOKEN", "")
	t.Setenv("OPENBAO_STORED_ROOT_TOKEN", "")
	dir := t.TempDir()
	t.Setenv("RUNNER_TEMP", dir)
	t.Setenv("GITHUB_STEP_SUMMARY", filepath.Join(dir, "summary.md"))
	t.Setenv("GITHUB_ENV", filepath.Join(dir, "github-env"))
	return b, key, filepath.Join(dir, RestoreRootTokenFileName)
}

func restoreReq(key string) RestoreRequest {
	return RestoreRequest{Region: "prod", Mode: RestoreModeRestore, Snapshot: key, Confirm: "restore:prod"}
}

func readFileOrEmpty(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return string(b)
}

// THE WHOLE FLOW. Root is minted from the quorum, used, and replaced on the
// restored store; the rotated credential the snapshot predates is put back; the
// token exists only in the file; and after the revoke step no token this run
// minted is live anywhere.
func TestCIRestoreMintsRestoresReappliesAndLeavesNoLiveRoot(t *testing.T) {
	b, key, tokenFile := withCIRestore(t, "c-1")
	b.snapKV["obj/platform"] = map[string]string{"AWS_ACCESS_KEY_ID": "OLD-REVOKED"}
	b.snapKV["alerts/webhooks"] = map[string]string{"url": "same"}
	b.kv["obj/platform"] = map[string]string{"AWS_ACCESS_KEY_ID": "NEW-LIVE"}
	b.kv["alerts/webhooks"] = map[string]string{"url": "same"}
	secrets := withGHSetSecret(t, nil)

	if err := RunRestoreCI(restoreReq(key), false, ""); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if b.restored != 1 {
		t.Fatalf("restored %d time(s), want 1", b.restored)
	}
	if b.minted != 2 {
		t.Errorf("minted %d root token(s), want 2 (one to restore, one on the restored store)", b.minted)
	}
	if got := b.kv["obj/platform"]["AWS_ACCESS_KEY_ID"]; got != "NEW-LIVE" {
		t.Errorf("after the restore secret/obj/platform holds %q — the revoked pre-snapshot key was left in place", got)
	}
	if b.live("s.minted-1") {
		t.Error("the token that performed the restore is still live")
	}
	if got := strings.TrimSpace(readFileOrEmpty(t, tokenFile)); got != "s.minted-2" || !b.live(got) {
		t.Errorf("token file holds %q, want the live token minted on the restored store", got)
	}
	if fi, err := os.Stat(tokenFile); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("token file mode = %v (%v), want 0600", fi.Mode().Perm(), err)
	}
	if len(*secrets) != 0 {
		t.Errorf("wrote GitHub secrets %v — the restore workflow must never write a token back", *secrets)
	}
	if env := readFileOrEmpty(t, os.Getenv("GITHUB_ENV")); env != "" {
		t.Errorf("wrote $GITHUB_ENV %q — the token must not reach later steps' environment", env)
	}
	sum := readFileOrEmpty(t, os.Getenv("GITHUB_STEP_SUMMARY"))
	for _, want := range []string{"secret/obj/platform", "Still to do by hand", "never written to GitHub", key} {
		if !strings.Contains(sum, want) {
			t.Errorf("summary lacks %q:\n%s", want, sum)
		}
	}
	if strings.Contains(sum, "secret/alerts/webhooks") {
		t.Errorf("summary reports re-applying a path the restore did not change:\n%s", sum)
	}
	for _, secret := range []string{"s.minted", "NEW-LIVE", "OLD-REVOKED"} {
		if strings.Contains(sum, secret) {
			t.Errorf("summary carries %q — a token or credential value:\n%s", secret, sum)
		}
	}

	// The workflow's always() step.
	if err := RunRevokeRoot(tokenFile); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if b.live("s.minted-2") || b.live("s.minted-1") {
		t.Error("a root token this run minted is still live after the revoke step")
	}
	if _, err := os.Stat(tokenFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("token file survived the revoke step (%v)", err)
	}
}

// Every refusal prepareRestore and Apply's guards can reach must land BEFORE a
// recovery-key quorum is spent on a root token for a restore that will not happen.
func TestCIRestoreRefusesBeforeSpendingAQuorum(t *testing.T) {
	t.Run("foreign snapshot without force", func(t *testing.T) {
		b, key, tokenFile := withCIRestore(t, "peer-cluster")
		err := RunRestoreCI(restoreReq(key), false, "")
		if !errors.Is(err, baosnap.ErrNeedsForce) {
			t.Errorf("err = %v, want ErrNeedsForce", err)
		}
		if b.minted != 0 || b.restored != 0 || readFileOrEmpty(t, tokenFile) != "" {
			t.Errorf("minted %d, restored %d, token file %q — want nothing", b.minted, b.restored, readFileOrEmpty(t, tokenFile))
		}
	})
	t.Run("kube-context on another deployment", func(t *testing.T) {
		b, key, _ := withCIRestore(t, "c-1")
		fixtureNodes = "lke9999-1-aaaa"
		err := RunRestoreCI(restoreReq(key), false, "")
		if err == nil || !strings.Contains(err.Error(), "LKE cluster 9999") {
			t.Errorf("err = %v, want the kube-context refusal", err)
		}
		if b.minted != 0 || b.restored != 0 {
			t.Errorf("minted %d, restored %d before refusing the kube-context", b.minted, b.restored)
		}
	})
	t.Run("force reaches the restore", func(t *testing.T) {
		b, key, _ := withCIRestore(t, "peer-cluster")
		req := restoreReq(key)
		req.Force = true
		if err := RunRestoreCI(req, false, ""); err != nil {
			t.Fatalf("forced restore: %v", err)
		}
		if b.restored != 1 {
			t.Errorf("restored %d, want 1", b.restored)
		}
		if sum := readFileOrEmpty(t, os.Getenv("GITHUB_STEP_SUMMARY")); !strings.Contains(sum, "restored with force") {
			t.Errorf("a forced restore's summary does not say so:\n%s", sum)
		}
	})
}

// THE REBUILD CASE. The restored store answers only to the snapshot's recovery
// shares; infra-<region> holds the rebuild's. The restore stands, the job fails,
// and the summary says exactly why and what was not done.
func TestCIRestoreSaysWhatDidNotRunWhenTheRestoredStoreRejectsTheShares(t *testing.T) {
	b, key, tokenFile := withCIRestore(t, "c-1")
	b.kv["obj/platform"] = map[string]string{"AWS_ACCESS_KEY_ID": "NEW-LIVE"}
	b.quorumBroken = func() bool { return b.restored > 0 }
	err := RunRestoreCI(restoreReq(key), false, "")
	if err == nil || !strings.Contains(err.Error(), "restore SUCCEEDED") {
		t.Fatalf("err = %v, want the restore-succeeded-but post-restore failure", err)
	}
	sum := readFileOrEmpty(t, os.Getenv("GITHUB_STEP_SUMMARY"))
	for _, want := range []string{"post-restore steps did NOT run", "OPENBAO_RECOVERY_KEY_1", "secret/obj/platform", "bootstrap-openbao.yml"} {
		if !strings.Contains(sum, want) {
			t.Errorf("summary lacks %q:\n%s", want, sum)
		}
	}
	// The revoke step still finds the first token — and proves it gone.
	if err := RunRevokeRoot(tokenFile); err != nil {
		t.Errorf("revoke after a failed post-restore: %v", err)
	}
}

// A snapshot taken while infra-<region>::OPENBAO_ROOT_TOKEN was live brings it
// back. Reported, not revoked: someone may hold it.
func TestCIRestoreReportsAStoredRootTokenTheSnapshotRevived(t *testing.T) {
	b, key, _ := withCIRestore(t, "c-1")
	b.snapTokens["s.parked"] = true
	t.Setenv("OPENBAO_STORED_ROOT_TOKEN", "s.parked")
	if err := RunRestoreCI(restoreReq(key), false, ""); err != nil {
		t.Fatal(err)
	}
	if !b.live("s.parked") {
		t.Error("the resurrected stored token was revoked — it is reported, never revoked, by this verb")
	}
	if sum := readFileOrEmpty(t, os.Getenv("GITHUB_STEP_SUMMARY")); !strings.Contains(sum, "is **live** again") {
		t.Errorf("summary does not flag the live stored root token:\n%s", sum)
	}
}

// --preflight is the workflow's first step: it decides from the request and the
// spec alone, and touches neither the cluster nor OpenBao.
func TestCIRestorePreflightTouchesNothing(t *testing.T) {
	b, key, _ := withCIRestore(t, "c-1")
	if err := RunRestoreCI(restoreReq(key), true, ""); err != nil {
		t.Fatal(err)
	}
	if b.execs != 0 || b.http != 0 {
		t.Errorf("preflight made %d exec(s) and %d OpenBao request(s)", b.execs, b.http)
	}
	bad := restoreReq(key)
	bad.Confirm = "restore:staging"
	if err := RunRestoreCI(bad, true, ""); err == nil {
		t.Error("preflight accepted a confirmation naming another deployment")
	}
}

func TestRestoreRequestValidate(t *testing.T) {
	ok := []RestoreRequest{
		{Region: "prod", Mode: "list"},
		{Region: "prod", Mode: "plan", Latest: true},
		{Region: "prod", Mode: "plan", Latest: true, FromCluster: "c-1"},
		{Region: "prod", Mode: "plan", Snapshot: "k"},
		{Region: "prod", Mode: "restore", Snapshot: "k", Confirm: "restore:prod"},
		{Region: "prod", Mode: "restore", Latest: true, Force: true, Confirm: "restore:prod"},
	}
	for _, r := range ok {
		if err := r.Validate(); err != nil {
			t.Errorf("%+v refused: %v", r, err)
		}
	}
	bad := []RestoreRequest{
		{Mode: "plan", Latest: true},
		{Region: "prod", Mode: "apply", Latest: true},
		{Region: "prod", Mode: "", Latest: true},
		{Region: "prod", Mode: "plan"},
		{Region: "prod", Mode: "plan", Snapshot: "k", Latest: true},
		{Region: "prod", Mode: "plan", Snapshot: "k", FromCluster: "c-1"},
		{Region: "prod", Mode: "restore", Snapshot: "k"},
		{Region: "prod", Mode: "restore", Snapshot: "k", Confirm: "restore:staging"},
		{Region: "prod", Mode: "restore", Snapshot: "k", Confirm: "prod"},
		{Region: "prod", Mode: "restore", Snapshot: "k", Confirm: "restore: prod"},
		{Region: "prod", Mode: "restore", Snapshot: "k", Confirm: "destroy:prod:cluster"},
	}
	for _, r := range bad {
		if err := r.Validate(); err == nil {
			t.Errorf("%+v accepted", r)
		}
	}
}

// plan and restore refuse a deployment with backups off; list still reads the
// bucket.
func TestCIRestoreRefusesADeploymentWithBackupsOff(t *testing.T) {
	b, key, _ := withCIRestore(t, "c-1")
	envFile := filepath.Join("environments", "prod.yaml")
	raw, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envFile, append(raw, []byte("  components:\n    openbaoBackup: {enabled: false}\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{RestoreModePlan, RestoreModeRestore} {
		req := restoreReq(key)
		req.Mode = mode
		err := RunRestoreCI(req, false, "")
		if err == nil || !strings.Contains(err.Error(), "openbaoBackup is disabled") {
			t.Errorf("mode=%s with backups off: %v", mode, err)
		}
	}
	if b.restored != 0 || b.minted != 0 {
		t.Errorf("restored %d, minted %d with backups off", b.restored, b.minted)
	}
	if err := RunRestoreCI(RestoreRequest{Region: "prod", Mode: RestoreModeList}, true, ""); err != nil {
		t.Errorf("list with backups off refused: %v", err)
	}
}

func TestRevokeRootFailsClosedAndToleratesNothingMinted(t *testing.T) {
	b, _, tokenFile := withCIRestore(t, "c-1")
	if err := RunRevokeRoot(tokenFile); err != nil {
		t.Errorf("no token file: %v, want success (nothing was minted)", err)
	}
	b.tokens["s.live"] = true
	if err := os.WriteFile(tokenFile, []byte("s.live\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b.lookupBroken = true
	if err := RunRevokeRoot(tokenFile); err == nil {
		t.Error("an inconclusive lookup was reported as a revoked token")
	}
	if readFileOrEmpty(t, tokenFile) == "" {
		t.Error("the token file was deleted although the token may still be live")
	}
	b.lookupBroken = false
	if err := RunRevokeRoot(tokenFile); err != nil {
		t.Fatal(err)
	}
	if b.live("s.live") {
		t.Error("revoke left the token live")
	}
}

// A second mint must never orphan a first token it cannot account for.
func TestMintRefusesToOrphanAnUnaccountedToken(t *testing.T) {
	b, _, tokenFile := withCIRestore(t, "c-1")
	b.tokens["s.prev"] = true
	if err := os.WriteFile(tokenFile, []byte("s.prev"), 0o600); err != nil {
		t.Fatal(err)
	}
	b.lookupBroken = true
	if _, err := mintEphemeralRoot("prod", tokenFile); err == nil {
		t.Error("minted over a token whose state was unknown")
	}
	if b.minted != 0 {
		t.Errorf("minted %d token(s)", b.minted)
	}
}
