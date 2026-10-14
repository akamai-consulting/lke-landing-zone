package kube

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// streamEvents writes the given watch frames to w, flushing after each so the
// client's decoder sees them incrementally (a real watch trickles frames).
func streamEvents(t *testing.T, w http.ResponseWriter, frames ...WatchEvent) {
	t.Helper()
	fl, ok := w.(http.Flusher)
	if !ok {
		t.Fatal("ResponseWriter is not a Flusher")
	}
	enc := json.NewEncoder(w)
	for _, f := range frames {
		if err := enc.Encode(f); err != nil {
			t.Fatalf("encode frame: %v", err)
		}
		fl.Flush()
	}
}

func app(name string) map[string]any {
	return map[string]any{"metadata": map[string]any{"name": name}}
}

func TestWatchStreamsFramesThenEOF(t *testing.T) {
	c, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("watch") != "true" {
			t.Errorf("watch query not set: %s", r.URL.RawQuery)
		}
		if r.URL.Query().Get("resourceVersion") != "42" {
			t.Errorf("resourceVersion = %q, want 42", r.URL.Query().Get("resourceVersion"))
		}
		streamEvents(t, w,
			WatchEvent{Type: "ADDED", Object: app("a")},
			WatchEvent{Type: "MODIFIED", Object: app("a")},
			WatchEvent{Type: "DELETED", Object: app("a")},
		)
		// handler returns → body closes → client sees EOF
	})

	var got []string
	err := c.Watch(context.Background(), "/apis/argoproj.io/v1alpha1/applications", "42", func(ev WatchEvent) error {
		md, _ := ev.Object["metadata"].(map[string]any)
		got = append(got, ev.Type+":"+md["name"].(string))
		return nil
	})
	if err != nil {
		t.Fatalf("Watch returned %v, want nil on clean stream end", err)
	}
	want := []string{"ADDED:a", "MODIFIED:a", "DELETED:a"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("frame %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestWatchNoResourceVersionOmitsParam(t *testing.T) {
	c, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("resourceVersion") {
			t.Errorf("resourceVersion should be omitted when empty: %s", r.URL.RawQuery)
		}
		streamEvents(t, w, WatchEvent{Type: "ADDED", Object: app("a")})
	})
	if err := c.Watch(context.Background(), "/api/v1/pods", "", func(WatchEvent) error { return nil }); err != nil {
		t.Fatalf("Watch: %v", err)
	}
}

func TestWatchNon200Errors(t *testing.T) {
	c, _ := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"kind":"Status","code":403}`))
	})
	err := c.Watch(context.Background(), "/api/v1/pods", "", func(WatchEvent) error { return nil })
	if err == nil {
		t.Fatal("expected error on 403, got nil")
	}
}

func TestWatchHandlerErrorStopsStream(t *testing.T) {
	c, _ := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		streamEvents(t, w,
			WatchEvent{Type: "ADDED", Object: app("a")},
			WatchEvent{Type: "ADDED", Object: app("b")},
		)
		// keep the stream open so the test proves fn's error — not EOF — stopped it
		<-time.After(2 * time.Second)
	})
	sentinel := context.Canceled // any non-nil error value
	calls := 0
	err := c.Watch(context.Background(), "/api/v1/pods", "", func(WatchEvent) error {
		calls++
		return sentinel
	})
	if err != sentinel {
		t.Fatalf("Watch returned %v, want the handler's sentinel error", err)
	}
	if calls != 1 {
		t.Fatalf("fn called %d times, want 1 (stream must stop on first fn error)", calls)
	}
}

func TestWatchContextCancelReturnsCtxErr(t *testing.T) {
	firstSeen := make(chan struct{})
	c, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		streamEvents(t, w, WatchEvent{Type: "ADDED", Object: app("a")})
		<-r.Context().Done() // hold the stream open until the client cancels
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-firstSeen
		cancel()
	}()

	err := c.Watch(ctx, "/api/v1/pods", "", func(WatchEvent) error {
		select {
		case <-firstSeen:
		default:
			close(firstSeen)
		}
		return nil
	})
	if err == nil {
		t.Fatal("expected ctx error after cancel, got nil")
	}
	if ctx.Err() == nil {
		t.Fatal("ctx should be cancelled")
	}
}

func TestWatchMalformedFrameErrors(t *testing.T) {
	c, _ := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"type":"ADDED","object":{}}` + "\n" + `{not json`))
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
	})
	calls := 0
	err := c.Watch(context.Background(), "/api/v1/pods", "", func(WatchEvent) error {
		calls++
		return nil
	})
	if err == nil {
		t.Fatal("expected decode error on malformed frame")
	}
	if calls != 1 {
		t.Fatalf("fn called %d times, want 1 (the one valid frame before the garbage)", calls)
	}
}

// TestWatchAnErrorFrameIsNotACleanClose.
//
// The apiserver reports 410 Gone (the requested resourceVersion aged out),
// aggregation failures and cache errors IN BAND: an ERROR frame carrying a
// metav1.Status, followed by EOF. Neither the transport-error nor the non-2xx
// path above sees any of that. Handing the frame to fn — which every caller in
// this repo ignores — and then returning nil on the EOF scored a stream the
// server had just killed as a healthy close, so the reconcile loop reset its
// consecutive-failure count, published llz_watch_connected=1, left
// llz_watch_errors_total flat, and re-established at the 1s backoff floor: a
// ~1Hz relist loop with a gauge asserting it was fine.
func TestWatchAnErrorFrameIsNotACleanClose(t *testing.T) {
	c, _ := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		streamEvents(t, w,
			WatchEvent{Type: "ADDED", Object: app("a")},
			WatchEvent{Type: "ERROR", Object: map[string]any{
				"kind": "Status", "code": float64(410), "reason": "Expired",
				"message": "too old resource version: 1 (2)",
			}},
		)
	})

	seen := 0
	err := c.Watch(context.Background(), "/apis/argoproj.io/v1alpha1/applications", "", func(WatchEvent) error {
		seen++
		return nil
	})
	if err == nil {
		t.Fatal("an ERROR frame ends the stream and is NOT a clean close — returning nil resets the " +
			"caller's failure count and its backoff, producing a relist loop the metrics call healthy")
	}
	// The reason and code are what separate "relist, this is routine" from "your
	// RBAC is wrong"; collapsing them to "watch failed" sends the reader to a
	// live cluster to find out which they have.
	for _, want := range []string{"410", "Expired", "too old resource version"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must carry the status detail %q, got: %v", want, err)
		}
	}
	if seen != 1 {
		t.Errorf("frames before the ERROR must still be delivered, got %d", seen)
	}
}

// TestWatchAnErrorFrameWithNoStatusStillErrors — fail closed. A frame this code
// cannot parse is still an ERROR frame, and reading it as a clean close is the
// same defect with a less helpful message.
func TestWatchAnErrorFrameWithNoStatusStillErrors(t *testing.T) {
	c, _ := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		streamEvents(t, w, WatchEvent{Type: "ERROR"})
	})
	if err := c.Watch(context.Background(), "/api/v1/nodes", "", func(WatchEvent) error { return nil }); err == nil {
		t.Error("an ERROR frame with an unreadable status must still end the stream as an error")
	}
}
