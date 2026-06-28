package main

import "io/fs"

type Extension struct {
	Name     string
	Source   string
	Dir      string
	fsys     fs.FS
	Manifest extManifest
}

// loadEnabledExtensions resolves the operator's enabled set (local + remote).
// Built-ins are added by loadAllExtensions, which the lifecycle driver uses.
