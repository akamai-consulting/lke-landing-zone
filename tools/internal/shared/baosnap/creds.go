package baosnap

// creds.go — what a restore must put back after it rolls the store back.
//
// A Raft restore replaces the WHOLE store, including the KV paths the platform's
// own rotators keep current. The rotators revoke a superseded credential at the
// provider (a Linode object-storage key, the in-cluster PAT) after writing its
// successor, so a snapshot older than a rotation holds a key that no longer works
// anywhere — and the restore puts it back as if it were current.
//
// The value that was live just BEFORE the restore is the one the outside world
// still accepts: the rotators wrote it, and nothing has revoked it. So both
// callers record every tracked credential path (credpaths.CredPaths — the table
// the rotators and the age sampler share, so a new credential is covered the day
// it is tracked) before restoring, and afterwards re-write any path the restore
// changed:
//
//   - `llz ci assert-openbao-restore` (the e2e drill), whose snapshot is seconds
//     old, so what it puts back is a rotation that landed inside the window;
//   - `llz ci openbao-restore` (the restore-openbao workflow), whose snapshot is
//     hours or days old, so what it puts back is every rotation since.
//
// ONE COPY, HERE, because they are the same rule and a drill that cleaned up with
// its own copy would prove the copy — the split-contract archetype, with the gate
// on the wrong side of it.
//
// WHAT IT CANNOT KNOW: whether the live value was itself the damage being undone.
// A path re-applied here is reported by name so the operator can tell.

import (
	"context"
	"fmt"
	"sort"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cli"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/credpaths"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/openbao"
)

// CredState is a set of KV paths' data as read at one moment; a nil entry means
// the path did not exist.
type CredState map[string]map[string]string

// TrackedCredPaths are the paths a restore protects from its own rollback: every
// tracked credential, read from credpaths.CredPaths.
func TrackedCredPaths() []string {
	out := make([]string, 0, len(credpaths.CredPaths))
	for _, p := range credpaths.CredPaths {
		out = append(out, p.Path)
	}
	return out
}

// CaptureCreds reads paths through c. A read error is an error — never a
// "missing" path, which would make the re-apply skip a credential it could not
// see rather than one that was not there.
func CaptureCreds(ctx context.Context, c *openbao.Client, paths []string) (CredState, error) {
	st := CredState{}
	for _, p := range paths {
		raw, ok, err := c.Read(ctx, openbao.DataPath(p))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", p, err)
		}
		if !ok {
			st[p] = nil
			continue
		}
		st[p] = KVStrings(raw)
	}
	return st, nil
}

// KVStrings flattens a KV v2 read (`data.data`) to string fields.
func KVStrings(raw map[string]any) map[string]string {
	out := map[string]string{}
	inner, _ := raw["data"].(map[string]any)
	for k, v := range inner {
		out[k] = cli.AsString(v)
	}
	return out
}

// CredsToReapply returns, sorted, the paths whose value after the restore is not
// what it was just before it — the writes the rollback undid. A path that did not
// exist before is never "re-applied" (there is nothing to put back). Pure.
func CredsToReapply(before, after CredState) []string {
	var out []string
	for p, b := range before {
		if b == nil {
			continue
		}
		if !sameFields(b, after[p]) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

func sameFields(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

// ReapplyCreds re-reads before's paths through c (which must carry a token valid
// on the RESTORED store) and writes back the pre-restore value of every path the
// restore changed. Returns the paths it re-wrote, sorted.
func ReapplyCreds(ctx context.Context, c *openbao.Client, before CredState) ([]string, error) {
	if len(before) == 0 {
		return nil, nil
	}
	paths := make([]string, 0, len(before))
	for p := range before {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	after, err := CaptureCreds(ctx, c, paths)
	if err != nil {
		return nil, err
	}
	todo := CredsToReapply(before, after)
	for _, p := range todo {
		if err := c.Write(ctx, p, before[p]); err != nil {
			return nil, fmt.Errorf("re-apply %s: %w", p, err)
		}
	}
	return todo, nil
}
