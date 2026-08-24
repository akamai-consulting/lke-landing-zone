package dependabotcoverage

// guard.go implements `llz ci dependabot-coverage` — the gate that keeps
// .github/dependabot.yml pointed at every dependency manifest this tree carries.
//
// WHY: three pin sets went unscanned at once, and every one of them looked
// correct at its own site. `actions/setup-go` was SHA-pinned, in the file the
// `setup-go-sole-site` guard requires it to be in — and invisible to Dependabot,
// because for the github-actions ecosystem `directory: "/"` means
// `.github/workflows` plus a ROOT-level action.yml and nothing else, so
// consolidating the pin into a composite action moved it out of scope in the same
// stroke. `git log --author=dependabot -- .github/actions/` was empty and the pin
// had gone a major version stale. The Dockerfile bases and the modules' provider
// constraints had never been listed at all.
//
// NONE OF THAT IS VISIBLE IN A DIFF. A config that omits an ecosystem is
// well-formed, and Dependabot reports nothing about a directory it was never
// asked to look at: the failure is silence, and silence is what a working
// Dependabot also looks like. The relation between the tree and the config is the
// only place it shows, which is the same shape as version-pins (a restatement
// equal to an authority) and setup-go-sole-site (a count of sites held at one).
//
// THE EXCLUSION FILE IS PART OF THE GATE, NOT AN ESCAPE HATCH. Some manifests
// genuinely must not be scanned — the delivered scaffold's workflows are
// digest-locked, so a Dependabot bump would break `managed-lock-check` and land
// red forever. Recording those as data with a reason means the decision is
// reviewable, and a stale one fails: an exclusion that no longer matches anything
// is removed by this gate rather than accumulating into a file nobody trusts.

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
)

const (
	configPath    = ".github/dependabot.yml"
	exclusionPath = ".dependabot-coverage.yaml"

	// minReason is the length below which a "reason" is a placeholder rather than
	// an argument. The number is arbitrary; requiring one at all is not. An
	// exclusion is a decision to leave a dependency unwatched, and the next person
	// to read it needs to know whether the decision still holds.
	minReason = 30
)

// config is the subset of dependabot.yml this gate reads.
type config struct {
	Updates []update `json:"updates"`
}

// update is one `updates:` entry. Both directory forms are read because they mean
// different things: `directory` takes one literal path, `directories` takes a
// list and is the only form that accepts a glob.
type update struct {
	Ecosystem   string   `json:"package-ecosystem"`
	Directory   string   `json:"directory"`
	Directories []string `json:"directories"`
}

// exclusionFile is .dependabot-coverage.yaml.
type exclusionFile struct {
	Exclusions []exclusion `json:"exclusions"`
}

// exclusion is one manifest deliberately left unscanned. Path matches the
// manifest's directory or any directory beneath it, so one entry can cover a
// subtree (the six vendored composite actions, say) without listing each.
type exclusion struct {
	Ecosystem string `json:"ecosystem"`
	Path      string `json:"path"`
	Reason    string `json:"reason"`
}

// Run fails when a dependency manifest is neither scanned by dependabot.yml nor
// excluded with a reason — and when the config or the exclusion file claims
// something the tree no longer contains.
func Run(root string, out, errOut io.Writer) error {
	repo := capability.RepoForGate(Extension(), root)

	cfg, err := loadConfig(repo)
	if err != nil {
		return err
	}
	excl, err := loadExclusions(repo)
	if err != nil {
		return err
	}
	manifests, err := discover(repo)
	if err != nil {
		return fmt.Errorf("dependabot-coverage: %w", err)
	}
	// FAIL CLOSED ON AN EMPTY CORPUS. A walk that found no manifest at all is what
	// a wrong --root looks like, and "0 uncovered" over it would launder a
	// mis-invocation into a green check.
	if len(manifests) == 0 {
		return fmt.Errorf("dependabot-coverage: no dependency manifests found under %s — refusing to pass vacuously", root)
	}

	var uncovered []manifest
	scanned := 0
	entryHits := make([]int, len(cfg.Updates))
	exclHits := make([]int, len(excl.Exclusions))

	for _, m := range manifests {
		if i, ok := coveringEntry(cfg, m); ok {
			entryHits[i]++
			scanned++
			continue
		}
		if i, ok := matchingExclusion(excl, m); ok {
			exclHits[i]++
			continue
		}
		uncovered = append(uncovered, m)
	}

	var problems []string
	for _, m := range uncovered {
		problems = append(problems, fmt.Sprintf("%s manifest %s is scanned by nothing (found via %s)",
			m.ecosystem, dirLabel(m.dir), m.evidence))
	}
	// A STALE ENTRY IS COVERAGE THAT SCANS NOTHING, and it reads exactly like
	// coverage that works. Dependabot does not fail an update run over a directory
	// that has stopped existing, so without this the config decays into a list of
	// places the repo used to keep its dependencies.
	for i, u := range cfg.Updates {
		if entryHits[i] == 0 {
			problems = append(problems, fmt.Sprintf("%s: %s names %s, where no manifest of that ecosystem exists",
				configPath, u.Ecosystem, strings.Join(quoteAll(u.dirs()), ", ")))
		}
	}
	for i, e := range excl.Exclusions {
		if exclHits[i] == 0 {
			problems = append(problems, fmt.Sprintf("%s: the %s exclusion for %s matches nothing — delete it",
				exclusionPath, e.Ecosystem, e.Path))
		}
	}

	if len(problems) == 0 {
		fmt.Fprintf(out, "dependabot-coverage: OK — %d manifest(s): %d scanned by %s, %d excluded by %s\n",
			len(manifests), scanned, configPath, len(manifests)-scanned, exclusionPath)
		return nil
	}

	for _, m := range uncovered {
		fmt.Fprintf(errOut, "::error file=%s::%s manifest in %s is not scanned by any %s entry\n",
			m.evidence, m.ecosystem, dirLabel(m.dir), configPath)
	}
	fmt.Fprintf(errOut, "\n%s dependabot-coverage: %d problem(s)\n", color.Red("✗"), len(problems))
	for _, p := range problems {
		fmt.Fprintf(errOut, "    %s\n", p)
	}
	if len(uncovered) > 0 {
		fmt.Fprintf(errOut, "\nEither scan them — add to %s:\n\n%s\n", configPath, snippet(uncovered))
		fmt.Fprintf(errOut, "or record the decision not to, in %s:\n\n"+
			"  exclusions:\n"+
			"    - ecosystem: %s\n"+
			"      path: %s\n"+
			"      reason: why this manifest must NOT be bumped automatically\n\n",
			exclusionPath, uncovered[0].ecosystem, dirLabel(uncovered[0].dir))
	}
	return fmt.Errorf("dependabot-coverage: %d problem(s)", len(problems))
}

