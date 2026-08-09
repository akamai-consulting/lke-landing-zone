package reconcilelanes

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kube"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/platform"
)

func TestSCIsDefault(t *testing.T) {
	def := func(v string) map[string]any {
		return map[string]any{"metadata": map[string]any{"annotations": map[string]any{platform.SCDefaultAnnotation: v}}}
	}
	if !scIsDefault(def("true")) {
		t.Error(`is-default-class "true" should be default`)
	}
	for _, sc := range []map[string]any{
		def("false"),
		{"metadata": map[string]any{"annotations": map[string]any{}}},
		{"metadata": map[string]any{}},
		{},
	} {
		if scIsDefault(sc) {
			t.Errorf("should not be default: %v", sc)
		}
	}
}

// scServer serves GET/PATCH for one StorageClass name and records patches.
func scServer(t *testing.T, name string, getStatus int, isDefault bool) (*kube.Client, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var patched []string
	path := SCStorageClassesPath + "/" + name
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet:
			if getStatus != 0 && getStatus != 200 {
				w.WriteHeader(getStatus)
				return
			}
			ann := map[string]any{}
			if isDefault {
				ann[platform.SCDefaultAnnotation] = "true"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"name": name, "annotations": ann}})
		case http.MethodPatch:
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			patched = append(patched, string(body))
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)
	return kube.NewClient(srv.URL, "tok", srv.Client()), &patched
}

func TestReconcileSCDemoteWhenDefault(t *testing.T) {
	client, patched := scServer(t, DefaultDemoteSC, 200, true)
	if err := SCDemote(context.Background(), client, DefaultDemoteSC); err != nil {
		t.Fatalf("SCDemote: %v", err)
	}
	if len(*patched) != 1 || !strings.Contains((*patched)[0], `"storageclass.kubernetes.io/is-default-class":"false"`) {
		t.Fatalf("expected one demote patch, got %v", *patched)
	}
}

func TestReconcileSCDemoteAlreadyNonDefaultIsNoOp(t *testing.T) {
	client, patched := scServer(t, DefaultDemoteSC, 200, false)
	if err := SCDemote(context.Background(), client, DefaultDemoteSC); err != nil {
		t.Fatalf("SCDemote: %v", err)
	}
	if len(*patched) != 0 {
		t.Fatalf("non-default SC should not be patched, got %v", *patched)
	}
}

func TestReconcileSCDemoteAbsentIsNoOp(t *testing.T) {
	client, patched := scServer(t, DefaultDemoteSC, 404, false)
	if err := SCDemote(context.Background(), client, DefaultDemoteSC); err != nil {
		t.Fatalf("404 should be a no-op, got %v", err)
	}
	if len(*patched) != 0 {
		t.Fatalf("absent SC should not be patched, got %v", *patched)
	}
}

func TestReconcileSCDemoteServerErrorSurfaces(t *testing.T) {
	client, _ := scServer(t, DefaultDemoteSC, 500, false)
	if err := SCDemote(context.Background(), client, DefaultDemoteSC); err == nil {
		t.Error("500 should surface an error")
	}
}
