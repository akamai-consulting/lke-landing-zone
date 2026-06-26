package main

import (
	"errors"
	"strings"
	"testing"
)

// fixedNow pins nowUnix for the duration of a test so the force-sync annotation
// value is deterministic.
func fixedNow(t *testing.T, v int64) {
	t.Helper()
	orig := nowUnix
	nowUnix = func() int64 { return v }
	t.Cleanup(func() { nowUnix = orig })
}

func TestRunCINudgeArgoRefreshesSyncsAndForceSyncs(t *testing.T) {
	fixedNow(t, 1700000000)
	var calls []string
	withExecOutput(t, func(name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil, nil
	})
	o := nudgeOpts{apps: defaultNudgeApps, store: defaultSecretStore, storeTimeout: 300}
	if err := runCINudgeArgo(globalOpts{}, o); err != nil {
		t.Fatalf("nudge-argo: %v", err)
	}
	// annotate+patch per app, plus one store-wait and one ExternalSecret force-sync.
	if want := 2*len(defaultNudgeApps) + 2; len(calls) != want {
		t.Fatalf("exec calls = %d, want %d (annotate+patch per app + wait + force-sync)", len(calls), want)
	}
	joined := strings.Join(calls, " | ")
	for _, app := range defaultNudgeApps {
		if !strings.Contains(joined, "annotate application "+app+" argocd.argoproj.io/refresh=hard") {
			t.Errorf("missing hard-refresh annotate for %s in %q", app, joined)
		}
		if !strings.Contains(joined, "patch application "+app) {
			t.Errorf("missing sync patch for %s in %q", app, joined)
		}
	}
	if !strings.Contains(joined, "-n argocd") {
		t.Errorf("nudge must target the argocd namespace: %q", joined)
	}
	// Store-Ready wait must precede the force-sync, and force-sync must hit every
	// ExternalSecret with a changing value.
	waitAt := strings.Index(joined, "wait --for=condition=Ready clustersecretstore/openbao --timeout=300s")
	syncAt := strings.Index(joined, "annotate externalsecret --all-namespaces --all force-sync=1700000000 --overwrite")
	if waitAt < 0 {
		t.Errorf("missing store-Ready wait in %q", joined)
	}
	if syncAt < 0 {
		t.Errorf("missing ExternalSecret force-sync in %q", joined)
	}
	if waitAt >= 0 && syncAt >= 0 && waitAt > syncAt {
		t.Errorf("force-sync must run AFTER the store-Ready wait: %q", joined)
	}
}

func TestRunCINudgeArgoForceSyncsEvenIfStoreNeverReady(t *testing.T) {
	// The store-wait timing out must NOT skip the force-sync (best-effort: a slow
	// store shouldn't strand the secrets at their refreshInterval).
	fixedNow(t, 42)
	var sawForceSync bool
	withExecOutput(t, func(_ string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "wait" {
			return nil, errors.New("timed out waiting for the condition")
		}
		if strings.Contains(strings.Join(args, " "), "force-sync=42") {
			sawForceSync = true
		}
		return nil, nil
	})
	if err := runCINudgeArgo(globalOpts{}, nudgeOpts{apps: nil, store: defaultSecretStore, storeTimeout: 1}); err != nil {
		t.Fatalf("best-effort nudge must not return error, got %v", err)
	}
	if !sawForceSync {
		t.Error("force-sync must still run after a store-wait timeout")
	}
}

func TestRunCINudgeArgoBestEffort(t *testing.T) {
	// Every kubectl call failing must NOT fail the command (best-effort), and it
	// must still attempt every app.
	apps := 0
	withExecOutput(t, func(_ string, args ...string) ([]byte, error) {
		if len(args) > 2 && args[2] == "annotate" { // -n argocd annotate application <app> ...
			apps++
		}
		return nil, errors.New("the server could not find the requested resource")
	})
	if err := runCINudgeArgo(globalOpts{}, nudgeOpts{apps: []string{"a", "b", "c"}, store: defaultSecretStore, storeTimeout: 1}); err != nil {
		t.Fatalf("best-effort nudge must not return error, got %v", err)
	}
	if apps != 3 {
		t.Errorf("attempted %d apps, want 3 (a failure must not stop the loop)", apps)
	}
}

func TestRunCINudgeArgoEmptyStoreSkipsForceSync(t *testing.T) {
	var joined string
	withExecOutput(t, func(name string, args ...string) ([]byte, error) {
		joined += " | " + name + " " + strings.Join(args, " ")
		return nil, nil
	})
	if err := runCINudgeArgo(globalOpts{}, nudgeOpts{apps: defaultNudgeApps, store: ""}); err != nil {
		t.Fatalf("nudge-argo: %v", err)
	}
	if strings.Contains(joined, "wait") || strings.Contains(joined, "externalsecret") {
		t.Errorf("empty --secret-store must skip the store-wait + force-sync: %q", joined)
	}
}

func TestRunCINudgeArgoDryRunAndWiring(t *testing.T) {
	withExecOutput(t, func(string, ...string) ([]byte, error) {
		t.Error("dry-run must not exec")
		return nil, nil
	})
	o := nudgeOpts{apps: defaultNudgeApps, store: defaultSecretStore, storeTimeout: 300}
	if err := runCINudgeArgo(globalOpts{dryRun: true}, o); err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	c := ciNudgeArgoCmd()
	if c.Use != "nudge-argo" {
		t.Errorf("Use = %q, want nudge-argo", c.Use)
	}
	if c.Flags().Lookup("secret-store") == nil || c.Flags().Lookup("store-timeout") == nil {
		t.Error("nudge-argo must expose --secret-store and --store-timeout flags")
	}
}
