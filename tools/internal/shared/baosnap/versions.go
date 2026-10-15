package baosnap

// versions.go — every VERSION in the backup bucket, and deleting one.
//
// The bucket is versioned (llz-object-storage), so "empty" has two meanings: no
// current objects (what List sees), and no versions or delete markers at all
// (what a bucket delete needs, and what "no backup can be recovered from here"
// means). The destroy-time guard needs the second, and must not mistake a bucket
// whose snapshots were all pruned — but are still recoverable as noncurrent
// versions — for one with nothing in it.

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/objstore"
)

// Version is one object version or delete marker.
type Version struct {
	Key, VersionID string
	IsLatest       bool
	// DeleteMarker: a tombstone, not data. A bucket holding only these has
	// nothing recoverable in it, but still cannot be deleted until they go.
	DeleteMarker bool
	Size         int64
}

type versionsResult struct {
	Versions []struct {
		Key       string `xml:"Key"`
		VersionID string `xml:"VersionId"`
		IsLatest  bool   `xml:"IsLatest"`
		Size      int64  `xml:"Size"`
	} `xml:"Version"`
	DeleteMarkers []struct {
		Key       string `xml:"Key"`
		VersionID string `xml:"VersionId"`
		IsLatest  bool   `xml:"IsLatest"`
	} `xml:"DeleteMarker"`
	IsTruncated         bool   `xml:"IsTruncated"`
	NextKeyMarker       string `xml:"NextKeyMarker"`
	NextVersionIDMarker string `xml:"NextVersionIdMarker"`
}

// ListVersions returns every version and delete marker in the bucket, walking all
// pages. Like List, a page cap reached or a response that does not parse is an
// error — "could not tell" is never "empty".
func (s Store) ListVersions(ctx context.Context) ([]Version, error) {
	var out []Version
	keyMarker, vidMarker := "", ""
	for page := 0; page < listPageCap; page++ {
		q := "max-keys=1000&versions="
		if keyMarker != "" {
			q += "&key-marker=" + objstore.S3EscapeQueryComponent(keyMarker)
		}
		if vidMarker != "" {
			q += "&version-id-marker=" + objstore.S3EscapeQueryComponent(vidMarker)
		}
		resp, err := s.do(ctx, http.MethodGet, "", q, nil)
		if err != nil {
			return nil, fmt.Errorf("list versions %s: %w", s.Bucket, err)
		}
		if resp.StatusCode/100 != 2 {
			err := errFrom("list versions "+s.Bucket, resp)
			resp.Body.Close()
			return nil, err
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("list versions %s: %w", s.Bucket, err)
		}
		var vr versionsResult
		if err := xml.Unmarshal(body, &vr); err != nil {
			return nil, fmt.Errorf("list versions %s: unparseable ListObjectVersions response: %v", s.Bucket, err)
		}
		for _, v := range vr.Versions {
			out = append(out, Version{Key: v.Key, VersionID: v.VersionID, IsLatest: v.IsLatest, Size: v.Size})
		}
		for _, d := range vr.DeleteMarkers {
			out = append(out, Version{Key: d.Key, VersionID: d.VersionID, IsLatest: d.IsLatest, DeleteMarker: true})
		}
		if !vr.IsTruncated {
			return out, nil
		}
		if vr.NextKeyMarker == "" && vr.NextVersionIDMarker == "" {
			return nil, fmt.Errorf("list versions %s: truncated page with no next marker", s.Bucket)
		}
		keyMarker, vidMarker = vr.NextKeyMarker, vr.NextVersionIDMarker
	}
	return nil, fmt.Errorf("list versions %s: more than %d pages — refusing to act on a partial listing", s.Bucket, listPageCap)
}

// DeleteVersion permanently removes one version (or delete marker) of key. An
// empty versionID is the unversioned "null" version. A 404 is success.
func (s Store) DeleteVersion(ctx context.Context, key, versionID string) error {
	if versionID == "" {
		versionID = "null"
	}
	resp, err := s.do(ctx, http.MethodDelete, key, "versionId="+objstore.S3EscapeQueryComponent(versionID), nil)
	if err != nil {
		return fmt.Errorf("delete %s/%s@%s: %w", s.Bucket, key, versionID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusNotFound {
		return errFrom("delete "+s.Bucket+"/"+key+"@"+versionID, resp)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}
