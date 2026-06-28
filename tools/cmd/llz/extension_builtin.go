package main

// extension_builtin.go holds the compiled-in extensions — the issue's
// `builtinRecipes()`. They prove origin-erasure: a built-in carries an embed.FS
// as its fsys, a remote carries os.DirFS over its cache, and every router reads
// through Extension.fsys without knowing which. Built-ins are always enabled
// (they propagate with the binary), so loadAllExtensions prepends them.

import (
	"embed"
	"io/fs"

	"sigs.k8s.io/yaml"
)

//go:embed all:builtins
var builtinFS embed.FS

// builtinExtensions returns the extensions compiled into this binary. Each is a
// subdir of builtins/ with a recipe.yaml; its fsys is rooted there so files:
// render from the embed exactly as a local/remote extension renders from disk.
func builtinExtensions() []Extension {
	entries, err := builtinFS.ReadDir("builtins")
	if err != nil {
		return nil
	}
	var out []Extension
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sub, err := fs.Sub(builtinFS, "builtins/"+e.Name())
		if err != nil {
			continue
		}
		b, err := fs.ReadFile(sub, extensionManifest)
		if err != nil {
			continue
		}
		var m extManifest
		if yaml.Unmarshal(b, &m) != nil {
			continue
		}
		out = append(out, Extension{Name: m.Name, Source: "builtin", fsys: sub, Manifest: m})
	}
	return out
}
