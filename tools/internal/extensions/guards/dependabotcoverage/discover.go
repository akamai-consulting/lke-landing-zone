package dependabotcoverage

// discover.go — what a dependency manifest is, where Dependabot looks for one,
// and whether a config entry reaches it.
//
// THE TWO HALVES ARE DELIBERATELY SEPARATE. Discovery answers "what does this
// tree contain"; coverage answers "what does the config say it scans". A guard
// that derived either from the other would be checking the config against itself
// — which is how the omission this gate exists for survived in the first place:
// every `uses:` was SHA-pinned, every entry in dependabot.yml was valid, and the
// only defect was that no entry named the directory one of the pins had moved to.

import (
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
)

// manifest is one dependency-manifest location: the directory Dependabot would
// have to be pointed at, plus the file that proves something lives there.
type manifest struct {
	ecosystem string // a dependabot `package-ecosystem` value
	dir       string // slash path of the directory an entry must name; "" is the repo root
	evidence  string // slash path of the file that made this a manifest
}

// skipDirs are never walked. `.git` and the build/vendor caches hold copies of
// files that are not this tree's to scan; `.instance-test` is a RENDERED instance
// (`make instance-test` writes it), so counting its manifests would demand config
// entries for a directory that exists only after a test run.
var skipDirs = map[string]bool{
	".git":           true,
	".instance-test": true,
	"node_modules":   true,
	"vendor":         true,
	"testdata":       true,
}

// discover walks the tree and returns every manifest in it, sorted.
//
// NESTED CHECKOUTS ARE SKIPPED WHOLE. A git worktree or a vendored clone under
// the repo root carries a complete copy of somebody's tree, and its manifests are
// not this config's business — reporting them would make the gate fail on a
// developer's machine for a directory CI cannot see. A directory holding a `.git`
// entry is that, whether the entry is a directory (clone) or a file (worktree).
func discover(repo capability.Repo) ([]manifest, error) {
	seen := map[string]manifest{}
	add := func(m manifest) {
		key := m.ecosystem + "\x00" + m.dir
		if _, dup := seen[key]; !dup {
			seen[key] = m
		}
	}

	err := repo.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		slash := filepath.ToSlash(p)
		if d.IsDir() {
			if slash == "." {
				return nil
			}
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			if _, err := repo.Stat(filepath.Join(p, ".git")); err == nil {
				return filepath.SkipDir
			}
			return nil
		}
		for _, m := range detect(repo, slash) {
			add(m)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk: %w", err)
	}

	out := make([]manifest, 0, len(seen))
	for _, m := range seen {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].dir != out[j].dir {
			return out[i].dir < out[j].dir
		}
		return out[i].ecosystem < out[j].ecosystem
	})
	return out, nil
}

// detect classifies one file. It returns the manifests that file proves, which is
// zero for almost every file in the tree.
func detect(repo capability.Repo, file string) []manifest {
	dir, name := path.Split(file)
	dir = strings.TrimSuffix(dir, "/")

	switch {
	case name == "go.mod":
		return []manifest{{ecosystem: "gomod", dir: dir, evidence: file}}

	case name == "Dockerfile" || strings.HasPrefix(name, "Dockerfile."):
		return []manifest{{ecosystem: "docker", dir: dir, evidence: file}}

	// The devcontainers ecosystem is pointed at the directory CONTAINING
	// `.devcontainer`, not at `.devcontainer` itself — so the manifest dir is one
	// level up from the file, and for a repo-root dev container it is "".
	case name == "devcontainer.json" && path.Base(dir) == ".devcontainer":
		return []manifest{{ecosystem: "devcontainers", dir: path.Dir(dir), evidence: file}}

	case name == "action.yml" || name == "action.yaml":
		return []manifest{{ecosystem: "github-actions", dir: dir, evidence: file}}

	case strings.HasSuffix(dir, ".github/workflows") &&
		(strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml")):
		return []manifest{{ecosystem: "github-actions", dir: dir, evidence: file}}

	// A .tf file is only a manifest when it DECLARES a provider. Terraform code
	// that merely uses one has no version to bump, and reporting every root would
	// bury the four directories that actually pin something.
	case strings.HasSuffix(name, ".tf"):
		body, err := repo.ReadFile(filepath.FromSlash(file))
		if err == nil && strings.Contains(string(body), "required_providers") {
			return []manifest{{ecosystem: "terraform", dir: dir, evidence: file}}
		}
	}
	return nil
}

// normalizeDir turns a dependabot `directory` / `directories` value into the form
// discover() produces: no leading slash, no trailing slash, "" for the root.
func normalizeDir(d string) string {
	return strings.Trim(strings.TrimSpace(d), "/")
}

// dirs returns every directory value an update entry declares, normalized.
func (u update) dirs() []string {
	var out []string
	if u.Directory != "" {
		out = append(out, normalizeDir(u.Directory))
	}
	for _, d := range u.Directories {
		out = append(out, normalizeDir(d))
	}
	return out
}

// covers reports whether update entry u scans manifest m, per Dependabot's own
// scoping rules rather than a naive prefix match.
func covers(u update, m manifest) bool {
	if u.Ecosystem != m.ecosystem {
		return false
	}
	for _, d := range u.dirs() {
		// THE ONE ECOSYSTEM WITH AN IMPLICIT DIRECTORY. For github-actions, `/`
		// means `.github/workflows` PLUS a root-level action.yml — and nothing
		// else. Reading it as "the whole repo" is exactly the mistake that left a
		// composite action in `.github/actions/` unscanned while the config looked
		// like it covered everything.
		if m.ecosystem == "github-actions" && d == "" {
			if m.dir == ".github/workflows" || m.dir == "" {
				return true
			}
			continue
		}
		if matchDir(d, m.dir) {
			return true
		}
	}
	return false
}

// matchDir matches a dependabot directory pattern against a directory. `*` stays
// inside one path segment and `**` crosses them, which is how the `directories`
// key documents its globs; the `directory` key takes no glob at all, so a literal
// simply compares equal.
func matchDir(pattern, dir string) bool {
	if pattern == dir {
		return true
	}
	if !strings.ContainsAny(pattern, "*?[") {
		return false
	}
	return matchSegments(strings.Split(pattern, "/"), strings.Split(dir, "/"))
}

func matchSegments(pat, seg []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			if len(pat) == 1 {
				return true
			}
			for i := 0; i <= len(seg); i++ {
				if matchSegments(pat[1:], seg[i:]) {
					return true
				}
			}
			return false
		}
		if len(seg) == 0 {
			return false
		}
		ok, err := path.Match(pat[0], seg[0])
		if err != nil || !ok {
			return false
		}
		pat, seg = pat[1:], seg[1:]
	}
	return len(seg) == 0
}
