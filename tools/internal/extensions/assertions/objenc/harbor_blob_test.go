package objenc

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/harborauth"
)

// fakeRegistry is just enough of the OCI distribution API to take one blob: the
// /v2/ challenge, the token service, an upload session, and the monolithic PUT.
// It records the PUT so a test can assert the bytes actually reached the
// "storage driver" rather than stopping at a session.
type fakeRegistry struct {
	grant     []string // actions the token grants on the probe repo
	putStatus int      // what the completing PUT answers
	putBody   []byte   // what the PUT carried
	digest    string   // the ?digest= the PUT named
}

func (f *fakeRegistry) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v2/":
		w.Header().Set("Www-Authenticate", `Bearer realm="https://reg.example/service/token",service="harbor-registry"`)
		w.WriteHeader(http.StatusUnauthorized)
	case r.Method == http.MethodGet && r.URL.Path == "/service/token":
		if u, p, ok := r.BasicAuth(); !ok || u != "robot" || p != "pw" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		acts := `"` + strings.Join(f.grant, `","`) + `"`
		io.WriteString(w, `{"token":"tok","access":[{"type":"repository","name":"`+harborEncProbeRepo+`","actions":[`+acts+`]}]}`)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/blobs/uploads/"):
		w.Header().Set("Location", "/v2/"+harborEncProbeRepo+"/blobs/uploads/session-1?_state=x")
		w.WriteHeader(http.StatusAccepted)
	case r.Method == http.MethodPut:
		f.digest = r.URL.Query().Get("digest")
		f.putBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(f.putStatus)
		io.WriteString(w, `{"errors":[{"code":"UNKNOWN","detail":"s3aws: NoSuchBucket"}]}`)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// withFakeRegistry routes harborauth.HTTP into f, and stubs the readback so a
// test decides what the pushed blob looks like in object storage.
func withFakeRegistry(t *testing.T, f *fakeRegistry, readback func(bucket, key string) (sseVerdict, string)) {
	t.Helper()
	prevHTTP, prevProbe, prevCreds := harborauth.HTTP, ObjectSSEProbe, ObjEncConsumerCreds
	prevNow, prevSleep := objEncNow, objEncSleep
	t.Cleanup(func() {
		harborauth.HTTP, ObjectSSEProbe, ObjEncConsumerCreds = prevHTTP, prevProbe, prevCreds
		objEncNow, objEncSleep = prevNow, prevSleep
	})
	harborauth.HTTP = func(req *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		f.serve(rec, req)
		return rec.Result(), nil
	}
	ObjectSSEProbe = func(_, _, _, bucket, key string) (sseVerdict, string) { return readback(bucket, key) }
	ObjEncConsumerCreds = func(Deps, string, string, string) (string, string, error) { return "ak", "sk", nil }
	// A clock that advances on every sleep, so the absent-blob poll terminates
	// without the test waiting thirty real seconds.
	now := testNow
	objEncNow = func() time.Time { return now }
	objEncSleep = func(d time.Duration) { now = now.Add(d) }
}

var robot = harborauth.RobotCreds{Username: "robot", Password: "pw", RegistryHost: "reg.example"}

// THE CASE THE GATE EXISTS FOR, end to end: Harbor really writes the blob, and the
// object it lands as reports server-side encryption. The readback must HEAD the
// key distribution derives from the digest the PUT named — not some other path.
func TestCheckHarborBlobPassesWhenHarborsWriteIsEncrypted(t *testing.T) {
	f := &fakeRegistry{grant: []string{"pull", "push"}, putStatus: http.StatusCreated}
	var headed string
	withFakeRegistry(t, f, func(bucket, key string) (sseVerdict, string) {
		headed = bucket + "/" + key
		return sseEncrypted, "x-amz-server-side-encryption: AES256"
	})
	nonce := []byte("unique-probe-bytes")
	if got := checkHarborBlobIsEncrypted(testDeps(t), "ep", "harbor-bkt", robot, nonce); len(got) != 0 {
		t.Fatalf("an encrypted Harbor write must pass, got %+v", got)
	}
	if string(f.putBody) != string(nonce) {
		t.Errorf("the PUT carried %q, want the probe nonce — a push that moves no bytes proves nothing", f.putBody)
	}
	if want := "harbor-bkt/" + harborBlobStorageKey(f.digest); headed != want {
		t.Errorf("read back %q, want %q (the key distribution stores this digest under)", headed, want)
	}
}

