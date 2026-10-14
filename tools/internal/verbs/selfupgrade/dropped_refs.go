package selfupgrade

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// dropped_refs.go reports links an upgrade REMOVED from a `managed` file that
// still resolve inside the instance.
//
// THE INCIDENT. Every documentation entry point an instance has — `docs/**`,
// `README.md`, `AGENTS.md` — is `managed`, so the overwrite pass replaces it from
// a clean render of the new release. An adopter who had extended the platform and
// linked their own subsystem from those files lost the references in one upgrade:
// six files, all of them rewritten correctly, and a playbook left present but
// unreachable. Nothing was broken. Nothing reported it. `llz lint` was green,
// because a link that no longer exists cannot be a broken link.
//
// That is the shape worth catching: not a bad file, an ABSENCE — and absences are
// only visible to something holding both versions, which the overwrite pass does
// and nothing downstream of it ever will again.
//
// WHY "STILL RESOLVES" IS THE WHOLE PREDICATE. A release legitimately drops links
// all the time — a doc that moved, a flag that went away, a section rewritten.
// Those targets are gone from the new render too, so reporting them would bury the
// signal in noise on every upgrade and the report would get ignored, which is the
// only outcome worse than not having it. A link whose target is STILL SITTING IN
// THE INSTANCE is different in kind: the template cannot have meant to drop it,
// because the template does not know that file exists. It is the adopter's.
//
// Advisory, never fatal. `llz upgrade` is mid-flight by the time this runs — the
// tree is already rewritten — and "your links moved" is not a reason to fail an
// upgrade that otherwise succeeded. It prints, and the durable fix is docs/local.md.

// mdLink matches an inline Markdown link target: [text](target).
var mdLink = regexp.MustCompile(`\[[^\]]*\]\(([^)]+)\)`)

// DroppedRef is one reference an upgrade removed whose target is still present.
type DroppedRef struct {
	File   string // the managed file that was overwritten
	Target string // the link target, as written
}

// droppedRefs compares a managed file's content before and after the overwrite and
// returns the link targets that disappeared AND still resolve on disk.
//
// instanceRoot is where relative targets resolve from; targets are resolved
// relative to the FILE's directory, which is what Markdown means by a relative
// link and what a reader clicking it would get.
func droppedRefs(rel string, before, after []byte, instanceRoot string) []DroppedRef {
	if !strings.HasSuffix(rel, ".md") {
		return nil
	}
	post := targetSet(after)
	var out []DroppedRef
	for _, t := range targets(before) {
		if post[t] {
			continue
		}
		if !stillResolves(rel, t, instanceRoot) {
			continue
		}
		out = append(out, DroppedRef{File: rel, Target: t})
	}
	return out
}

func targets(b []byte) []string {
	var out []string
	for _, m := range mdLink.FindAllStringSubmatch(string(b), -1) {
		out = append(out, strings.TrimSpace(m[1]))
	}
	return out
}

func targetSet(b []byte) map[string]bool {
	s := map[string]bool{}
	for _, t := range targets(b) {
		s[t] = true
	}
	return s
}

// stillResolves reports whether a link target names a path that exists in the
// instance. Absolute URLs, mail links and bare anchors are not instance paths and
// are skipped — a dropped `https://` link is the template's business, not a sign
// that the adopter lost something.
func stillResolves(rel, target, instanceRoot string) bool {
	if target == "" || strings.HasPrefix(target, "#") {
		return false
	}
	if i := strings.IndexAny(target, "#?"); i >= 0 {
		target = target[:i]
	}
	if target == "" {
		return false
	}
	if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
		return false
	}
	base := filepath.Dir(filepath.Join(instanceRoot, filepath.FromSlash(rel)))
	_, err := os.Stat(filepath.Join(base, filepath.FromSlash(target)))
	return err == nil
}

// FormatDroppedRefs renders the advisory. Grouped by file, because an operator
// restoring them edits one file at a time.
func FormatDroppedRefs(refs []DroppedRef) string {
	if len(refs) == 0 {
		return ""
	}
	byFile := map[string][]string{}
	for _, r := range refs {
		byFile[r.File] = append(byFile[r.File], r.Target)
	}
	files := make([]string, 0, len(byFile))
	for f := range byFile {
		files = append(files, f)
	}
	sort.Strings(files)

	var b strings.Builder
	b.WriteString("this upgrade removed link(s) to paths that still exist in this instance:\n")
	for _, f := range files {
		ts := byFile[f]
		sort.Strings(ts)
		for _, t := range ts {
			b.WriteString("      " + f + " → " + t + "\n")
		}
	}
	b.WriteString("    These files are `managed`: the template overwrites them, and it cannot know about\n" +
		"    paths it does not ship — so the targets are still there and only the references are\n" +
		"    gone. Re-add them in docs/local.md, which is `owned` and survives every upgrade.")
	return b.String()
}
