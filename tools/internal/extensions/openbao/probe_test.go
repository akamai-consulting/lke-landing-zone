package openbao

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func probeClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(srv.URL, "t", "", 5*time.Second)
}

func TestSealStatus(t *testing.T) {
	c := probeClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sys/seal-status" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"sealed":false,"initialized":true}`))
	})
	si, err := c.SealStatus(context.Background())
	if err != nil {
		t.Fatalf("SealStatus: %v", err)
	}
	if si.Sealed || !si.Initialized {
		t.Errorf("got %+v, want {Sealed:false Initialized:true}", si)
	}
}

func TestSealStatusSealed(t *testing.T) {
	c := probeClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"sealed":true,"initialized":true}`))
	})
	si, err := c.SealStatus(context.Background())
	if err != nil || !si.Sealed {
		t.Fatalf("got (%+v,%v), want Sealed:true", si, err)
	}
}

func TestSealStatusError(t *testing.T) {
	c := probeClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) })
	if _, err := c.SealStatus(context.Background()); err == nil {
		t.Error("500 should error")
	}
}

func TestMetadataUpdatedTime(t *testing.T) {
	want := "2026-06-01T12:00:00.123456Z"
	c := probeClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/secret/metadata/loki/object-store" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":{"updated_time":"` + want + `","current_version":3}}`))
	})
	got, ok, err := c.MetadataUpdatedTime(context.Background(), "secret/loki/object-store")
	if err != nil || !ok {
		t.Fatalf("MetadataUpdatedTime = (%v,%v,%v)", got, ok, err)
	}
	if !got.Equal(time.Date(2026, 6, 1, 12, 0, 0, 123456000, time.UTC)) {
		t.Errorf("time = %v", got)
	}
}

func TestMetadataUpdatedTime404(t *testing.T) {
	c := probeClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) })
	_, ok, err := c.MetadataUpdatedTime(context.Background(), "secret/absent")
	if err != nil || ok {
		t.Errorf("404 should be (_,false,nil), got ok=%v err=%v", ok, err)
	}
}

func TestMetadataUpdatedTimeUnparseable(t *testing.T) {
	c := probeClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"updated_time":"not-a-time"}}`))
	})
	if _, _, err := c.MetadataUpdatedTime(context.Background(), "secret/x"); err == nil {
		t.Error("unparseable updated_time should error")
	}
}

func TestSealStatusMalformedJSON(t *testing.T) {
	c := probeClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{not json`))
	})
	if _, err := c.SealStatus(context.Background()); err == nil {
		t.Error("malformed seal-status body should error")
	}
}

func TestMetadataUpdatedTimeServerError(t *testing.T) {
	c := probeClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) })
	if _, _, err := c.MetadataUpdatedTime(context.Background(), "secret/x"); err == nil {
		t.Error("500 should error")
	}
}

func TestMetadataUpdatedTimeEmpty(t *testing.T) {
	c := probeClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"current_version":1}}`)) // no updated_time
	})
	_, ok, err := c.MetadataUpdatedTime(context.Background(), "secret/x")
	if err != nil || ok {
		t.Errorf("empty updated_time should be (_,false,nil), got ok=%v err=%v", ok, err)
	}
}

func TestMetadataUpdatedTimeMalformedJSON(t *testing.T) {
	c := probeClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{not json`))
	})
	if _, _, err := c.MetadataUpdatedTime(context.Background(), "secret/x"); err == nil {
		t.Error("malformed metadata body should error")
	}
}
