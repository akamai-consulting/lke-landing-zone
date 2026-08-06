package converge

// Gap-closing tests for ci_wait.go surfaced by mutation testing. Each one pins a
// decision the wait gates make that was previously unasserted: the SIZE of the
// budget handed to `kubectl wait`, that the poll loop actually POLLS (rather than
// probing once), that it honours --interval seconds (not nanoseconds), and that
// the timeout diagnostics really probe the apiserver instead of reporting
// "unknown" — the arm an operator reads when a gate gives up.

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// remainingSecs is the value that becomes `kubectl wait --timeout=<N>s`, so its
// two arms are load-bearing: a live budget must be passed through intact, and an
// exhausted one must floor at 1 (0 means "wait forever" to kubectl — the exact
// opposite of an expired budget).
func TestRemainingSecsCarriesTheBudgetAndFloorsAtOne(t *testing.T) {
	if got := remainingSecs(time.Now().Add(600 * time.Second)); got < 590 || got > 600 {
		t.Errorf("remainingSecs(now+600s) = %d, want ~600 — the remaining budget must reach kubectl wait", got)
	}
	for _, d := range []time.Duration{-time.Hour, -time.Second, 0, 500 * time.Millisecond, 1500 * time.Millisecond} {
		if got := remainingSecs(time.Now().Add(d)); got < 1 {
			t.Errorf("remainingSecs(now%+v) = %d, want >= 1 — 0 or negative means 'wait forever' to kubectl", d, got)
		}
	}
}

// waitTimeoutArg extracts N from the "--timeout=Ns" flag in a joined kubectl argv.
func waitTimeoutArg(t *testing.T, joined string) int {
	t.Helper()
	for _, f := range strings.Fields(joined) {
		if s, ok := strings.CutPrefix(f, "--timeout="); ok {
			n, err := strconv.Atoi(strings.TrimSuffix(s, "s"))
			if err != nil {
				t.Fatalf("unparseable --timeout in %q: %v", joined, err)
			}
			return n
		}
	}
	t.Fatalf("no --timeout flag in %q", joined)
	return 0
}

// The shared deadline must be built from --timeout SECONDS, and every kubectl
// wait must inherit what is left of it. A deadline computed in the wrong unit
// collapses to "now", so each wait gets the 1s floor and the gate gives up
// essentially immediately while still reporting a 600s budget in its message.
func TestRunCIWaitPodsHandsTheRemainingBudgetToEachWait(t *testing.T) {
	calls := stubKubectlWait(t, func(string) error { return nil })
	if err := runCIWaitPods("ns", "Running", []string{"p-0", "p-1"}, 600, 0); err != nil {
		t.Fatalf("wait-pods = %v, want nil", err)
	}
	if len(*calls) != 4 {
		t.Fatalf("made %d kubectl wait calls, want 4: %v", len(*calls), *calls)
	}
	for _, c := range *calls {
		if got := waitTimeoutArg(t, c); got < 500 {
			t.Errorf("kubectl wait got --timeout=%ds under a 600s budget (call %q) — the shared deadline is not in seconds", got, c)
		}
	}
}

// The cluster-ready gate must POLL: a fresh LKE pool answers the API in seconds
// but registers nodes minutes later, so a gate that probes once and gives up is
// the "bootstrap onto an empty pool" failure this command exists to prevent.
func TestRunCIWaitClusterReadyKeepsPollingUntilNodesJoin(t *testing.T) {
	origCombined := deps.ExecCombined
	deps.ExecCombined = func(string, ...string) string { return "" }
	t.Cleanup(func() { deps.ExecCombined = origCombined })

	polls := 0
	withKubectl(t, func(a string) ([]byte, error) {
		if !strings.HasPrefix(a, "get nodes -o ") {
			return nil, errors.New("unexpected: " + a)
		}
		polls++
		if polls >= 2 {
			return []byte("node-1=True\n"), nil // the pool finally registers
		}
		return []byte(""), nil // reachable, but no nodes yet
	})
	// 1s budget, 1s interval => pollUntil allows timeout/interval+1 = 2 attempts.
	// An earlier version passed interval=0 to run the whole sequence inside one
	// second; pollUntil now caps a zero interval at a SINGLE attempt, so that
	// spelling silently tested nothing but the first poll. The interval must be
	// non-zero for the gate to re-poll at all, which costs one real second here —
	// the smallest that still proves it does not give up after one look.
	if err := runCIWaitClusterReady(1, 1, 10, 1); err != nil {
		t.Fatalf("wait-cluster-ready = %v, want nil — the gate must keep polling for the pool (polls=%d)", err, polls)
	}
	if polls < 2 {
		t.Errorf("gate made %d polls, want >= 2 — it stopped probing before the nodes joined", polls)
	}
}