// A FRESH plaintext blob is the endpoint not encrypting new writes, and the fix
// must say what that almost always means: an E0/E1 endpoint.
func TestCheckHarborBlobFailsWhenHarborsWriteIsPlaintext(t *testing.T) {
	f := &fakeRegistry{grant: []string{"pull", "push"}, putStatus: http.StatusCreated}
	withFakeRegistry(t, f, func(string, string) (sseVerdict, string) { return ssePlaintext, "no header" })
	got := checkHarborBlobIsEncrypted(testDeps(t), "ep", "harbor-bkt", robot, []byte("n"))
	if len(got) != 1 || !strings.Contains(got[0].Problem, "PLAINTEXT") {
		t.Fatalf("a plaintext fresh blob must be one finding, got %+v", got)
	}
	if !strings.Contains(got[0].Fix, "E0/E1") {
		t.Errorf("the fix must name the endpoint generation: %q", got[0].Fix)
	}
}

// The registry acknowledges before a separate HEAD necessarily sees the object, so
// a 404 is polled — but not forever, and the eventual finding must say the blob
// never appeared rather than calling it plaintext.
func TestCheckHarborBlobPollsThenGivesUpOnAMissingBlob(t *testing.T) {
	f := &fakeRegistry{grant: []string{"pull", "push"}, putStatus: http.StatusCreated}
	heads := 0
	withFakeRegistry(t, f, func(string, string) (sseVerdict, string) { heads++; return sseAbsent, "HTTP 404" })
	got := checkHarborBlobIsEncrypted(testDeps(t), "ep", "harbor-bkt", robot, []byte("n"))
	if len(got) != 1 || !strings.Contains(got[0].Problem, "never appeared") {
		t.Fatalf("a blob that never appears must be one finding, got %+v", got)
	}
	if heads < 2 {
		t.Errorf("HEADed %d time(s) — a single 404 right after the PUT is a race, not an answer", heads)
	}
}

// A 400 on a blob written THIS SECOND cannot be an SSE-C leftover, so it is not
// read as "encrypted" here, unlike in the object sample.
func TestCheckHarborBlobRefusesACustomerKeyAnswerOnAFreshBlob(t *testing.T) {
	f := &fakeRegistry{grant: []string{"pull", "push"}, putStatus: http.StatusCreated}
	withFakeRegistry(t, f, func(string, string) (sseVerdict, string) { return sseCustomerKey, "HTTP 400" })
	got := checkHarborBlobIsEncrypted(testDeps(t), "ep", "harbor-bkt", robot, []byte("n"))
	if len(got) != 1 || !strings.Contains(got[0].Problem, "could not classify") {
		t.Fatalf("a 400 on a fresh blob must not pass, got %+v", got)
	}
}

// The storage stage failing is attributed to the storage stage, and never reaches
// the readback — there is no object to judge.
func TestCheckHarborBlobAttributesARefusedPutToStorage(t *testing.T) {
	f := &fakeRegistry{grant: []string{"pull", "push"}, putStatus: http.StatusInternalServerError}
	withFakeRegistry(t, f, func(string, string) (sseVerdict, string) {
		t.Error("read back a blob the registry refused to write")
		return sseUnknown, ""
	})
	got := checkHarborBlobIsEncrypted(testDeps(t), "ep", "harbor-bkt", robot, []byte("n"))
	if len(got) != 1 || !strings.Contains(got[0].Fix, "could not PERSIST") {
		t.Fatalf("a refused PUT must be attributed to the storage path, got %+v", got)
	}
}

// Harbor hands out a valid token with an EMPTY grant when the robot lacks scope, so
// "I got a token" is not authorization — and the finding must say it is an
// authorization problem, not an encryption one.
func TestCheckHarborBlobAttributesAMissingScopeToAuth(t *testing.T) {
	f := &fakeRegistry{grant: []string{"pull"}, putStatus: http.StatusCreated}
	withFakeRegistry(t, f, func(string, string) (sseVerdict, string) { return sseEncrypted, "" })
	got := checkHarborBlobIsEncrypted(testDeps(t), "ep", "harbor-bkt", robot, []byte("n"))
	if len(got) != 1 || !strings.Contains(got[0].Fix, "AUTHORIZATION") {
		t.Fatalf("a token without push must be an auth finding, got %+v", got)
	}
}

func TestCheckHarborBlobAttributesARejectedCredentialToAuth(t *testing.T) {
	f := &fakeRegistry{grant: []string{"pull", "push"}, putStatus: http.StatusCreated}
	withFakeRegistry(t, f, func(string, string) (sseVerdict, string) { return sseEncrypted, "" })
	bad := robot
	bad.Password = "wrong"
	got := checkHarborBlobIsEncrypted(testDeps(t), "ep", "harbor-bkt", bad, []byte("n"))
	if len(got) != 1 || !strings.Contains(got[0].Problem, "REJECTED") || !strings.Contains(got[0].Fix, "AUTHORIZATION") {
		t.Fatalf("a rejected credential must be an auth finding, got %+v", got)
	}
}

