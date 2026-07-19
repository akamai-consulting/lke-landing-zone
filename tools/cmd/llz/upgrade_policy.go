package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const copierAnswersPath = ".copier-answers.yml"

// upgradeSnapshot protects operator-owned files from Copier's generic 3-way
// merge. The manifest is the authority: owned files are restored after Copier
// runs, while managed files are overwritten from a clean render of the target
// template version.
type upgradeSnapshot struct {
	dir   string
	files map[string]os.FileMode
}

func snapshotUpgradeOwned(m templateManifest) (upgradeSnapshot, error) {
	files, err := upgradeWorktreeFiles()
	if err != nil {
		return upgradeSnapshot{}, err
	}
	s := upgradeSnapshot{files: map[string]os.FileMode{}}
	for _, rel := range files {
		rel = filepath.ToSlash(rel)
		if !upgradeProtectsOwned(m.classify(rel), rel) {
			continue
		}
		info, err := os.Stat(filepath.FromSlash(rel))
		if err != nil || info.IsDir() {
			continue
		}
		if s.dir == "" {
			dir, err := os.MkdirTemp("", "llz-upgrade-owned-*")
			if err != nil {
				return upgradeSnapshot{}, err
			}
			s.dir = dir
		}
		if err := copyUpgradeFile(filepath.FromSlash(rel), filepath.Join(s.dir, filepath.FromSlash(rel)), info.Mode().Perm()); err != nil {
			return upgradeSnapshot{}, err
		}
		s.files[rel] = info.Mode().Perm()
	}
	return s, nil
}

func (s upgradeSnapshot) cleanup() {
	if s.dir != "" {
		_ = os.RemoveAll(s.dir)
	}
}

func (s upgradeSnapshot) restore() error {
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

func upgradeProtectsOwned(class, rel string) bool {
	return class == "owned" && rel != copierAnswersPath
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

func applyUpgradeManifestPolicy(g globalOpts, ref string, before upgradeSnapshot) error {
	if g.dryRun {
		fmt.Fprintf(os.Stderr, "→ (dry-run) would restore %d owned file(s) after copier update\n", len(before.files))
		fmt.Fprintln(os.Stderr, "→ (dry-run) would overwrite managed files from a clean target-template render")
		return nil
	}
	if err := before.restore(); err != nil {
		return fmt.Errorf("restore owned files: %w", err)
	}
	cleanRoot, cleanup, err := renderUpgradeScaffold(ref)
	if err != nil {
		return err
	}
	defer cleanup()
	count, err := overwriteManagedFromScaffold(cleanRoot)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "%s restored %d owned file(s); overwrote %d managed file(s) from %s\n",
		dim("→"), len(before.files), count, ref)
	return nil
}

func renderUpgradeScaffold(ref string) (string, func(), error) {
	a, err := readAnswers(".")
	if err != nil {
		return "", nil, err
	}
	tmp, err := os.MkdirTemp("", "llz-upgrade-render-*")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(tmp) }
	dst := filepath.Join(tmp, "scaffold")
	argv := copierRenderArgv(a, ref, dst)
	if err := execArgv(argv, ""); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("render target scaffold for manifest policy: %w", err)
	}
	return dst, cleanup, nil
}

func copierRenderArgv(a *answers, ref, dst string) []string {
	source := "gh:" + updateRepo()
	upstreamOrg := defaultTemplateOrg
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
	return []string{"copier", "copy", "--trust", "--force", "--skip-tasks", "--vcs-ref", ref,
		"--data", "upstream_org=" + upstreamOrg,
		"--data", "instance_repo=" + instanceRepo,
		"--data", "llz_version=" + ref,
		source, dst}
}

func overwriteManagedFromScaffold(cleanRoot string) (int, error) {
	m, err := loadTemplateManifest(cleanRoot)
	if err != nil {
		return 0, err
	}
	files, err := scaffoldManifestFiles(cleanRoot)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, rel := range files {
		if m.classify(rel) != "managed" {
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
