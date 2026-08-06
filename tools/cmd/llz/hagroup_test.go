package main

// TestHAGroupMissingRole STAYED: haGroupMissingRole is in scaffold_spec.go, part of
// the scaffolding path, not the topology reader. It travelled to
// internal/envtopology inside env_set_test.go and came straight back.

import (
	"path/filepath"
	"testing"
)

func TestHaGroupMissingRole(t *testing.T) {
	chdirTempDir(t)
	writeSpecInstance(t, map[string]string{
		"east": clusterDef("east", "    ha: { role: active, group: prod }\n"),
	})
	if got := haGroupMissingRole("prod"); got != "standby" {
		t.Errorf("missing role with only active = %q, want standby", got)
	}
	writeFileMkdir(t, filepath.Join("environments", "west.yaml"), clusterDef("west", "    ha: { role: standby, group: prod }\n"))
	if got := haGroupMissingRole("prod"); got != "" {
		t.Errorf("complete pair should report no missing role, got %q", got)
	}
}

// #4: committedTargets renders the letsencrypt issuer with the spec ACME email,
// and omits it entirely when no email is set.
// committedTargets emits only the THIN per-env files (overlay + env-revision +
// region patch + apl-overlay) — the manifests themselves live in the shared base.
