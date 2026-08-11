package selfupgrade

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/answers"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/manifest"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/proc"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/templateid"
)

const copierAnswersPath = ".copier-answers.yml"

// UpgradeSnapshot protects operator-owned files from Copier's generic 3-way
// merge. The manifest is the authority: owned files are restored after Copier
// runs, while managed files are overwritten from a clean render of the target
// template Version.
type UpgradeSnapshot struct {
	dir   string
	files map[string]os.FileMode
}

func SnapshotUpgradeOwned(m manifest.Manifest) (UpgradeSnapshot, error) {
	files, err := upgradeWorktreeFiles()
	if err != nil {
		return UpgradeSnapshot{}, err
	}
	s := UpgradeSnapshot{files: map[string]os.FileMode{}}
	for _, rel := range files {
		rel = filepath.ToSlash(rel)
		if !upgradeProtectsOwned(m.Classify(rel), rel) {
			continue
		}
		info, err := os.Stat(filepath.FromSlash(rel))
		if err != nil || info.IsDir() {
			continue
		}
		if s.dir == "" {
			dir, err := os.MkdirTemp("", "llz-upgrade-owned-*")
			if err != nil {
				return UpgradeSnapshot{}, err
			}
			s.dir = dir
		}
		if err := copyUpgradeFile(filepath.FromSlash(rel), filepath.Join(s.dir, filepath.FromSlash(rel)), info.Mode().Perm()); err != nil {
			return UpgradeSnapshot{}, err
		}
		s.files[rel] = info.Mode().Perm()
	}
	return s, nil
}

func (s UpgradeSnapshot) Cleanup() {
	if s.dir != "" {
		_ = os.RemoveAll(s.dir)
	}
}

func (s UpgradeSnapshot) restore() error {
	if s.dir == "" {
		return nil
	}
	var files []string
	for rel := range s.files {
		files = append(files, rel)
	}
	sort.Strings(files)
	for _, rel := range files {
		if err := copyUpgradeFile(filepath.Join(s.dir, filepath.FromSlash(rel)), filepath.FromSlash(rel), s.files[rel]); err != nil {
			return err
		}
	}
	return nil
}

// upgradeProtectsOwned reports whether a file must be snapshotted before copier
// runs and put back after. The class table is the authority (manifest.UpgradeRestore);
// the answers tracker is copier's own bookkeeping and is never restored.
func upgradeProtectsOwned(class, rel string) bool {
	c, ok := manifest.LookupClass(class)
	return ok && c.Upgrade == manifest.UpgradeRestore && rel != copierAnswersPath
}

func upgradeWorktreeFiles() ([]string, error) {
	tracked, trackedOK, err := gitFileList("ls-files")
	if err != nil {
		return nil, err
	}
	if !trackedOK {
		return walkUpgradeFiles(".")
	}
	untracked, _, err := gitFileList("ls-files", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var files []string
	for _, rel := range append(tracked, untracked...) {
		rel = filepath.ToSlash(strings.TrimSpace(rel))
		if rel == "" || seen[rel] {
			continue
		}
		seen[rel] = true
		files = append(files, rel)
	}
	sort.Strings(files)
	return files, nil
}

func gitFileList(args ...string) ([]string, bool, error) {
	out, err := execOutput("git", args...)
	if err != nil {
		if len(args) == 1 && args[0] == "ls-files" {
			return nil, false, nil
		}
		return nil, false, err
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			files = append(files, filepath.ToSlash(line))
		}
	}
	return files, true, nil
}

func walkUpgradeFiles(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".terraform", ".llz":
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

// ManifestPolicy is a clean render of the target ref, already on disk, waiting to
// be applied to the instance.
//
// IT IS TWO STEPS BECAUSE THE ORDER IS THE SAFETY PROPERTY. The render is the
// failure-prone half: it shells out to copier, which runs the template's `_tasks`
// — arbitrary shell, `cp -a <src>/docs/.` among them, which exits non-zero on a
// fork whose template carries no docs/. The apply is the destructive half.
// Rendering INSIDE the apply put the fragile step after `copier update` had
// already rewritten the instance, so a task that failed left a half-upgraded tree:
// copier's merge applied, the `managed` overwrite and `.template-removals` not,
// and no rollback. `llz upgrade` now renders first and only touches the instance
// once it holds a scaffold it can finish with.
//
// Reading .copier-answers.yml BEFORE the update rather than after is not a
// behaviour change: copierRenderArgv consumes exactly _src_path, upstream_org and
// instance_repo, which are the operator's answers rather than the pin, and
// `llz ci upgrade-test`'s answers-preserved check is the assertion that an upgrade
// does not move them. llz_version is passed explicitly as ref.
type ManifestPolicy struct {
	dryRun    bool
	ref       string
	cleanRoot string
	cleanup   func()
}

// PrepareManifestPolicy renders the target ref's clean scaffold. Call it BEFORE
// mutating the instance; call Cleanup when done regardless of outcome.
func PrepareManifestPolicy(dryRun bool, ref string) (*ManifestPolicy, error) {
	p := &ManifestPolicy{dryRun: dryRun, ref: ref, cleanup: func() {}}
	if dryRun {
		return p, nil
	}
	cleanRoot, cleanup, err := renderUpgradeScaffold(ref)
	if err != nil {
		return nil, err
	}
	p.cleanRoot, p.cleanup = cleanRoot, cleanup
	return p, nil
}

func (p *ManifestPolicy) Cleanup() {
	if p != nil && p.cleanup != nil {
		p.cleanup()
	}
}

// Apply restores the `owned` files copier overwrote, then overwrites every
// `managed` file from the pre-rendered scaffold.
func (p *ManifestPolicy) Apply(before UpgradeSnapshot) error {
	if p.dryRun {
		fmt.Fprintf(os.Stderr, "→ (dry-run) would restore %d owned file(s) after copier update\n", len(before.files))
		fmt.Fprintln(os.Stderr, "→ (dry-run) would overwrite managed files from a clean target-template render")
		return nil
	}
	if err := before.restore(); err != nil {
		return fmt.Errorf("restore owned files: %w", err)
	}
	count, err := overwriteManagedFromScaffold(p.cleanRoot)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "%s restored %d owned file(s); overwrote %d managed file(s) from %s\n",
		color.Dim("→"), len(before.files), count, p.ref)
	return nil
}