func TestCheckHarborBlobAttributesAnUnreachableRegistry(t *testing.T) {
	f := &fakeRegistry{}
	withFakeRegistry(t, f, func(string, string) (sseVerdict, string) { return sseEncrypted, "" })
	harborauth.HTTP = func(*http.Request) (*http.Response, error) { return nil, errors.New("dial tcp: no route to host") }
	got := checkHarborBlobIsEncrypted(testDeps(t), "ep", "harbor-bkt", robot, []byte("n"))
	if len(got) != 1 || !strings.Contains(got[0].Fix, "REGISTRY is unreachable") {
		t.Fatalf("an unreachable registry must say so, got %+v", got)
	}
}

// The readback reads the CLUSTER's credential first; ambient AWS_* is the Terraform
// state key in the assert-suite and cannot see the data buckets.
func TestObjEncBucketReadCredsPrefersTheClusterSecret(t *testing.T) {
	prevCreds, prevAK, prevSK := ObjEncConsumerCreds, objEncAccessKey, objEncSecretKey
	t.Cleanup(func() { ObjEncConsumerCreds, objEncAccessKey, objEncSecretKey = prevCreds, prevAK, prevSK })
	objEncAccessKey = func() string { return "state-ak" }
	objEncSecretKey = func() string { return "state-sk" }

	ObjEncConsumerCreds = func(Deps, string, string, string) (string, string, error) { return "loki-ak", "loki-sk", nil }
	if ak, _, err := objEncBucketReadCreds(testDeps(t)); err != nil || ak != "loki-ak" {
		t.Errorf("got %q (%v), want the cluster's credential ahead of ambient AWS_*", ak, err)
	}

	ObjEncConsumerCreds = func(Deps, string, string, string) (string, string, error) { return "", "", errors.New("forbidden") }
	if ak, _, err := objEncBucketReadCreds(testDeps(t)); err != nil || ak != "state-ak" {
		t.Errorf("got %q (%v), want the ambient fallback when the Secret is unreadable", ak, err)
	}

	objEncAccessKey = func() string { return "" }
	if _, _, err := objEncBucketReadCreds(testDeps(t)); err == nil {
		t.Error("no credential anywhere must be an error, not an empty key")
	}
}

// The Secret reader must decode the CONSUMER's real Secret shape, and refuse a
// malformed reference rather than guessing a namespace.
func TestObjEncConsumerCredsReadsTheNamedSecret(t *testing.T) {
	d := testDeps(t)
	var asked []string
	d.KubectlOut = func(args ...string) (string, error) {
		asked = args
		return `{"data":{"AWS_ACCESS_KEY_ID":"YWs=","AWS_SECRET_ACCESS_KEY":"c2s="}}`, nil
	}
	ak, sk, err := ObjEncConsumerCreds(d, LokiObjSecretRef, "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY")
	if err != nil || ak != "ak" || sk != "sk" {
		t.Fatalf("got %q/%q (%v), want ak/sk", ak, sk, err)
	}
	if strings.Join(asked, " ") != "-n monitoring get secret loki-s3-linode-credentials -o json" {
		t.Errorf("read %v, want the Loki Secret in monitoring", asked)
	}
	if _, _, err := ObjEncConsumerCreds(d, "no-slash", "a", "b"); err == nil {
		t.Error("a ref without namespace/ must be refused")
	}
	d.KubectlOut = func(...string) (string, error) { return `{"data":{}}`, nil }
	if _, _, err := ObjEncConsumerCreds(d, LokiObjSecretRef, "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"); err == nil {
		t.Error("a Secret missing the field must be an error, not an empty credential")
	}
}

// The object sample stops at the first unlistable bucket and names it.
func TestCheckObjectsEncryptedReportsAListFailure(t *testing.T) {
	withSSESample(t, nil, errors.New("listing b returned HTTP 403 (AccessDenied)"), allEncrypted)
	f := checkObjectsAreEncrypted(testDeps(t), "ep", []string{"b"}, 50, testWindow)
	if len(f) != 1 || !strings.Contains(f[0].Problem, "could not list b") {
		t.Fatalf("a LIST failure must be one finding naming the bucket, got %+v", f)
	}
}

// The verb's flags are the contract the suite lane and the e2e workflow call, so
// their names and defaults are pinned.
func TestAssertObjEncryptionCmdFlags(t *testing.T) {
	c := AssertObjEncryptionCmd()
	for name, def := range map[string]string{
		"region": c.Flag("region").DefValue, "since": "24h0m0s", "sample": "50",
		"endpoint": "", "bucket": "", "harbor-bucket": "",
	} {
		fl := c.Flag(name)
		if fl == nil {
			t.Errorf("--%s is gone", name)
			continue
		}
		if fl.DefValue != def {
			t.Errorf("--%s default = %q, want %q", name, fl.DefValue, def)
		}
	}
	// No --region SKIPS: the verb must run end to end to a clean exit without a spec.
	t.Setenv("REGION", "")
	c = AssertObjEncryptionCmd()
	c.SetArgs([]string{"--region="})
	if err := c.Execute(); err != nil {
		t.Errorf("no --region must skip cleanly: %v", err)
	}
}
