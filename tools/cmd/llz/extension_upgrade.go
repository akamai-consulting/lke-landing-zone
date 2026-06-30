package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	yamlv3 "gopkg.in/yaml.v3"
	"sigs.k8s.io/yaml"
)

type extMigration func(m extManifest, dir string) (extManifest, []string, error)

var extMigrations = []extMigration{
	migrateAddKind,        // v1 → v2: manifests predate the `kind` ceiling selector
	migrateStageUniversal, // v2 → v3: an empty stage was the implicit cross-cutting marker; make it explicit
}

// migrateStageUniversal stamps the explicit `universal` stage on a v2 manifest that
// left stage empty. Empty used to mean "cross-cutting, platform-gated"; v3 makes that
// an authored value (lintManifest now rejects an empty stage), so the migration writes
// what the manifest already meant. A manifest that already declares a delivery stage is
// left untouched.
func migrateStageUniversal(m extManifest, _ string) (extManifest, []string, error) {
	if m.Stage != "" {
		return m, nil, nil
	}
	m.Stage = StageUniversal
	return m, []string{"set stage: universal (the explicit cross-cutting marker; empty stage is no longer valid)"}, nil
}

// migrateAddKind infers and stamps `kind` on a v1 manifest that predates it: an
// extension carrying its own logic (internal/ or any *_test.go) is `check`,
// otherwise it only shells external tools and is `tool`.
func migrateAddKind(m extManifest, dir string) (extManifest, []string, error) {
	if m.Kind != "" {
		return m, nil, nil
	}
	if hasGoTests(dir) || dirExists(filepath.Join(dir, "internal")) {
		m.Kind = "check"
	} else {
		m.Kind = "tool"
	}
	return m, []string{fmt.Sprintf("inferred kind: %s (the v2 ceiling selector)", m.Kind)}, nil
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// runExtensionUpgrade migrates dir's manifest to extSchemaVersion. --check writes
// nothing and exits non-zero when behind (the CI "did you upgrade?" gate, like
// `llz env pipeline --check` / `llz drift --strict`); --dry-run reports the plan.
// runExtensionUpgrade is the extension-level analog of `llz upgrade` = copier
// update + re-apply: it migrates the manifest to the current schema AND re-applies
// the extension's files: into the instance (root), so a template change ships with
// the binary without a manual re-scaffold. --check aggregates schema drift and
// scaffold drift; both write nothing.
func runExtensionUpgrade(g globalOpts, dir, root string, check bool) error {
	path := filepath.Join(dir, extensionManifest)
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", extensionManifest, err)
	}
	var m extManifest
	if err := yaml.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("%s: %w", extensionManifest, err)
	}
	from := manifestVersion(m)
	if from > extSchemaVersion {
		return fmt.Errorf("extension %q schema v%d is newer than this llz understands (v%d) — update llz", m.Name, from, extSchemaVersion)
	}

	schemaBehind := from < extSchemaVersion
	var changelog []string
	if schemaBehind {
		for v := from; v < extSchemaVersion; v++ {
			nm, changes, mErr := extMigrations[v-1](m, dir)
			if mErr != nil {
				return fmt.Errorf("migrate v%d→v%d: %w", v, v+1, mErr)
			}
			m = nm
			changelog = append(changelog, changes...)
		}
		m.SchemaVersion = extSchemaVersion
	}

	reportSchema := func() {
		if schemaBehind {
			fmt.Fprintf(os.Stderr, "extension %q: schema v%d → v%d\n", m.Name, from, extSchemaVersion)
			for _, c := range changelog {
				fmt.Fprintf(os.Stderr, "  • %s\n", c)
			}
		} else {
			fmt.Fprintf(os.Stderr, "extension %q: schema v%d, up to date\n", m.Name, from)
		}
	}

	// --check: report both drifts, write nothing.
	if check {
		reportSchema()
		scaffoldErr := runExtensionApply(g, dir, root, true) // re-render vs disk
		if schemaBehind {
			return fmt.Errorf("extension %q is behind the current schema (v%d) — run `llz extension upgrade`", m.Name, extSchemaVersion)
		}
		return scaffoldErr
	}

	// Apply: migrate the manifest if behind, then ALWAYS re-scaffold files so a
	// template edit propagates with the binary (no copier round-trip).
	reportSchema()
	if schemaBehind && !g.dryRun {
		if err := writeManifestPreserving(path, b, m); err != nil {
			return err
		}
	} else if schemaBehind {
		fmt.Fprintln(os.Stderr, "(dry-run) recipe.yaml not written")
	}
	return runExtensionApply(g, dir, root, false)
}

// writeManifestPreserving applies the migrated manifest back to disk while
// keeping the operator's comments and key order: it edits the parsed YAML node
// tree in place rather than re-marshalling the struct (which sigs.k8s.io/yaml
// would reorder + strip comments from). It syncs only the scalar top-level keys
// the experiment's migrations touch (schemaVersion, kind, stage) — a migration that adds
// lists/maps would extend this. This is what makes `upgrade` safe to run on a
// human-owned, just-merged manifest, including when copier's `migrations:` hook
// invokes it after a `copier update` 3-way merge.
func writeManifestPreserving(path string, orig []byte, m extManifest) error {
	var doc yamlv3.Node
	if err := yamlv3.Unmarshal(orig, &doc); err != nil || len(doc.Content) == 0 || doc.Content[0].Kind != yamlv3.MappingNode {
		out, mErr := yaml.Marshal(m) // fallback: can't edit in place, regenerate
		if mErr != nil {
			return mErr
		}
		return os.WriteFile(path, out, 0o644)
	}
	root := doc.Content[0]
	setScalarKey(root, "schemaVersion", strconv.Itoa(m.SchemaVersion), "!!int")
	if m.Kind != "" {
		setScalarKey(root, "kind", m.Kind, "!!str")
	}
	if m.Stage != "" {
		setScalarKey(root, "stage", string(m.Stage), "!!str")
	}
	out, err := yamlv3.Marshal(&doc)
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o644)
}

// setScalarKey updates key's value in a YAML mapping node, or appends the pair
// when absent — leaving every other node (and its comments) untouched.
func setScalarKey(mapping *yamlv3.Node, key, val, tag string) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			v := mapping.Content[i+1]
			v.Kind, v.Tag, v.Value, v.Content = yamlv3.ScalarNode, tag, val, nil
			return
		}
	}
	mapping.Content = append(mapping.Content,
		&yamlv3.Node{Kind: yamlv3.ScalarNode, Tag: "!!str", Value: key},
		&yamlv3.Node{Kind: yamlv3.ScalarNode, Tag: tag, Value: val},
	)
}

// ── commands ─────────────────────────────────────────────────────────────────
