package main

// instance_root.go — the "am I standing in the right directory?" guard.
//
// `llz env add` resolves every path it writes relative to the CWD (instanceLayout
// returns bare `terraform-iac-bootstrap` / `apl-values` for the instance layout),
// so run from the wrong directory it does not fail — it succeeds, authoring a
// complete landingzone.yaml + environments/<env>.yaml + apl-values/<env>/ tree
// wherever the operator happened to be standing. Nothing downstream ever reads
// it: CI builds from the instance repo.
//
// That is the quickstart's most reachable mistake, because the flow prints
// `cd my-instance` as its own line — a copy/paste that skips it, or a second
// terminal tab, lands the operator one directory up with no signal beyond a
// warning line scrolled off the top of a 20-line render log. So make the CWD a
// precondition, and point at the instance directory we can see from here.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/color"
)

// instanceRootMarkers identify an instance checkout. Any ONE is enough:
// .copier-answers.yml is present from `llz new` onwards, landingzone.yaml from
// the first `env add`, and terraform-iac-bootstrap/ in every rendered instance
// including the pre-spec (legacy, hand-authored tfvars) ones that have neither.
var instanceRootMarkers = []string{
	".copier-answers.yml",
	clusterspec.LandingZoneFile,
	"terraform-iac-bootstrap",
}

// isInstanceRoot reports whether dir is an instance checkout — or the template
// repo itself, whose instance-template/ subtree is the layout instanceLayout
// resolves against (the scaffold-render checks in template CI run there).
func isInstanceRoot(dir string) bool {
	for _, m := range instanceRootMarkers {
		if _, err := os.Stat(filepath.Join(dir, m)); err == nil {
			return true
		}
	}
	fi, err := os.Stat(filepath.Join(dir, "instance-template", "terraform-iac-bootstrap"))
	return err == nil && fi.IsDir()
}

// instanceSubdirs returns the immediate children of dir that are themselves
// instance checkouts, sorted. This is the recovery hint: the operator who forgot
// `cd my-instance` is, almost always, in its parent.
func instanceSubdirs(dir string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if isInstanceRoot(filepath.Join(dir, e.Name())) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// enclosingInstanceRoot walks up from the CWD and returns the first ancestor that
// is an instance checkout, "" if there is none. This is the other half of the
// mistake: `llz env add` run from INSIDE an instance but a level or two down
// (terraform-iac-bootstrap/, apl-values/<env>/) is just as wrong and just as
// silent, and telling that operator to run `llz new` would be nonsense.
func enclosingInstanceRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		parent := filepath.Dir(dir)
		if parent == dir { // filesystem root
			return ""
		}
		dir = parent
		if isInstanceRoot(dir) {
			return dir
		}
	}
}

// requireInstanceRoot fails when the CWD is not an instance checkout, naming the
// candidate it can see — below, or above — so the fix is one `cd` away. op names
// the command in the message ("llz env add").
func requireInstanceRoot(op string) error {
	if isInstanceRoot(".") {
		return nil
	}
	cwd, _ := os.Getwd()
	var b strings.Builder
	fmt.Fprintf(&b, "%s must run from your instance repo root, and %s is not one\n", op, cwd)
	fmt.Fprintf(&b, "  (no %s here). Running it here would author a spec tree that no build ever reads —\n",
		strings.Join(instanceRootMarkers, " / "))
	b.WriteString("  CI builds from the instance repo, not from this directory.\n")
	switch cands := instanceSubdirs("."); {
	case len(cands) > 0:
		b.WriteString("  • your instance is right here — cd into it first:\n")
		for _, c := range cands {
			fmt.Fprintf(&b, "      %s\n", color.Cyan("cd "+c))
		}
	case enclosingInstanceRoot() != "":
		fmt.Fprintf(&b, "  • you are inside an instance, below its root — go up to it:\n      %s\n",
			color.Cyan("cd "+enclosingInstanceRoot()))
	default:
		fmt.Fprintf(&b, "  • already scaffolded one? cd into it:      %s\n", color.Cyan("cd <instance-dir>"))
		fmt.Fprintf(&b, "  • not yet? scaffold + publish it first:    %s\n", color.Cyan("llz new <instance-dir> --push --yes"))
	}
	return errors.New(strings.TrimRight(b.String(), "\n"))
}
