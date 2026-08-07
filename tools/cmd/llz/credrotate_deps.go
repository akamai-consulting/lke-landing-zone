package main

// credrotate_deps.go — wires internal/credrotate's one seam.
//
// Package main owns the forge credentials and the secret writer; credrotate owns
// WHICH secrets a rotation must reach — every infra-<deployment> environment, or
// the repo level on pre-env-scoped instances. That split is why the fan-out lives
// on the package side and the writer stays here.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/credrotate"

func init() {
	credrotate.Install(func(name, env, value string) error { return ghSetSecretFn(name, env, value) })
}
