package main

import (
	"errors"
	"strings"
	"testing"
)

func TestRunCINudgeArgoRefreshesAndSyncs(t *testing.T) {
	var calls []string
	withExecOutput(t, func(name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil, nil
	})
	if err := runCINudgeArgo(globalOpts{}, defaultNudgeApps); err != nil {
		t.Fatalf("nudge-argo: %v", err)
	}
	// One annotate (hard refresh) + one patch (sync) per app, against argocd ns.
	if len(calls) != 2*len(defaultNudgeApps) {
		t.Fatalf("exec calls = %d, want %d (annotate+patch per app)", len(calls), 2*len(defaultNudgeApps))
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
}

func TestRunCINudgeArgoBestEffort(t *testing.T) {
	// Every kubectl call failing must NOT fail the command (best-effort), and it
	// must still attempt every app.
	apps := 0
	withExecOutput(t, func(_ string, args ...string) ([]byte, error) {
		if args[2] == "annotate" { // -n argocd annotate application <app> ...
			apps++
		}
		return nil, errors.New("the server could not find the requested resource")
	})
	if err := runCINudgeArgo(globalOpts{}, []string{"a", "b", "c"}); err != nil {
		t.Fatalf("best-effort nudge must not return error, got %v", err)
	}
	if apps != 3 {
		t.Errorf("attempted %d apps, want 3 (a failure must not stop the loop)", apps)
	}
}

func TestRunCINudgeArgoDryRunAndWiring(t *testing.T) {
	withExecOutput(t, func(string, ...string) ([]byte, error) {
		t.Error("dry-run must not exec")
		return nil, nil
	})
	if err := runCINudgeArgo(globalOpts{dryRun: true}, defaultNudgeApps); err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if c := ciNudgeArgoCmd(); c.Use != "nudge-argo" {
		t.Errorf("Use = %q, want nudge-argo", c.Use)
	}
}
