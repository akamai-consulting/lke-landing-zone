package main

import (
	"strings"
	"testing"
)

// stubSeed records what the seamed writers receive and restores them on cleanup.
func stubSeed(t *testing.T) (bao, gh *[]string) {
	t.Helper()
	var b, g []string
	origB, origG := seedBaoFn, seedGHEnvFn
	seedBaoFn = func(_ globalOpts, path, key, value string) error {
		b = []string{path, key, value}
		return nil
	}
	seedGHEnvFn = func(_ globalOpts, env, name, value string) error {
		g = []string{env, name, value}
		return nil
	}
	t.Cleanup(func() { seedBaoFn, seedGHEnvFn = origB, origG })
	return &b, &g
}

func TestParseBaoTarget(t *testing.T) {
	if p, k, err := parseBaoTarget("secret/app#token"); err != nil || p != "secret/app" || k != "token" {
		t.Fatalf("got %q,%q,%v", p, k, err)
	}
	for _, bad := range []string{"secret/app", "#token", "secret/app#", ""} {
		if _, _, err := parseBaoTarget(bad); err == nil {
			t.Fatalf("expected error for %q", bad)
		}
	}
}

func TestSeedRoutesToBothStores(t *testing.T) {
	root := t.TempDir()
	installExt(t, root, "ohttp",
		"schemaVersion: 2\nname: ohttp\nshort: x\nkind: tool\nsecrets:\n"+
			"  - {name: OHTTP_KEY_SEED, bao: 'secret/ohttp#seed'}\n"+
			"  - {name: FERMYON_CLOUD_TOKEN, ghEnv: infra-lab}\n", nil)
	saveExtConfig(root, extConfig{Enabled: []string{"ohttp"}})
	t.Setenv("OHTTP_KEY_SEED", "deadbeef")
	t.Setenv("FERMYON_CLOUD_TOKEN", "fc-xyz")

	bao, gh := stubSeed(t)
	if err := runExtensionSeed(globalOpts{yes: true}, root); err != nil {
		t.Fatal(err)
	}
	if strings.Join(*bao, ",") != "secret/ohttp,seed,deadbeef" {
		t.Fatalf("bao write = %v", *bao)
	}
	if strings.Join(*gh, ",") != "infra-lab,FERMYON_CLOUD_TOKEN,fc-xyz" {
		t.Fatalf("gh write = %v", *gh)
	}
}

func TestSeedGatedWithoutYesWritesNothing(t *testing.T) {
	root := t.TempDir()
	installExt(t, root, "a", "schemaVersion: 2\nname: a\nshort: x\nkind: tool\nsecrets:\n  - {name: TOK, bao: 'secret/a#tok'}\n", nil)
	saveExtConfig(root, extConfig{Enabled: []string{"a"}})
	t.Setenv("TOK", "v")
	bao, gh := stubSeed(t)
	if err := runExtensionSeed(globalOpts{yes: false}, root); err != nil {
		t.Fatalf("no-yes should print a plan, not error: %v", err)
	}
	if *bao != nil || *gh != nil {
		t.Fatal("nothing must be written without --yes")
	}
}

func TestSeedRequiredMissingErrors(t *testing.T) {
	root := t.TempDir()
	installExt(t, root, "a", "schemaVersion: 2\nname: a\nshort: x\nkind: tool\nsecrets:\n  - {name: ZZ_UNSET, required: true, bao: 'secret/a#k'}\n", nil)
	saveExtConfig(root, extConfig{Enabled: []string{"a"}})
	stubSeed(t)
	if err := runExtensionSeed(globalOpts{yes: true}, root); err == nil {
		t.Fatal("a required secret with no value should fail")
	}
}

func TestSeedSkipsOptionalUnsetAndDeclareOnly(t *testing.T) {
	root := t.TempDir()
	installExt(t, root, "a", "schemaVersion: 2\nname: a\nshort: x\nkind: tool\nsecrets:\n"+
		"  - {name: OPT_UNSET, bao: 'secret/a#k'}\n"+ // optional, unset → skipped
		"  - {name: NO_TARGET}\n", nil) // declare-only → ignored by seed
	saveExtConfig(root, extConfig{Enabled: []string{"a"}})
	bao, gh := stubSeed(t)
	if err := runExtensionSeed(globalOpts{yes: true}, root); err != nil {
		t.Fatal(err)
	}
	if *bao != nil || *gh != nil {
		t.Fatal("an optional-unset and a declare-only secret should write nothing")
	}
}