func renderUpgradeScaffold(ref string) (string, func(), error) {
	a, err := answers.Read(".")
	if err != nil {
		return "", nil, err
	}
	tmp, err := os.MkdirTemp("", "llz-upgrade-render-*")
	if err != nil {
		return "", nil, err
	}
	Cleanup := func() { _ = os.RemoveAll(tmp) }
	dst := filepath.Join(tmp, "scaffold")
	// THE GUARANTEE THIS RENDER DEPENDS ON, held where the dependency is rather
	// than in a caller. copierRenderArgv deliberately no longer passes
	// --skip-tasks, so this render's correctness now rests on copier's `_tasks`
	// actually running — and they invoke `llz` BY NAME, falling back to a warning
	// when `command -v llz` comes up empty. Taking that fallback here does not
	// merely skip the root-link repoint: it leaves the render's docs/ UNPRUNED, and
	// the overwrite pass below then copies the whole template docs tree into the
	// instance. proc.SelfOnPATH publishes the running binary under the name the
	// tasks look up, so the render cannot degrade just because the operator invoked
	// llz by path. See its doc comment for why $(dirname $self) is not enough.
	restorePATH, err := selfOnPATH("llz")
	if err != nil {
		Cleanup()
		return "", nil, err
	}
	defer restorePATH()
	argv := copierRenderArgv(a, ref, dst)
	if err := runProc(argv, ""); err != nil {
		Cleanup()
		return "", nil, fmt.Errorf("render target scaffold for manifest policy: %w", err)
	}
	return dst, Cleanup, nil
}

// Seams. renderUpgradeScaffold's contract is an ORDERING — `llz` is resolvable
// before copier starts — and an ordering is only provable by observing both
// halves. Without these a test would have to run a real copier render over the
// network to learn whether the arming happened at all.
var (
	selfOnPATH = proc.SelfOnPATH
	runProc    = proc.Run
)

func copierRenderArgv(a *answers.File, ref, dst string) []string {
	source := "gh:" + UpdateRepo()
	upstreamOrg := templateid.DefaultOrg
	instanceRepo := "your-org/your-instance-repo"
	if a != nil {
		if a.SrcPath != "" {
			source = a.SrcPath
		}
		if a.UpstreamOrg != "" {
			upstreamOrg = a.UpstreamOrg
		}
		if a.InstanceRepo != "" {
			instanceRepo = a.InstanceRepo
		}
	}
	// NO --skip-tasks. This render is the SOURCE the overwrite pass copies every
	// `managed` file from, so it has to be the same artifact a fresh `llz new` at
	// this ref produces — and copier's `_tasks` are part of producing it. They
	// deliver docs/, prune it to the operator set, and repoint the root Markdown
	// links that target template-only paths.
	//
	// It skipped them, so the overwrite pass sourced AGENTS.md from a render where
	// that repoint had never run, and put the pre-repoint copy back over the correct
	// one `copier update` had just produced. Every upgraded instance ended up with
	// AGENTS.md pointing at a relative docs/adopter-guide.md that deliver-docs
	// prunes out of an instance — a dead link that a freshly scaffolded instance
	// never had, and that survived every subsequent upgrade because the same pass
	// re-applied it each time. `llz ci upgrade-test`'s converges-with-fresh check is
	// what compares the two instances; it found this on its first run.
	return []string{"copier", "copy", "--trust", "--force", "--vcs-ref", ref,
		"--data", "upstream_org=" + upstreamOrg,
		"--data", "instance_repo=" + instanceRepo,
		"--data", "llz_version=" + ref,
		source, dst}
}

func overwriteManagedFromScaffold(cleanRoot string) (int, error) {
	m, err := manifest.Load(cleanRoot)
	if err != nil {
		return 0, err
	}
	files, err := manifest.ScaffoldFiles(cleanRoot)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, rel := range files {
		c, ok := manifest.LookupClass(m.Classify(rel))
		if !ok || c.Upgrade != manifest.UpgradeOverwrite {
			continue
		}
		src := filepath.Join(cleanRoot, filepath.FromSlash(rel))
		info, err := os.Stat(src)
		if err != nil || info.IsDir() {
			continue
		}
		if err := copyUpgradeFile(src, filepath.FromSlash(rel), info.Mode().Perm()); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

func copyUpgradeFile(src, dst string, mode os.FileMode) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if mode == 0 {
		mode = 0o644
	}
	return os.WriteFile(dst, b, mode)
}

// NOTE: the post-upgrade conflict-marker gate lives in runUpgrade as
// upgradeConflictFiles() — it scans only what copier just changed (rather than
// every tracked file) and shares the conflictMarkerLines predicate with
// `llz lint`, so the upgrade gate and the lint gate can't disagree.
