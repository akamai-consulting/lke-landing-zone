package envreq

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// chdirWithSecretsFile plants a .llz/secrets.env with a known mtime and chdirs
// there, because DetectDrift dates its verdict from that file's mtime.
func chdirWithSecretsFile(t *testing.T, mod time.Time) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".llz"), 0o700); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(dir, SecretsEnvFile)
	if err := os.WriteFile(f, []byte("X=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(f, mod, mod); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
}

func secretReq(name string) Requirement {
	return Requirement{Name: name, Secret: true, EnvScope: true, Required: true}
}

// TestDetectDrift_BehindIsTheIncident reproduces akamai/gsap-apl run
// 33556210825 in miniature: the secret was pushed, THEN the local file was
// edited with the replacement, and every presence check stayed green.
func TestDetectDrift_BehindIsTheIncident(t *testing.T) {
	edited := time.Date(2026, 9, 1, 20, 31, 5, 0, time.UTC)
	chdirWithSecretsFile(t, edited)

	st := LiveState{
		envSecrets:      map[string]bool{"OPENBAO_SECRETS_WRITE_TOKEN": true},
		envSecretTimes:  map[string]time.Time{"OPENBAO_SECRETS_WRITE_TOKEN": edited.Add(-102 * time.Second)},
		repoSecrets:     map[string]bool{},
		repoSecretTimes: map[string]time.Time{},
	}
	d := DetectDrift([]Requirement{secretReq("OPENBAO_SECRETS_WRITE_TOKEN")},
		map[string]string{"OPENBAO_SECRETS_WRITE_TOKEN": "ghp_new"}, nil, st)

	if len(d.Behind) != 1 || d.Behind[0].Name != "OPENBAO_SECRETS_WRITE_TOKEN" {
		t.Fatalf("a secret pushed 102s before the local edit must be Behind, got %+v", d.Behind)
	}
	if len(d.Ahead) != 0 {
		t.Errorf("nothing was pushed after the edit, got Ahead=%+v", d.Ahead)
	}
	if d.Empty() {
		t.Error("Empty() reported nothing to say for the exact state that broke a prod promote")
	}
}

// TestDetectDrift_AheadIsARotation is the opposite direction and the reason
// `llz tokens` does not auto-sync: CI republished the credential and revoked the
// one still sitting in the local file.
func TestDetectDrift_AheadIsARotation(t *testing.T) {
	edited := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	chdirWithSecretsFile(t, edited)

	st := LiveState{
		envSecrets:      map[string]bool{"LINODE_API_TOKEN": true},
		envSecretTimes:  map[string]time.Time{"LINODE_API_TOKEN": edited.Add(14 * 24 * time.Hour)},
		repoSecrets:     map[string]bool{},
		repoSecretTimes: map[string]time.Time{},
	}
	d := DetectDrift([]Requirement{secretReq("LINODE_API_TOKEN")},
		map[string]string{"LINODE_API_TOKEN": "stale"}, nil, st)

	if len(d.Ahead) != 1 || d.Ahead[0].Name != "LINODE_API_TOKEN" {
		t.Fatalf("a secret pushed after the local edit must be Ahead, got %+v", d.Ahead)
	}
	if len(d.Behind) != 0 {
		t.Fatalf("a rotated secret must NEVER land in the push set — that re-pushes a revoked credential: %+v", d.Behind)
	}
	if len(d.BehindNames()) != 0 {
		t.Error("BehindNames leaked the rotated secret into the push set")
	}
}

// TestDetectDrift_NoLocalFileDecidesNothing: without a local file there is no
// date to compare against, and a zero mtime would otherwise mark every secret
// on the instance as pushed "after" it and report a fleet-wide rotation.
func TestDetectDrift_NoLocalFileDecidesNothing(t *testing.T) {
	t.Chdir(t.TempDir())
	st := LiveState{
		envSecrets:     map[string]bool{"LINODE_API_TOKEN": true},
		envSecretTimes: map[string]time.Time{"LINODE_API_TOKEN": time.Now()},
	}
	d := DetectDrift([]Requirement{secretReq("LINODE_API_TOKEN")},
		map[string]string{"LINODE_API_TOKEN": "v"}, nil, st)
	if !d.Empty() || !d.LocalMod.IsZero() {
		t.Errorf("with no local file nothing is decidable, got %+v", d)
	}
}

// TestDetectDrift_UnparseableTimeIsUndecidable — an absent or malformed
// updated_at must drop out, not parse to the zero time and read as "pushed in
// year 1", which would mark a healthy secret Behind.
func TestDetectDrift_UnparseableTimeIsUndecidable(t *testing.T) {
	chdirWithSecretsFile(t, time.Now())
	if _, ok := (ghSecret{Name: "X", UpdatedAt: "not-a-time"}).updatedAt(); ok {
		t.Fatal("a malformed updated_at must not parse")
	}
	st := LiveState{ // present, but no recorded time
		envSecrets:     map[string]bool{"X": true},
		envSecretTimes: map[string]time.Time{},
	}
	d := DetectDrift([]Requirement{secretReq("X")}, map[string]string{"X": "v"}, nil, st)
	if !d.Empty() {
		t.Errorf("an undatable secret must be reported as neither behind nor ahead, got %+v", d)
	}
}

// TestDetectDrift_VariablesAreExact — variable VALUES are readable, so they get
// a real comparison rather than the timestamp heuristic secrets are stuck with.
func TestDetectDrift_VariablesAreExact(t *testing.T) {
	chdirWithSecretsFile(t, time.Now())
	st := LiveState{
		envVars:  map[string]string{"TF_IMAGE": "ghcr.io/x:old"},
		repoVars: map[string]string{},
	}
	reqs := []Requirement{{Name: "TF_IMAGE", Secret: false, EnvScope: true, Required: true}}

	d := DetectDrift(reqs, nil, map[string]string{"TF_IMAGE": "ghcr.io/x:new"}, st)
	if len(d.VarsDiffer) != 1 || d.VarsDiffer[0] != "TF_IMAGE" {
		t.Errorf("a variable whose local value differs must be reported exactly, got %+v", d.VarsDiffer)
	}
	same := DetectDrift(reqs, nil, map[string]string{"TF_IMAGE": "ghcr.io/x:old"}, st)
	if len(same.VarsDiffer) != 0 {
		t.Errorf("a matching variable must not be reported, got %+v", same.VarsDiffer)
	}
}

// TestDetectDrift_SkipsWhatItCannotOwn — a secret llz holds no local copy of
// has nothing to push, and a template-repo requirement is not this instance's to
// reconcile. Both would otherwise produce advice the operator cannot act on.
func TestDetectDrift_SkipsWhatItCannotOwn(t *testing.T) {
	edited := time.Now()
	chdirWithSecretsFile(t, edited)
	old := edited.Add(-time.Hour)
	st := LiveState{
		envSecrets:     map[string]bool{"NOT_HELD": true, "TEMPLATE_ONE": true},
		envSecretTimes: map[string]time.Time{"NOT_HELD": old, "TEMPLATE_ONE": old},
	}
	reqs := []Requirement{
		secretReq("NOT_HELD"),
		{Name: "TEMPLATE_ONE", Secret: true, EnvScope: true, Required: true, Template: true},
	}
	d := DetectDrift(reqs, map[string]string{"TEMPLATE_ONE": "v"}, nil, st)
	if !d.Empty() {
		t.Errorf("neither an unheld secret nor a template-repo one is actionable here, got %+v", d)
	}
}

// ── The exact path: what llz itself recorded pushing ─────────────────────────
//
// These cover the two false positives the mtime heuristic produced the first
// time it ran against a real instance. Both were reported by a check that was
// working exactly as designed, which is why the design changed.

func writePushLog(t *testing.T, m map[string]PushRecord) {
	t.Helper()
	if err := os.MkdirAll(".llz", 0o700); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(PushLogFile, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestDetectDrift_PushedByUsIsInSync is the false positive that mattered: five
// secrets the operator had just pushed BY HAND were reported as "CI rotated
// these, your copy is stale" — advice pointing at the opposite of the truth.
// The file mtime was older than the push, which is exactly what a rotation looks
// like from the outside; only a record of our own push separates the two.
func TestDetectDrift_PushedByUsIsInSync(t *testing.T) {
	edited := time.Date(2026, 9, 1, 20, 31, 5, 0, time.UTC)
	chdirWithSecretsFile(t, edited)
	pushedAt := edited.Add(3 * time.Hour)
	writePushLog(t, map[string]PushRecord{
		"OPENBAO_SECRETS_WRITE_TOKEN": {SHA256: Digest("ghp_current"), PushedAt: pushedAt},
	})
	st := LiveState{
		envSecrets:     map[string]bool{"OPENBAO_SECRETS_WRITE_TOKEN": true},
		envSecretTimes: map[string]time.Time{"OPENBAO_SECRETS_WRITE_TOKEN": pushedAt},
	}
	d := DetectDrift([]Requirement{secretReq("OPENBAO_SECRETS_WRITE_TOKEN")},
		map[string]string{"OPENBAO_SECRETS_WRITE_TOKEN": "ghp_current"}, nil, st)
	if !d.Empty() {
		t.Errorf("a secret llz pushed itself, unchanged since, must report nothing — got Behind=%+v Ahead=%+v", d.Behind, d.Ahead)
	}
}

// TestDetectDrift_ChangedSinceOurPushIsCertain — with a recorded digest this is
// no longer an inference from mtimes: the local value provably differs from what
// was sent.
func TestDetectDrift_ChangedSinceOurPushIsCertain(t *testing.T) {
	chdirWithSecretsFile(t, time.Unix(1, 0)) // deliberately OLDER than the push
	pushedAt := time.Now().Add(-time.Hour)
	writePushLog(t, map[string]PushRecord{
		"OPENBAO_SECRETS_WRITE_TOKEN": {SHA256: Digest("ghp_old"), PushedAt: pushedAt},
	})
	st := LiveState{
		envSecrets:     map[string]bool{"OPENBAO_SECRETS_WRITE_TOKEN": true},
		envSecretTimes: map[string]time.Time{"OPENBAO_SECRETS_WRITE_TOKEN": pushedAt},
	}
	d := DetectDrift([]Requirement{secretReq("OPENBAO_SECRETS_WRITE_TOKEN")},
		map[string]string{"OPENBAO_SECRETS_WRITE_TOKEN": "ghp_rescoped"}, nil, st)
	if len(d.Behind) != 1 {
		t.Fatalf("a value that differs from the recorded push digest must be Behind regardless of mtime, got %+v", d)
	}
}

// TestDetectDrift_ExternalWriteAfterOurPushIsAhead — the rotation case, now
// identified by the thing that actually distinguishes it: GitHub wrote the
// secret AFTER llz last did.
func TestDetectDrift_ExternalWriteAfterOurPushIsAhead(t *testing.T) {
	chdirWithSecretsFile(t, time.Now())
	ourPush := time.Now().Add(-30 * 24 * time.Hour)
	writePushLog(t, map[string]PushRecord{
		"LINODE_API_TOKEN": {SHA256: Digest("v1"), PushedAt: ourPush},
	})
	st := LiveState{
		envSecrets:     map[string]bool{"LINODE_API_TOKEN": true},
		envSecretTimes: map[string]time.Time{"LINODE_API_TOKEN": ourPush.Add(24 * time.Hour)},
	}
	d := DetectDrift([]Requirement{secretReq("LINODE_API_TOKEN")},
		map[string]string{"LINODE_API_TOKEN": "v1"}, nil, st)
	if len(d.Ahead) != 1 {
		t.Fatalf("a write later than our own push is somebody else's, got %+v", d)
	}
	if len(d.Behind) != 0 {
		t.Fatal("a rotated credential must never reach the push set")
	}
}

// TestDetectDrift_SkewMarginAbsorbsTheStamp — `gh secret set` returns before
// GitHub stamps updated_at, and the two clocks are not the same clock. Without a
// margin every push would report itself as an external write on the very next
// run.
func TestDetectDrift_SkewMarginAbsorbsTheStamp(t *testing.T) {
	chdirWithSecretsFile(t, time.Now())
	ourPush := time.Now().Add(-time.Hour)
	writePushLog(t, map[string]PushRecord{
		"APL_VALUES_REPO_TOKEN": {SHA256: Digest("v"), PushedAt: ourPush},
	})
	st := LiveState{
		envSecrets: map[string]bool{"APL_VALUES_REPO_TOKEN": true},
		// Stamped a minute after we pushed — inside the margin, so still ours.
		envSecretTimes: map[string]time.Time{"APL_VALUES_REPO_TOKEN": ourPush.Add(time.Minute)},
	}
	d := DetectDrift([]Requirement{secretReq("APL_VALUES_REPO_TOKEN")},
		map[string]string{"APL_VALUES_REPO_TOKEN": "v"}, nil, st)
	if !d.Empty() {
		t.Errorf("a stamp inside the skew margin is our own push, got %+v", d)
	}
}

// TestPushLog_RecordMerges — a push carries only what that run needed. Replacing
// the file would erase what earlier pushes established about every other
// credential, silently demoting them all back to the mtime heuristic.
func TestPushLog_RecordMerges(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll(".llz", 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := RecordPush(map[string]string{"A": "1"}, now); err != nil {
		t.Fatal(err)
	}
	if err := RecordPush(map[string]string{"B": "2"}, now); err != nil {
		t.Fatal(err)
	}
	log := LoadPushLog()
	if len(log) != 2 || log["A"].SHA256 != Digest("1") || log["B"].SHA256 != Digest("2") {
		t.Errorf("second push erased the first: %+v", log)
	}
}

// TestPushLog_StoresNoSecretMaterial — the file lives in .llz/ beside the real
// credentials and is gitignored, but it must still not BE a copy of them.
func TestPushLog_StoresNoSecretMaterial(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll(".llz", 0o700); err != nil {
		t.Fatal(err)
	}
	const secret = "ghp_averyrealsecretvalue"
	if err := RecordPush(map[string]string{"TOK": secret}, time.Now()); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(PushLogFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), secret) {
		t.Fatal("the push log wrote the credential itself, not a digest")
	}
	fi, err := os.Stat(PushLogFile)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("push log mode = %v, want 0600 to match the credential cache beside it", fi.Mode().Perm())
	}
}

// TestDetectDrift_UnreadableLogFallsBack — a truncated or hand-mangled log must
// degrade to the mtime heuristic, never fail the command or crash it.
func TestDetectDrift_UnreadableLogFallsBack(t *testing.T) {
	edited := time.Now()
	chdirWithSecretsFile(t, edited)
	if err := os.WriteFile(PushLogFile, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	st := LiveState{
		envSecrets:     map[string]bool{"X": true},
		envSecretTimes: map[string]time.Time{"X": edited.Add(-time.Hour)},
	}
	d := DetectDrift([]Requirement{secretReq("X")}, map[string]string{"X": "v"}, nil, st)
	if len(d.Behind) != 1 {
		t.Errorf("a corrupt log must fall back to the mtime comparison, got %+v", d)
	}
}
