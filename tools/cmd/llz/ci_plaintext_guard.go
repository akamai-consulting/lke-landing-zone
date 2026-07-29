package main

// ci_plaintext_guard.go implements `llz ci plaintext-guard` — the static gate on
// UNENCRYPTED in-cluster communication.
//
// The problem it solves is drift, not any single hop. An audit of in-cluster
// traffic found credentials crossing the pod network in cleartext (the Harbor
// admin password in a Basic-auth header; Keycloak's JWKS, i.e. the signing keys
// OpenBao validates team logins with) alongside a set of scrapes and probes
// whose plaintext is a considered trade. Nothing distinguished the two, and
// nothing stopped the set growing: a new `scheme: http` on a ServiceMonitor, or
// a new `InsecureSkipVerify: true`, passed every gate in the repo silently.
//
// So: every plaintext hop in the tree must be REGISTERED here with a reason and
// an owner. Adding one becomes a reviewed decision instead of an invisible one,
// and the registry doubles as the auditor-facing list of accepted residuals —
// the thing docs/adr/0010-in-cluster-mtls.md describes in prose.
//
// UNUSED ENTRIES FAIL. A registry that keeps entries for hops that no longer
// exist is how an allowlist rots into a rubber stamp: the next reader cannot
// tell which lines are load-bearing. When a hop is secured, its line goes.
//
// SCANNER SCOPE. It detects four shapes: `scheme: http` on a scrape endpoint,
// `insecureSkipVerify: true`, an http:// URL naming an in-cluster Service, and
// `InsecureSkipVerify: true` in Go. It does NOT see plaintext container ports or
// probe endpoints — a listener that simply serves HTTP on a port is invisible to
// it. Those are real hops (kubelet→/healthz is one) and they belong in the ADR,
// but a scanner that tried to infer them from a port number would be guessing.
//
// WHAT THIS DOES NOT DO. There is no ValidatingAdmissionPolicy twin, unlike
// ci_wave_health_guard.go. A VAP rejecting plaintext ServiceMonitors would apply
// to apl-core's resources too — cert-manager's, Loki's, everything the platform
// installs — and would fail them closed at admission. This guard is scoped to
// what THIS repo ships, which is what this repo can actually fix.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// plaintextRule is why one hop is allowed to stay unencrypted.
type plaintextRule struct {
	// reason must say what crosses the wire, not merely that it is allowed —
	// "otelcol_* internal counters" is reviewable; "accepted" is not.
	reason string
	// owner names who could actually close it: "llz" (us — so it is a TODO with
	// our name on it), "apl-core", "upstream-<project>", or "inherent" for hops
	// no party can close.
	owner string
}

// plaintextAllowed registers every accepted unencrypted hop, keyed by
// "<file-relative-path>:<locator>" where locator identifies the finding within
// the file (a ServiceMonitor endpoint port, a Go symbol, a values key).
//
// Keys are path-based on purpose. A kind/name key would collide across the
// chart-templated resources, and the path is what a reviewer needs to go read.
var plaintextAllowed = map[string]plaintextRule{
	// ── ours, actively being closed (PR #360, ADR 0010) ──────────────────────
	//
	// These are NOT settled residuals. They are the cleartext hops the mTLS work
	// removes, registered so this guard can land independently of it. When #360
	// merges, each of these stops matching and the guard FAILS on the stale
	// entry — which is the intended forcing function: the registry cannot
	// silently outlive the thing it describes.
	"platform-apl/components/llzReconciler/llz-reconciler/servicemonitor.yaml:metrics": {
		owner: "llz",
		reason: "Prometheus scrape of the reconciler's llz_* gauges. Becomes mTLS in #360 " +
			"(serving cert from llz-serving-ca, client cert from llz-client-ca) — delete this " +
			"entry when that lands",
	},
	"kubernetes-charts/llz-openbao-platform/templates/openbao-servicemonitor.yaml:https": {
		owner: "llz",
		reason: "Prometheus scrape of OpenBao /v1/sys/metrics — encrypted but the server is not " +
			"verified and the client is not authenticated. Becomes real CA verification plus a " +
			"client certificate in #360 — delete this entry when that lands",
	},
	"kubernetes-charts/llz-openbao-platform/values.yaml:http://loki-gateway.llz-observability.svc.cluster.local": {
		owner: "llz",
		reason: "OpenBao audit-log shipping from the promtail sidecar. Values are hashed by " +
			"OpenBao, but request paths, operations and auth.display_name ship in clear. NOTE this " +
			"Service does not exist — Loki runs in `monitoring` — so the pipeline is also BROKEN; " +
			"whoever repairs the URL must give it TLS at the same time",
	},
	"tools/internal/openbao/openbao.go:HTTPClientInsecure": {
		owner: "llz",
		reason: "the shared unverified transport for pod→OpenBao. #360 splits it: the loopback " +
			"cases keep an unverified client (renamed HTTPClientLoopback) and everything on the " +
			"pod network moves to mTLS — re-key this entry to the new name when that lands",
	},

	// ── apl-core owned ───────────────────────────────────────────────────────
	"platform-apl/components/observability/cert-manager-servicemonitor.yaml:tcp-prometheus-servicemonitor": {
		owner: "apl-core",
		reason: "cert-manager is installed and configured by apl-core; its metrics listener serves " +
			"HTTP and LLZ ships no values for it. Payload is cert-manager counters plus certificate " +
			"expiry timestamps — no key material. INVESTIGATED: `cert-manager` is not an app key in " +
			"apl-values/values.yaml, but core apps CAN take _rawValues (argocd does), so an override " +
			"may be possible; `llz ci validate-apl-values` cannot confirm it without a rendered " +
			"instance, so the next step is a render or a live cluster, not a guess",
	},

	// ── upstream limited ─────────────────────────────────────────────────────
	"platform-apl/components/observability/otel-collector-servicemonitor.yaml:monitoring": {
		owner: "upstream-opentelemetry-collector",
		reason: "the :8888 Service is created by the otel-operator from the CR, and its Prometheus " +
			"pull endpoint takes TLS only through the collector's thin service.telemetry support. " +
			"Payload is otelcol_* internals; breaking OTelCollectorMetricsTargetDown to encrypt a " +
			"queue gauge is a bad trade",
	},

	// ── ours, out of this guard's scope by construction ──────────────────────
	"tools/cmd/llz/ci_wait.go:apiProbeClient": {
		owner: "inherent",
		reason: "probes the LKE-managed control-plane endpoint from OUTSIDE the cluster during " +
			"provisioning, before a cluster exists to have a PKI. Reads a status line; carries no " +
			"credential and its result authorizes nothing",
	},
}

