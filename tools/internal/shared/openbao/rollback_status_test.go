package openbao

// rollback_status_test.go — a rollback that did not happen must not report that
// it did.
//
// Rollback is only ever reached from one place: DualWrite, after the secondary
// write failed and the primary is holding the NEW credential. Its job is to put
// the old one back, and the caller turns its return value straight into the
// sentence a human reads — "primary rolled back to v7" or "MANUAL INTERVENTION".
//
// c.do reports TRANSPORT failures only. A 403 from an expired token, a 429, a
// 503 from a sealed node all arrive as err == nil, and both requests in this
// function used to be issued without anyone reading the status. Worse, OpenBao's
// error body decodes cleanly into kvResponse — it simply has no `data` key — so
// a failed read produced an empty map that the restore then POSTed over the live
// secret.
//
// These are the arms that make the difference visible: the wire (what the server
// actually received) and the sentence (what DualWrite told the operator).

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// rollbackServer records what it was asked to do and answers by script.
type rollbackServer struct {
	mu       sync.Mutex
	getCode  int    // status for the versioned GET
	getBody  string // body for the versioned GET
	postCode int    // status for the restore POST
	delCode  int    // status for the metadata DELETE

	posts []map[string]any // every restore body that reached the server
	dels  int
	srv   *httptest.Server
}

func newRollbackServer(t *testing.T) *rollbackServer {
	s := &rollbackServer{getCode: 200, postCode: 200, delCode: 204}
	s.getBody = `{"data":{"data":{"password":"old"},"metadata":{"version":1}}}`
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *rollbackServer) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch r.Method {
	case http.MethodGet:
		w.WriteHeader(s.getCode)
		_, _ = io.WriteString(w, s.getBody)
	case http.MethodPost:
		b, _ := io.ReadAll(r.Body)
		var got map[string]any
		_ = json.Unmarshal(b, &got)
		s.posts = append(s.posts, got)
		w.WriteHeader(s.postCode)
	case http.MethodDelete:
		s.dels++
		w.WriteHeader(s.delCode)
	}
}

func (s *rollbackServer) client() *Client {
	return &Client{addr: s.srv.URL, token: "t", http: s.srv.Client()}
}

// A NON-2xx ON EITHER LEG IS A FAILED ROLLBACK. Both requests are covered by one
// table because they failed the same way for the same reason, and fixing one
// without the other would leave the sentence just as wrong.
func TestRollbackRefusesEveryStatusItCannotCallSuccess(t *testing.T) {
	for _, tc := range []struct {
		name              string
		getCode, postCode int
		wantIn            string
	}{
		{"read forbidden", http.StatusForbidden, 200, "reading v1"},
		{"read rate limited", http.StatusTooManyRequests, 200, "reading v1"},
		{"read server error", http.StatusInternalServerError, 200, "reading v1"},
		{"read not found", http.StatusNotFound, 200, "reading v1"},
		{"restore forbidden", 200, http.StatusForbidden, "restoring v1"},
		{"restore rate limited", 200, http.StatusTooManyRequests, "restoring v1"},
		{"restore sealed", 200, http.StatusServiceUnavailable, "restoring v1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newRollbackServer(t)
			s.getCode, s.postCode = tc.getCode, tc.postCode

			err := s.client().Rollback(context.Background(), "secret/infra/x", 1)
			if err == nil {
				t.Fatal("Rollback reported success on a request the server refused")
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error does not say which leg failed: %v", err)
			}
			// The status has to be in the message. "rollback failed" without it
			// cannot tell an expired token from a sealed node, and the two have
			// nothing in common as remedies.
			if !strings.Contains(err.Error(), "HTTP") {
				t.Errorf("error does not carry the status: %v", err)
			}
			// A read that failed must not be followed by a write. This is the
			// arm that stops the empty-secret restore.
			if tc.getCode != 200 && len(s.posts) != 0 {
				t.Errorf("a failed read was still followed by %d restore write(s): %v", len(s.posts), s.posts)
			}
		})
	}
}

