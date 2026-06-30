package main

import (
	"fmt"
	"os"
	"path/filepath"

	"sigs.k8s.io/yaml"
)

// extSource is a git-pinned remote extension repo.
type extSource struct {
	Repo string `json:"repo"` // e.g. github.com/apple/llz-recipes
	Ref  string `json:"ref"`  // tag or SHA — pinned; a floating branch drifts on sync
}

const extensionsConfigFile = ".llz/extensions.yaml"

// extConfig is the committed enable-list. Dir is where local extensions live
// (each a subdir holding a recipe.yaml); Enabled names the ones that are on.
type extConfig struct {
	Dir     string      `json:"dir,omitempty"`
	Sources []extSource `json:"sources,omitempty"` // git-pinned remote extension repos
	Enabled []string    `json:"enabled,omitempty"`
}

func (c extConfig) extDir() string {
	if c.Dir == "" {
		return "extensions"
	}
	return c.Dir
}

func loadExtConfig(root string) (extConfig, error) {
	var c extConfig
	b, err := os.ReadFile(filepath.Join(root, extensionsConfigFile))
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil // optional, like .llz/commands.yaml
		}
		return c, err
	}
	if err := yaml.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s: %w", extensionsConfigFile, err)
	}
	return c, nil
}

func saveExtConfig(root string, c extConfig) error {
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	p := filepath.Join(root, extensionsConfigFile)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o644)
}

// Extension is the single runtime value built-in, local, and remote extensions
// all normalize into (the issue's `Recipe`): fsys is the read-root for its
// files: — embed.FS for a built-in, os.DirFS for local/remote — so every router
// is origin-agnostic past the load boundary. Dir is the on-disk path ("" for a

// readManifestAt loads and parses an extension's recipe.yaml from dir.
func readManifestAt(dir string) (extManifest, error) {
	var m extManifest
	b, err := os.ReadFile(filepath.Join(dir, extensionManifest))
	if err != nil {
		return m, err
	}
	if err := yaml.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("%s: %w", extensionManifest, err)
	}
	return m, nil
}