// ── scanning ────────────────────────────────────────────────────────────────

type plaintextFinding struct {
	key, file, what string
	line            int
}

var (
	reSchemeHTTP   = regexp.MustCompile(`^\s*scheme:\s*http\s*$`)
	reInsecureYAML = regexp.MustCompile(`^\s*insecureSkipVerify:\s*true\s*$`)
	reInsecureGo   = regexp.MustCompile(`InsecureSkipVerify:\s*true`)
	reSvcHTTP      = regexp.MustCompile(`http://[a-z0-9.\-]+\.svc(\.cluster\.local)?`)
	rePortName     = regexp.MustCompile(`^\s*-?\s*port:\s*(\S+)\s*$`)
)

func ciPlaintextGuardCmd() *cobra.Command {
	var root string
	cmd := &cobra.Command{
		Use:   "plaintext-guard",
		Short: "fail when an unencrypted in-cluster hop is not registered as an accepted residual",
		Long: "Static gate on cleartext in-cluster communication (docs/adr/0010-in-cluster-mtls.md).\n" +
			"Scans platform-apl/ and kubernetes-charts/ for `scheme: http` scrapes,\n" +
			"`insecureSkipVerify: true`, and http:// URLs to in-cluster Services, plus\n" +
			"tools/ for InsecureSkipVerify. Every hit must be registered in\n" +
			"plaintextAllowed with a reason and an owner; unregistered hits fail, and so\n" +
			"do registry entries whose hop no longer exists.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIPlaintextGuard(root) },
	}
	cmd.Flags().StringVar(&root, "root", ".", "repo root (template or instance layout)")
	return cmd
}

func runCIPlaintextGuard(root string) error {
	dirs := plaintextScanDirs(root)
	findings, examined, err := collectPlaintextFindings(root, dirs)
	if err != nil {
		return err
	}
	// Same rationale as the sibling guards: a guard that walked nothing prints
	// the same green as one that walked everything.
	if err := requireCorpus("plaintext-guard", examined, dirs); err != nil {
		return err
	}

	seen := map[string]bool{}
	failed := false
	for _, f := range findings {
		rule, ok := plaintextAllowed[f.key]
		seen[f.key] = true
		if ok {
			fmt.Printf("  ok: %s:%d %s — [%s] %s\n", f.file, f.line, f.what, rule.owner, rule.reason)
			continue
		}
		failed = true
		fmt.Printf("::error file=%s,line=%d::unregistered plaintext hop (%s). Every unencrypted in-cluster hop must be an explicit, reviewed decision: either secure it, or register %q in plaintextAllowed (ci_plaintext_guard.go) with a reason naming WHAT crosses the wire and an owner who could close it. See docs/adr/0010-in-cluster-mtls.md.\n",
			f.file, f.line, f.what, f.key)
	}

	// Stale entries: the hop is gone, so the line is now misinformation.
	var stale []string
	for k := range plaintextAllowed {
		if !seen[k] {
			stale = append(stale, k)
		}
	}
	sort.Strings(stale)
	for _, k := range stale {
		failed = true
		fmt.Printf("::error::plaintextAllowed entry %q matches nothing in the tree. The hop was secured or moved — delete the entry. A registry that keeps dead entries stops being reviewable, because a reader cannot tell which lines are load-bearing.\n", k)
	}

	if failed {
		return fmt.Errorf("plaintext-guard: unregistered plaintext hop(s) and/or stale registry entries")
	}
	fmt.Printf("plaintext-guard: %d plaintext hop(s), all registered with a reason and an owner.\n", len(findings))
	return nil
}

