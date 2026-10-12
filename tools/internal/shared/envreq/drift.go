// drift.go — "is what CI holds still what you have?", which PRESENCE cannot answer.
//
// GitHub never reads a secret back, so no llz command can compare a pushed value
// to a local one. `llz tokens` and `llz doctor` therefore report PRESENCE from
// GitHub and VALIDITY/SCOPE from the LOCAL .llz/*.env copy — two different
// tokens, rendered as one row. When the two diverge the table is green and the
// pipeline is broken, which is exactly how akamai/gsap-apl run 33556210825
// happened: a re-scoped PAT was pasted into .llz/secrets.env two minutes AFTER
// the push that was supposed to carry it, and the promote 403'd on the old one
// while `llz doctor` reported "✓ valid ✓ scoped" for the new one.
//
// What IS comparable is metadata. GitHub reports each secret's updated_at, and
// the local file has an mtime, so "this secret was last pushed BEFORE you last
// edited the file that holds it" is decidable — and is the shape of the bug.
// Variables are stronger still: their values ARE readable, so those are compared
// exactly rather than by timestamp.
//
// BOTH DIRECTIONS MATTER, and the second is the one a naive version gets
// backwards. CI writes these secrets back: `llz ci rotate-broad-pat` mints a new
// LINODE_API_TOKEN, publishes it to every infra-<deployment> and REVOKES the
// old one, and llz-secret-rotation.yml does the same for TF_STATE_ACCESS_KEY /
// TF_STATE_SECRET_KEY. For those, a pushed copy NEWER than the local file means
// the local copy is the stale one — and pushing it back would overwrite a live
// credential with a revoked one. Hence Ahead, and hence `llz tokens` offering to
// push only what is Behind.
package envreq

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
)

// SecretsEnvFile is the local dotenv whose mtime dates every secret comparison.
const SecretsEnvFile = ".llz/secrets.env"

// pushSkewMargin is how far GitHub's updated_at may trail llz's own recorded
// push before the write is attributed to someone else. Clock skew between this
// host and GitHub, plus the gap between `gh secret set` returning and the stamp
// landing, both live inside it.
const pushSkewMargin = 2 * time.Minute

// DriftEntry is one secret whose pushed copy is datable relative to the local file.
type DriftEntry struct {
	Name     string
	PushedAt time.Time
}

// Drift is everything a metadata-only comparison can conclude. Deliberately not
// called "differences": for secrets this reports ORDERING, not inequality — see
// Note, which says so in the operator's own words rather than overclaiming.
type Drift struct {
	// Behind: pushed before the local secrets file was last edited. If that edit
	// touched this credential, CI is still running the previous value.
	Behind []DriftEntry
	// Ahead: pushed after the local file was last edited — CI's rotation writing
	// back, or a push from another machine. The LOCAL copy is the stale one.
	Ahead []DriftEntry
	// VarsDiffer: variables whose local value is not the pushed value. Exact, not
	// a heuristic — variable values are readable.
	VarsDiffer []string
	// LocalMod is the secrets file's mtime; zero when the file does not exist,
	// which makes every secret comparison undecidable and empties Behind/Ahead.
	LocalMod time.Time
}

// Empty reports whether there is nothing to tell the operator.
func (d Drift) Empty() bool {
	return len(d.Behind) == 0 && len(d.Ahead) == 0 && len(d.VarsDiffer) == 0
}

