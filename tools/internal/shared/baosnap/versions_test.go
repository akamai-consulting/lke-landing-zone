package baosnap

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// versionedS3 is a fake versioned bucket: ListObjectVersions with paging, and
// DELETE ?versionId=.
type versionedS3 struct {
	mu       sync.Mutex
	versions []Version
	pageSize int
	deletes  int
	garbage  bool
}

func (f *versionedS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := r.URL.Query()
	_, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	switch {
	case r.Method == http.MethodGet && q.Has("versions"):
		if f.garbage {
			_, _ = w.Write([]byte("<html>proxy error"))
			return
		}
		start := 0
		if m := q.Get("key-marker"); m != "" {
			start, _ = strconv.Atoi(m)
		}
		end := len(f.versions)
		if f.pageSize > 0 && start+f.pageSize < end {
			end = start + f.pageSize
		}
		var b strings.Builder
		b.WriteString("<ListVersionsResult>")
		for _, v := range f.versions[start:end] {
			if v.DeleteMarker {
				fmt.Fprintf(&b, "<DeleteMarker><Key>%s</Key><VersionId>%s</VersionId><IsLatest>%v</IsLatest></DeleteMarker>", v.Key, v.VersionID, v.IsLatest)
			} else {
				fmt.Fprintf(&b, "<Version><Key>%s</Key><VersionId>%s</VersionId><IsLatest>%v</IsLatest><Size>%d</Size></Version>", v.Key, v.VersionID, v.IsLatest, v.Size)
			}
		}
		if end < len(f.versions) {
			fmt.Fprintf(&b, "<IsTruncated>true</IsTruncated><NextKeyMarker>%d</NextKeyMarker><NextVersionIdMarker>v</NextVersionIdMarker>", end)
		} else {
			b.WriteString("<IsTruncated>false</IsTruncated>")
		}
		b.WriteString("</ListVersionsResult>")
		_, _ = w.Write([]byte(b.String()))
	case r.Method == http.MethodDelete && q.Has("versionId"):
		f.deletes++
		id := q.Get("versionId")
		for i, v := range f.versions {
			if v.Key == key && v.VersionID == id {
				f.versions = append(f.versions[:i], f.versions[i+1:]...)
				break
			}
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusBadRequest)
	}
}

func newVersioned(t *testing.T, vs ...Version) (*versionedS3, Store) {
	f := &versionedS3{versions: vs}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, Store{AccessKey: "AK", SecretKey: "SK", Endpoint: srv.URL, Bucket: "b", HTTP: srv.Client()}
}

func TestListVersionsWalksPagesAndKeepsDeleteMarkers(t *testing.T) {
	var vs []Version
	for i := 0; i < 5; i++ {
		vs = append(vs, Version{Key: fmt.Sprintf("openbao-raft/k%d.snap", i), VersionID: fmt.Sprintf("v%d", i), Size: 3})
	}
	vs = append(vs, Version{Key: "openbao-raft/k0.snap", VersionID: "dm", DeleteMarker: true, IsLatest: true})
	f, s := newVersioned(t, vs...)
	f.pageSize = 2
	got, err := s.ListVersions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	markers := 0
	for _, v := range got {
		if v.DeleteMarker {
			markers++
		}
	}
	if len(got) != 6 || markers != 1 {
		t.Errorf("ListVersions across pages = %d versions (%d markers), want 6 (1)", len(got), markers)
	}
	if err := s.DeleteVersion(context.Background(), "openbao-raft/k1.snap", "v1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ListVersions(context.Background()); len(got) != 5 {
		t.Errorf("DeleteVersion did not remove exactly that version: %d left", len(got))
	}
}

// "Could not tell" is never "empty".
func TestListVersionsFailsClosedOnGarbage(t *testing.T) {
	f, s := newVersioned(t)
	f.garbage = true
	if _, err := s.ListVersions(context.Background()); err == nil {
		t.Error("an unparseable listing was read as an empty bucket")
	}
}