func plaintextScanDirs(root string) []string {
	dirs := platformTreeDirs(root)
	dirs = append(dirs, filepath.Join(root, "kubernetes-charts"), filepath.Join(root, "tools"))
	return dirs
}

func collectPlaintextFindings(root string, dirs []string) ([]plaintextFinding, int, error) {
	var out []plaintextFinding
	examined := 0
	for _, dir := range dirs {
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil // a missing tree is requireCorpus's problem, not a walk error
			}
			if info.IsDir() {
				// Vendored/generated trees say nothing about what we ship.
				if b := info.Name(); b == "vendor" || b == "rendered" || b == "coverage" || b == ".git" {
					return filepath.SkipDir
				}
				return nil
			}
			// The guard's own source contains the literals it searches for (its
			// finding messages and the registry's example keys), so it would
			// report itself. Linters exempt themselves for the same reason.
			if filepath.Base(path) == "ci_plaintext_guard.go" || filepath.Base(path) == "ci_plaintext_guard_test.go" {
				return nil
			}
			ext := filepath.Ext(path)
			isYAML := ext == ".yaml" || ext == ".yml"
			isGo := ext == ".go" && !strings.HasSuffix(path, "_test.go")
			if !isYAML && !isGo {
				return nil
			}
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			examined++
			rel := relForKey(root, path)
			out = append(out, scanPlaintext(rel, string(b), isGo)...)
			return nil
		})
		if err != nil {
			return nil, 0, err
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].file != out[j].file {
			return out[i].file < out[j].file
		}
		return out[i].line < out[j].line
	})
	return out, examined, nil
}

// scanPlaintext is the pure scanner — file content in, findings out — so the
// match rules are unit-tested without a tree on disk.
func scanPlaintext(rel, content string, isGo bool) []plaintextFinding {
	var out []plaintextFinding
	lines := strings.Split(content, "\n")
	lastPort := ""
	for i, ln := range lines {
		n := i + 1
		// Strip comments so a line DESCRIBING a plaintext hop is not itself a
		// finding — every registered residual carries an explanatory comment, and
		// those must not become self-fulfilling matches.
		code := stripComment(ln, isGo)
		if strings.TrimSpace(code) == "" {
			continue
		}
		if isGo {
			if reInsecureGo.MatchString(code) {
				out = append(out, plaintextFinding{
					key: rel + ":" + goSymbolFor(lines, i), file: rel, line: n,
					what: "InsecureSkipVerify: true",
				})
			}
			continue
		}
		if m := rePortName.FindStringSubmatch(code); m != nil {
			lastPort = strings.Trim(m[1], `"'`)
		}
		switch {
		case reSchemeHTTP.MatchString(code):
			out = append(out, plaintextFinding{
				key: rel + ":" + lastPort, file: rel, line: n,
				what: "scrape over plaintext (scheme: http)",
			})
		case reInsecureYAML.MatchString(code):
			out = append(out, plaintextFinding{
				key: rel + ":" + lastPort, file: rel, line: n,
				what: "TLS without server verification (insecureSkipVerify: true)",
			})
		case reSvcHTTP.MatchString(code):
			out = append(out, plaintextFinding{
				key: rel + ":" + reSvcHTTP.FindString(code), file: rel, line: n,
				what: "plaintext URL to an in-cluster Service (" + reSvcHTTP.FindString(code) + ")",
			})
		}
	}
	return out
}

// stripComment removes the trailing/leading comment so prose about a hop is not
// mistaken for the hop. Deliberately naive about quoted '#' / '//' inside
// strings: a false NEGATIVE there would need a URL inside a quoted string
// following a comment marker on the same line, which does not occur in this
// tree, and the alternative (matching comments) makes every explanatory note a
// finding.
func stripComment(ln string, isGo bool) string {
	if isGo {
		if i := strings.Index(ln, "//"); i >= 0 {
			return ln[:i]
		}
		return ln
	}
	if i := strings.Index(ln, "#"); i >= 0 {
		return ln[:i]
	}
	return ln
}

// goSymbolFor names the enclosing func/var for a Go finding, so the registry key
// is stable against line moves. Walks backwards to the nearest declaration.
func goSymbolFor(lines []string, idx int) string {
	for i := idx; i >= 0; i-- {
		s := strings.TrimSpace(lines[i])
		for _, p := range []string{"func ", "var ", "const "} {
			if strings.HasPrefix(s, p) {
				rest := strings.TrimPrefix(s, p)
				rest = strings.TrimPrefix(rest, "(")
				cut := strings.IndexAny(rest, " (){}=*[]")
				if cut > 0 {
					return rest[:cut]
				}
				if rest != "" {
					return rest
				}
			}
		}
	}
	return "?"
}

// relForKey renders a repo-relative, slash-separated path for registry keys, so
// a key is identical whether the guard runs from the repo root or from tools/.
func relForKey(root, path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return filepath.ToSlash(path)
	}
	rel, err := filepath.Rel(rootAbs, abs)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(rel)
}
