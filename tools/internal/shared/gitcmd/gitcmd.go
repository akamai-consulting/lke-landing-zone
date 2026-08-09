// Package gitcmd runs git and captures its output.
//
// IT EXISTS BECAUSE FIVE EXTENSIONS HAD WRITTEN THE SAME THREE LINES. `upgrade`,
// `lint` and `chartguard` each carried a byte-identical unexported gitOutput;
// `buildpreflight` carried an exported GitOut whose own comment said "a local
// copy: the sustain extraction took the original with stamp.go, and
// build_preflight is the only other caller" -- which had stopped being true, since
// three more packages import it now. `sustain` has the fifth variant.
//
// That comment is the interesting part. It was accurate when written and quietly
// false a few extractions later, which is the same way four Incomplete markers and
// two "package main owns this" seams went stale in this campaign. A duplicated
// helper does not announce that it has acquired more callers.
//
// The campaign's own rule -- N callers of a private helper means a package -- was
// applied a dozen times inside package main (guardwalk, kubectlprobe, pathglob,
// shquote, color, cigate) and never once between extensions. This is that rule,
// applied where the duplication actually was.
package gitcmd

import (
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

// Output runs git in dir and returns trimmed stdout.
//
// `-C dir` rather than a chdir: these callers run inside a CLI process that has
// other work to do, and a process-wide chdir is not something a helper should do
// on their behalf.
func Output(dir string, args ...string) (string, error) {
	out, err := kubectlprobe.Exec("git", append([]string{"-C", dir}, args...)...)
	return strings.TrimSpace(string(out)), err
}

// Out runs git in the CURRENT directory and returns trimmed stdout, "" on
// failure. The error-swallowing shape is deliberate and is what build-preflight's
// copy did: every caller is reading a stamp (a ref, a sha, a remote) where "not a
// git checkout" and "no such ref" are both simply "unknown", and a returned error
// would be discarded at all of them.
func Out(args ...string) string {
	out, err := kubectlprobe.Exec("git", args...)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