// ...and the poll cadence must be --interval SECONDS. An interval collapsed to
// zero turns the gate into a hot loop against the apiserver and lets it burn
// through far more attempts than the budget allows.
func TestRunCIWaitClusterReadyHonoursThePollInterval(t *testing.T) {
	origCombined := deps.ExecCombined
	deps.ExecCombined = func(string, ...string) string { return "" }
	t.Cleanup(func() { deps.ExecCombined = origCombined })

	polls := 0
	withKubectl(t, func(a string) ([]byte, error) {
		if !strings.HasPrefix(a, "get nodes -o ") {
			return nil, errors.New("unexpected: " + a)
		}
		polls++
		if polls >= 10 {
			return []byte("node-1=True\n"), nil
		}
		return []byte(""), nil
	})
	// 1s budget at a 1s interval leaves room for ~2 polls, so a pool that only
	// registers on the 10th must NOT be reached: the gate has to give up.
	if err := runCIWaitClusterReady(1, 1, 10, 1); err == nil {
		t.Fatalf("wait-cluster-ready = nil after %d polls — a 1s interval was not honoured, so the gate outran its own 1s budget", polls)
	}
	if polls > 4 {
		t.Errorf("gate made %d polls in a 1s budget at a 1s interval, want <= 4 — it is hot-looping the apiserver", polls)
	}
}

// diagnoseAPIServer is the deadline arm's headline diagnostic: it must actually
// reach the endpoint and report the HTTP status, because "API up but this
// runner's credentials/ACL are wrong" and "API never came up" want different
// operator responses. Reporting "unknown" (or "probe failed") for a reachable
// endpoint collapses the two.
func TestDiagnoseAPIServerReportsTheProbeResult(t *testing.T) {
	hits := 0
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/version" {
			t.Errorf("probed %q, want /version", r.URL.Path)
		}
		hits++
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"gitVersion":"v1.32.1"}`))
	}))
	defer probe.Close()

	withKubectl(t, func(a string) ([]byte, error) {
		if a == "config view --minify -o jsonpath={.clusters[0].cluster.server}" {
			return []byte(probe.URL + "\n"), nil
		}
		return nil, errors.New("unexpected: " + a)
	})
	out := captureStderr(t, diagnoseAPIServer)
	if hits != 1 {
		t.Fatalf("the /version probe was made %d times, want 1 — a readable kubeconfig server MUST be probed", hits)
	}
	if !strings.Contains(out, "API endpoint: "+probe.URL) {
		t.Errorf("diagnostics did not name the endpoint it read:\n%s", out)
	}
	if !strings.Contains(out, "direct /version probe: HTTP 200") {
		t.Errorf("a successful probe must report its HTTP status (it implicates the credentials, not provisioning):\n%s", out)
	}
	if !strings.Contains(out, "v1.32.1") {
		t.Errorf("the probe body carries the version the operator reads:\n%s", out)
	}

	// An unreadable kubeconfig server must say so and probe nothing.
	hits = 0
	withKubectl(t, func(string) ([]byte, error) { return nil, errors.New("no kubeconfig") })
	out = captureStderr(t, diagnoseAPIServer)
	if hits != 0 {
		t.Errorf("probed %d times with no readable server, want 0", hits)
	}
	if !strings.Contains(out, "API endpoint: unknown") {
		t.Errorf("an unreadable kubeconfig must report an unknown endpoint:\n%s", out)
	}

	// A server that refuses the connection reports the probe failure (still not
	// "API is up"), so the two arms stay distinguishable.
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := closed.URL
	closed.Close()
	withKubectl(t, func(a string) ([]byte, error) {
		if strings.HasPrefix(a, "config view") {
			return []byte(url), nil
		}
		return nil, errors.New("unexpected: " + a)
	})
	out = captureStderr(t, diagnoseAPIServer)
	if !strings.Contains(out, "direct /version probe failed") {
		t.Errorf("an unreachable endpoint must report the probe failure:\n%s", out)
	}
	if strings.Contains(out, "API is up") {
		t.Errorf("an unreachable endpoint must NOT claim the API is up:\n%s", out)
	}
}

// The diagnostic client must stay short-deadlined: it runs on the timeout path of
// a gate that has already burned its budget, inside a job with its own axe. An
// unbounded client (Timeout 0) would hang the failure report itself.
func TestAPIProbeClientIsShortDeadlined(t *testing.T) {
	c := apiProbeClient()
	if c.Timeout != 5*time.Second {
		t.Errorf("apiProbeClient().Timeout = %v, want 5s — 0 means no timeout, which would hang the timeout diagnostics", c.Timeout)
	}
}