// DetectDrift compares the local .llz/*.env against what GitHub last recorded.
//
// Only requirements already present on GitHub are considered: a MISSING one is
// the wizard's job to provision and is reported by the readiness table, and
// reporting it twice under two different framings is worse than once.
func DetectDrift(reqs []Requirement, secrets, vars map[string]string, st LiveState) Drift {
	var d Drift
	log := LoadPushLog()
	if fi, err := os.Stat(SecretsEnvFile); err == nil {
		d.LocalMod = fi.ModTime()
	}
	for _, r := range reqs {
		// The template repo's own e2e harness is not this instance's to reconcile.
		if r.Template || !st.Has(r.Name, r.Secret) {
			continue
		}
		if !r.Secret {
			if local, ok := vars[r.Name]; ok && local != st.Value(r.Name) {
				d.VarsDiffer = append(d.VarsDiffer, r.Name)
			}
			continue
		}
		// A secret llz has no local copy of cannot be stale locally — there is
		// nothing to push and nothing to warn about.
		local, held := secrets[r.Name]
		if !held {
			continue
		}
		pushed, ok := st.SecretUpdatedAt(r.Name)
		if !ok {
			continue
		}
		// EXACT PATH — llz pushed this itself and recorded what it sent.
		if rec, logged := log[r.Name]; logged {
			switch {
			// Written after our push: a rotation CronJob, or another machine. Our
			// recorded digest describes a value that is no longer the live one, and
			// the local copy it came from may since have been revoked.
			//
			// The margin absorbs clock skew between this host and GitHub, and the
			// gap between `gh secret set` returning and updated_at being stamped.
			// Without it every push races itself and reports as someone else's.
			case pushed.After(rec.PushedAt.Add(pushSkewMargin)):
				d.Ahead = append(d.Ahead, DriftEntry{r.Name, pushed})
			// Same value we sent, and nobody has written since: in sync, for real,
			// with no timestamp guessing involved.
			case Digest(local) == rec.SHA256:
			// The local value changed after we pushed it. This is the incident, and
			// on this path it is a certainty rather than an inference.
			default:
				d.Behind = append(d.Behind, DriftEntry{r.Name, pushed})
			}
			continue
		}
		// FALLBACK — no record of llz having pushed this one (an instance
		// provisioned before the log existed, or a secret set by hand in the GitHub
		// UI). Order the file against the secret and say only what that supports.
		if d.LocalMod.IsZero() {
			continue
		}
		if pushed.Before(d.LocalMod) {
			d.Behind = append(d.Behind, DriftEntry{r.Name, pushed})
		} else {
			d.Ahead = append(d.Ahead, DriftEntry{r.Name, pushed})
		}
	}
	sort.Slice(d.Behind, func(i, j int) bool { return d.Behind[i].Name < d.Behind[j].Name })
	sort.Slice(d.Ahead, func(i, j int) bool { return d.Ahead[i].Name < d.Ahead[j].Name })
	sort.Strings(d.VarsDiffer)
	return d
}

// BehindNames is the push set — what `llz secrets push` would usefully carry.
func (d Drift) BehindNames() []string {
	out := make([]string, 0, len(d.Behind))
	for _, e := range d.Behind {
		out = append(out, e.Name)
	}
	return out
}

// Note renders the operator-facing warning, or "" when there is nothing to say.
//
// The wording is careful about what it knows. mtime is per-FILE, so a Behind
// entry means "pushed before your last edit to this file", NOT "differs" — and
// saying "differs" would train operators to ignore a warning that is sometimes
// wrong. Saying the weaker, true thing keeps it worth reading.
func (d Drift) Note(env, repo string) string {
	if d.Empty() {
		return ""
	}
	var b strings.Builder
	stamp := func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04Z") }

	if len(d.Behind) > 0 {
		fmt.Fprintf(&b, "\n%s %s\n", color.Yellow("⚠"), fmt.Sprintf(
			"%d pushed secret(s) predate your local %s (edited %s):",
			len(d.Behind), SecretsEnvFile, stamp(d.LocalMod)))
		for _, e := range d.Behind {
			fmt.Fprintf(&b, "    %-30s pushed %s\n", e.Name, color.Dim(stamp(e.PushedAt)))
		}
		b.WriteString(color.Dim(fmt.Sprintf(
			"  If that edit changed any of them, CI is still using the OLD value.\n"+
				"  Send them with `llz secrets push %s --yes`, from a checkout of %s\n"+
				"  (it pushes to the repo of the working directory — it takes no --repo).\n", env, repo)))
	}
	if len(d.Ahead) > 0 {
		fmt.Fprintf(&b, "\n%s %s\n", color.Cyan("ⓘ"), fmt.Sprintf(
			"%d secret(s) were pushed AFTER your local file was last edited:", len(d.Ahead)))
		for _, e := range d.Ahead {
			fmt.Fprintf(&b, "    %-30s pushed %s\n", e.Name, color.Dim(stamp(e.PushedAt)))
		}
		b.WriteString(color.Dim(
			"  Either you pushed them from here, or something else wrote them — CI's\n" +
				"  credential rotation, or another machine. If it was a rotation, YOUR copy is\n" +
				"  the stale one: a rotation revokes the value it replaced, so pushing it back\n" +
				"  would put a dead credential into CI. Check before re-pushing any of these.\n"))
	}
	if len(d.VarsDiffer) > 0 {
		fmt.Fprintf(&b, "\n%s %s\n", color.Yellow("⚠"), fmt.Sprintf(
			"%d variable(s) differ from the pushed value: %s", len(d.VarsDiffer), strings.Join(d.VarsDiffer, ", ")))
		b.WriteString(color.Dim("  Variable values ARE readable, so this one is exact, not a timestamp guess.\n"))
	}
	return b.String()
}
