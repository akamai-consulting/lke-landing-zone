// Package ghapi reads the GitHub API as JSON, through the `gh` CLI.
//
// IT IS DELIBERATELY NOT internal/shared/ghcli. That package's own doc opens
// "Package ghcli is the `gh` command line as llz SHOWS it, not as llz runs it" --
// it owns the argv a dry-run prints and the quoting that makes it copy-pasteable,
// and execution lives elsewhere on purpose. This executes, so it is its own
// package rather than a violation of that split.
//
// IT CAME OUT OF internal/extensions/buildpreflight, where three peers were
// reaching for it. Reading a paginated API response is not a build-preflight
// concern; preflight was simply the first thing that needed it.
//
// THE --slurp FALLBACK IS THE PART WITH A SCAR IN IT, preserved verbatim below: an
// older `gh` rejects the flag at parse time with exit 1 and NO stdout, which used
// to fall through to a decode of empty output and surface as "could not determine
// whether the passphrase exists" -- with no way forward for an operator on a
// distro-packaged gh.
package ghapi

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

// GHAPIJSON runs `gh api <path>` and decodes the response into out. Package var
// so tests substitute the whole GitHub round-trip.
var GHAPIJSON = func(path string, out any) error {
	b, err := kubectlprobe.Exec("gh", "api", path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// GHAPIJSONPaged is GHAPIJSON over every page. `--paginate --slurp` yields an
// ARRAY of page objects, so the caller's shape is decoded per page and the named
// list fields concatenated — a single-object decode would silently keep page 1.
var GHAPIJSONPaged = func(path string, out any) error {
	b, err := kubectlprobe.Exec("gh", "api", "--paginate", "--slurp", path)
	if err != nil {
		// `--slurp` is a recent `gh api` flag. An older gh rejects it at parse time
		// — exit 1, NO stdout — so this is the only place that case is visible. It
		// used to fall through to a decode of empty output and surface as "could not
		// determine whether the passphrase exists", with no way forward for an
		// operator on a distro-packaged gh. Retry unpaged: one page is worse than a
		// hard stop, and the list this serves is short.
		if strings.Contains(err.Error(), "unknown flag") || strings.Contains(err.Error(), "unknown shorthand") {
			return GHAPIJSON(path, out)
		}
		return err
	}
	// --slurp ALWAYS wraps, including a single page, so a non-array here means the
	// shape changed rather than "this gh did not slurp".
	var pages []json.RawMessage
	if err := json.Unmarshal(b, &pages); err != nil {
		return fmt.Errorf("gh api --slurp %s: expected an array of pages: %w", path, err)
	}
	return MergeJSONPages(pages, out)
}

// MergeJSONPages decodes each page into a fresh copy of out's type, appends every
// SLICE field, and takes the LAST non-zero value of every other field.
//
// Reflection rather than a per-caller merge because the alternative is each caller
// hand-rolling pagination, which is how page-1 truncation gets reintroduced.
//
// Non-slice fields are carried rather than dropped: the first cut only appended
// slices, which silently zeroed `total_count` — the one field you would use to
// CHECK pagination was complete. Scalars are page-invariant on GitHub list
// endpoints (every page repeats the same total), so last-non-zero is well defined.
//
// Limitation, stated rather than discovered later: out must point at a STRUCT, so
// this cannot serve the gh endpoints returning a top-level array. Those need a
// slice-shaped variant; there is no caller for one yet.
func MergeJSONPages(pages []json.RawMessage, out any) error {
	dst := reflect.ValueOf(out)
	if dst.Kind() != reflect.Ptr || dst.Elem().Kind() != reflect.Struct {
		return fmt.Errorf("MergeJSONPages: out must be a pointer to struct, got %T", out)
	}
	for _, p := range pages {
		page := reflect.New(dst.Elem().Type())
		if err := json.Unmarshal(p, page.Interface()); err != nil {
			return err
		}
		for i := 0; i < dst.Elem().NumField(); i++ {
			f, pf := dst.Elem().Field(i), page.Elem().Field(i)
			if !f.CanSet() {
				continue // unexported
			}
			if f.Kind() == reflect.Slice {
				f.Set(reflect.AppendSlice(f, pf))
				continue
			}
			if !pf.IsZero() {
				f.Set(pf)
			}
		}
	}
	return nil
}