// coveringEntry returns the index of the first update entry that scans m.
func coveringEntry(cfg config, m manifest) (int, bool) {
	for i, u := range cfg.Updates {
		if covers(u, m) {
			return i, true
		}
	}
	return 0, false
}

// matchingExclusion returns the index of the first exclusion covering m. A path
// matches the manifest's own directory or any directory beneath it.
func matchingExclusion(f exclusionFile, m manifest) (int, bool) {
	for i, e := range f.Exclusions {
		if e.Ecosystem != m.ecosystem {
			continue
		}
		p := normalizeDir(e.Path)
		if m.dir == p || strings.HasPrefix(m.dir, p+"/") {
			return i, true
		}
	}
	return 0, false
}

// loadConfig reads dependabot.yml and fails closed on anything it cannot trust.
func loadConfig(repo capability.Repo) (config, error) {
	data, err := repo.ReadFile(filepath.FromSlash(configPath))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return config{}, fmt.Errorf("dependabot-coverage: %s does not exist — this gate cannot report coverage against a config that is not there", configPath)
		}
		return config{}, fmt.Errorf("dependabot-coverage: read %s: %w", configPath, err)
	}
	var cfg config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return config{}, fmt.Errorf("dependabot-coverage: parse %s: %w", configPath, err)
	}
	if len(cfg.Updates) == 0 {
		return config{}, fmt.Errorf("dependabot-coverage: %s declares no `updates:` entries", configPath)
	}
	for i, u := range cfg.Updates {
		if u.Ecosystem == "" {
			return config{}, fmt.Errorf("dependabot-coverage: %s entry %d has no package-ecosystem", configPath, i+1)
		}
		if len(u.dirs()) == 0 {
			return config{}, fmt.Errorf("dependabot-coverage: %s entry %d (%s) declares neither directory nor directories",
				configPath, i+1, u.Ecosystem)
		}
	}
	return cfg, nil
}

// loadExclusions reads .dependabot-coverage.yaml. Its ABSENCE is not a failure —
// a tree whose every manifest is scanned needs no exclusions — but a malformed or
// reasonless entry is, because that is the shape of an exclusion added to make a
// red gate green.
func loadExclusions(repo capability.Repo) (exclusionFile, error) {
	data, err := repo.ReadFile(filepath.FromSlash(exclusionPath))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return exclusionFile{}, nil
		}
		return exclusionFile{}, fmt.Errorf("dependabot-coverage: read %s: %w", exclusionPath, err)
	}
	var f exclusionFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return exclusionFile{}, fmt.Errorf("dependabot-coverage: parse %s: %w", exclusionPath, err)
	}
	for i, e := range f.Exclusions {
		switch {
		case e.Ecosystem == "":
			return exclusionFile{}, fmt.Errorf("dependabot-coverage: %s exclusion %d has no ecosystem", exclusionPath, i+1)
		case e.Path == "":
			return exclusionFile{}, fmt.Errorf("dependabot-coverage: %s exclusion %d has no path", exclusionPath, i+1)
		case len(strings.TrimSpace(e.Reason)) < minReason:
			return exclusionFile{}, fmt.Errorf("dependabot-coverage: %s exclusion %d (%s %s) needs a reason of at least %d characters.\n"+
				"  An exclusion is a decision to leave a dependency unwatched; the next reader has to be able to tell whether it still holds",
				exclusionPath, i+1, e.Ecosystem, e.Path, minReason)
		}
	}
	return f, nil
}

// snippet renders the dependabot.yml entries that would cover the uncovered
// manifests — one per ecosystem, directories sorted.
func snippet(ms []manifest) string {
	byEco := map[string][]string{}
	for _, m := range ms {
		byEco[m.ecosystem] = append(byEco[m.ecosystem], "/"+m.dir)
	}
	ecos := make([]string, 0, len(byEco))
	for e := range byEco {
		ecos = append(ecos, e)
	}
	sort.Strings(ecos)

	var b strings.Builder
	for _, e := range ecos {
		dirs := byEco[e]
		sort.Strings(dirs)
		fmt.Fprintf(&b, "  - package-ecosystem: %q\n    directories: [%s]\n    schedule:\n      interval: \"weekly\"\n",
			e, strings.Join(quoteAll(dirs), ", "))
	}
	return b.String()
}

// dirLabel prints the repo root as "/" rather than as the empty string.
func dirLabel(dir string) string {
	if dir == "" {
		return "/"
	}
	return dir
}

func quoteAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, fmt.Sprintf("%q", "/"+strings.TrimPrefix(s, "/")))
	}
	return out
}
