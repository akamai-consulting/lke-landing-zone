package ghapi

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

type pageShape struct {
	TotalCount int      `json:"total_count"`
	Names      []string `json:"names"`
	//lint:ignore U1000 exercised by reflection, not by name: MergeJSONPages walks
	// every field and must skip this one (CanSet is false) rather than panic.
	unexported int
}

// The whole point of the helper: a second page must not be lost. Page-1
// truncation is the bug it exists to prevent, and the one it could most easily
// reintroduce.
func TestMergeJSONPagesKeepsEveryPage(t *testing.T) {
	pages := []json.RawMessage{
		json.RawMessage(`{"total_count":3,"names":["a","b"]}`),
		json.RawMessage(`{"total_count":3,"names":["c"]}`),
	}
	var got pageShape
	if err := MergeJSONPages(pages, &got); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if strings.Join(got.Names, ",") != "a,b,c" {
		t.Errorf("names = %v, want a,b,c", got.Names)
	}
	// The scalar is carried, not zeroed — it is the field you would use to CHECK
	// that pagination was complete, so dropping it defeats the audit.
	if got.TotalCount != 3 {
		t.Errorf("total_count = %d, want 3 (a dropped scalar hides an incomplete merge)", got.TotalCount)
	}
}

func TestMergeJSONPagesEdgeCases(t *testing.T) {
	t.Run("single page", func(t *testing.T) {
		var got pageShape
		if err := MergeJSONPages([]json.RawMessage{json.RawMessage(`{"total_count":1,"names":["x"]}`)}, &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Names) != 1 || got.TotalCount != 1 {
			t.Errorf("got %+v", got)
		}
	})
	t.Run("no pages leaves the zero value", func(t *testing.T) {
		var got pageShape
		if err := MergeJSONPages(nil, &got); err != nil {
			t.Fatal(err)
		}
		if got.Names != nil || got.TotalCount != 0 {
			t.Errorf("got %+v, want zero", got)
		}
	})
	t.Run("a malformed page errors rather than contributing nothing", func(t *testing.T) {
		var got pageShape
		if err := MergeJSONPages([]json.RawMessage{json.RawMessage(`{"names":`)}, &got); err == nil {
			t.Error("a truncated page must not merge silently")
		}
	})
	// The documented limitation. Asserted so a future caller hits a clear error
	// rather than a silent no-op.
	t.Run("a non-struct target is refused", func(t *testing.T) {
		var got []string
		err := MergeJSONPages([]json.RawMessage{json.RawMessage(`["a"]`)}, &got)
		if err == nil || !strings.Contains(err.Error(), "pointer to struct") {
			t.Errorf("err = %v, want a pointer-to-struct refusal", err)
		}
	})
	t.Run("unexported fields are skipped, not panicked on", func(t *testing.T) {
		var got pageShape
		if err := MergeJSONPages([]json.RawMessage{json.RawMessage(`{"names":["a"]}`)}, &got); err != nil {
			t.Fatalf("reflection must not panic or error on an unexported field: %v", err)
		}
	})
}

// An older `gh` rejects --slurp at parse time (exit 1, no stdout). That used to
// surface as "could not determine whether the passphrase exists" with no way
// forward; it must degrade to an unpaged read instead.
func TestGHAPIJSONPagedFallsBackWhenSlurpIsUnknown(t *testing.T) {
	var sawPaginate, sawPlain bool
	withExecOutput(t, func(_ string, args ...string) ([]byte, error) {
		for _, a := range args {
			if a == "--slurp" {
				sawPaginate = true
				return nil, errors.New("unknown flag: --slurp")
			}
		}
		sawPlain = true
		return []byte(`{"total_count":1,"names":["a"]}`), nil
	})
	var got pageShape
	if err := GHAPIJSONPaged("repos/o/r/environments", &got); err != nil {
		t.Fatalf("an older gh must degrade to one page, not hard-stop: %v", err)
	}
	if !sawPaginate || !sawPlain {
		t.Errorf("expected the --slurp attempt then the unpaged retry (paginate=%v plain=%v)", sawPaginate, sawPlain)
	}
	if len(got.Names) != 1 {
		t.Errorf("fallback lost the payload: %+v", got)
	}
}

// Any OTHER error is a real failure and must propagate — degrading a 403 to an
// unpaged read would turn an indefinite answer into a definite one.
func TestGHAPIJSONPagedPropagatesRealErrors(t *testing.T) {
	withExecOutput(t, func(string, ...string) ([]byte, error) {
		return nil, errors.New("gh: Forbidden (HTTP 403)")
	})
	var got pageShape
	if err := GHAPIJSONPaged("repos/o/r/environments", &got); err == nil {
		t.Fatal("a 403 must propagate, not degrade to a single page")
	}
}

// withExecOutput swaps the capture seam. A LOCAL COPY, as everywhere else in this
// campaign: a stream-swapping helper cannot live in a shared package without
// shipping `testing` into production code. It swaps kubectlprobe.Exec directly
// because that is what this package calls -- buildpreflight's version wrapped a
// package-local execOutput closure that did not come with the move.
func withExecOutput(t *testing.T, fn func(name string, args ...string) ([]byte, error)) {
	t.Helper()
	orig := kubectlprobe.Exec
	kubectlprobe.Exec = fn
	t.Cleanup(func() { kubectlprobe.Exec = orig })
}
