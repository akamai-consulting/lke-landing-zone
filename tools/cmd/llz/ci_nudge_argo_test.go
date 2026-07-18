package main

import (
	"errors"
	"strings"
	"testing"
)

// fixedNow pins nowUnix for the duration of a test so the revalidation
// annotation value is deterministic.
func fixedNow(t *testing.T, v int64) {
	t.Helper()
	orig := nowUnix
	nowUnix = func() int64 { return v }
	t.Cleanup(func() { nowUnix = orig })
}

func TestRunCINudgeArgoRefreshesSyncsAndRevalidatesStore(t *testing.T) {
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
	// annotate+patch per app, plus the store revalidation bump and one store-wait.
	// No ExternalSecret force-sync: the es-store-recovery reconciler lane owns it.
	if want := 2*len(defaultNudgeApps) + 2; len(calls) != want {
		t.Fatalf("exec calls = %d, want %d (annotate+patch per app + store bump + wait)", len(calls), want)
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
	// The revalidation bump must precede the store-Ready wait — it's what makes the
	// wait event-paced rather than pinned to ESO's own refresh cadence.
	bumpAt := strings.Index(joined, "annotate clustersecretstore openbao force-sync=1700000000 --overwrite")
	waitAt := strings.Index(joined, "wait --for=condition=Ready clustersecretstore/openbao --timeout=300s")
	if bumpAt < 0 {
		t.Errorf("missing ClusterSecretStore revalidation bump in %q", joined)
	}
	if waitAt < 0 {
		t.Errorf("missing store-Ready wait in %q", joined)
	}
	if bumpAt >= 0 && waitAt >= 0 && bumpAt > waitAt {
		t.Errorf("the revalidation bump must run BEFORE the store-Ready wait: %q", joined)
	}
}

func TestRunCINudgeArgoNeverForceSyncsExternalSecrets(t *testing.T) {
	// secrets-before-apps Phase 3: the blanket ExternalSecret force-sync moved to
	// the in-cluster es-store-recovery reconciler lane, which fires on the store's
	// not-Ready→Ready transition and also covers PushSecrets. Re-adding it here
	// would duplicate the lane's work on every bootstrap — regression-guard it,
	// including on the store-wait-timeout path where the old code still fired.
	fixedNow(t, 42)
	var joined string
	withExecOutput(t, func(name string, args ...string) ([]byte, error) {
		joined += " | " + name + " " + strings.Join(args, " ")
		if len(args) > 0 && args[0] == "wait" {
			return nil, errors.New("timed out waiting for the condition")
		}
		return nil, nil
	})
	if err := runCINudgeArgo(globalOpts{}, nudgeOpts{apps: nil, store: defaultSecretStore, storeTimeout: 1}); err != nil {
		t.Fatalf("best-effort nudge must not return error, got %v", err)
	}
	if strings.Contains(joined, "externalsecret") {
		t.Errorf("nudge-argo must not touch ExternalSecrets (the reconciler lane owns that): %q", joined)
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

func TestRunCINudgeArgoEmptyStoreSkipsStoreHalf(t *testing.T) {
	var joined string
	withExecOutput(t, func(name string, args ...string) ([]byte, error) {
		joined += " | " + name + " " + strings.Join(args, " ")
		return nil, nil
	})
	if err := runCINudgeArgo(globalOpts{}, nudgeOpts{apps: defaultNudgeApps, store: ""}); err != nil {
		t.Fatalf("nudge-argo: %v", err)
	}
	if strings.Contains(joined, "wait") || strings.Contains(joined, "clustersecretstore") {
		t.Errorf("empty --secret-store must skip the revalidation bump + store-wait: %q", joined)
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