// THE EMPTY RESTORE, WHICH IS THE WORSE HALF. OpenBao answers an error with
// `{"errors":[…]}`, which unmarshals into kvResponse without complaint and
// leaves the data map nil. The old code marshalled that straight back out as
// {"data":null} and POSTed it over the live secret — a rollback that destroys
// the credential it was invoked to preserve.
func TestRollbackNeverRestoresAnEmptySecretOverTheLiveOne(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		body string
	}{
		{"error body behind a 403", http.StatusForbidden, `{"errors":["permission denied"]}`},
		{"error body behind a 200", 200, `{"errors":["permission denied"]}`},
		{"data present but empty", 200, `{"data":{"data":{},"metadata":{"version":1}}}`},
		{"data null", 200, `{"data":{"data":null,"metadata":{"version":1}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newRollbackServer(t)
			s.getCode, s.getBody = tc.code, tc.body

			if err := s.client().Rollback(context.Background(), "secret/infra/x", 1); err == nil {
				t.Fatal("Rollback reported success having read no data to restore")
			}
			if len(s.posts) != 0 {
				t.Fatalf("an empty restore reached the server: %v", s.posts)
			}
		})
	}
}

// THE HAPPY PATH STILL RESTORES THE REAL BYTES. A check that refuses everything
// passes every failure test and breaks the one thing the function is for.
func TestRollbackRestoresThePriorVersionsData(t *testing.T) {
	s := newRollbackServer(t)
	if err := s.client().Rollback(context.Background(), "secret/infra/x", 1); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if len(s.posts) != 1 {
		t.Fatalf("posts = %v, want exactly one restore", s.posts)
	}
	data, _ := s.posts[0]["data"].(map[string]any)
	if data["password"] != "old" {
		t.Errorf("restored %v, want the prior version's data", s.posts[0])
	}
}

// THE DELETE BRANCH IS NOT BEST-EFFORT EITHER. priorVersion==0 destroys the path
// and every version under it; a caller told that succeeded does not come back.
func TestRollbackToZeroRefusesAStatusItCannotCallSuccess(t *testing.T) {
	s := newRollbackServer(t)
	s.delCode = http.StatusForbidden
	if err := s.client().Rollback(context.Background(), "secret/infra/x", 0); err == nil {
		t.Fatal("a refused metadata delete reported success")
	}
	// 404 is the goal state by another route — nothing is there, which is what
	// the delete was for.
	s2 := newRollbackServer(t)
	s2.delCode = http.StatusNotFound
	if err := s2.client().Rollback(context.Background(), "secret/infra/x", 0); err != nil {
		t.Errorf("a 404 on the metadata delete = %v, want nil (already absent)", err)
	}
}

// ── at the consumer ──────────────────────────────────────────────────────────

// THE SENTENCE THE OPERATOR READS. Rollback returning nil is not the harm; the
// harm is DualWrite turning that nil into "primary rolled back to v1" while the
// primary still serves the new credential and the secondary the old one — a
// split neither region can detect on its own, announced as a clean recovery.
func TestDualWriteDoesNotClaimARollbackTheServerRefused(t *testing.T) {
	ctx := context.Background()

	// The primary answers everything except the ROLLBACK's read. The versioned
	// GET is the only request carrying `?version=`, so refusing exactly that one
	// reproduces the real shape — a token that still works for the write and a
	// permission that does not cover reading an old version — without having to
	// count requests.
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Query().Get("version") != "":
			http.Error(w, `{"errors":["permission denied"]}`, http.StatusForbidden)
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"data":{"data":{"password":"old"},"metadata":{"version":1}}}`)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(primary.Close)

	// The secondary refuses the write, which is what sends DualWrite to Rollback.
	secondary := newFakeBao(t)
	secondary.failWrite = true

	pc := &Client{addr: primary.URL, token: "t", http: primary.Client()}
	err := DualWrite(ctx, pc, secondary.client(), "secret/infra/x", map[string]string{"password": "new"})
	if err == nil {
		t.Fatal("DualWrite reported success with both a failed secondary and a failed rollback")
	}
	if strings.Contains(err.Error(), "rolled back") {
		t.Errorf("DualWrite claims a rollback the server refused: %v", err)
	}
	if !strings.Contains(err.Error(), "MANUAL INTERVENTION") {
		t.Errorf("a failed rollback must escalate, got: %v", err)
	}
}
