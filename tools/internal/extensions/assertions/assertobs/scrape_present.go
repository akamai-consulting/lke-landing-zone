package assertobs

// scrape_present.go — when a ServiceMonitor has no scrape target, say what IS
// there.
//
// ── WHY ───────────────────────────────────────────────────────────────────────
//
// The failure this answers reads:
//
//	FAIL: ServiceMonitor llz-observability/cert-manager has NO scrape target —
//	      never discovered (missing `prometheus: system` label / selector /
//	      namespace mismatch)
//
// Three candidate causes and no way to tell which. Every one of them is a fact
// about the OTHER side — the Services in the target namespace — and the message
// describes only this side. docs/runbooks/e2e-lane-diagnostics.md names the
// pattern and the remedy in the same breath:
//
//	"If a lane says 'X is absent', the next command is almost always 'what is
//	present'. Several gates now print that themselves; where one does not, that
//	is a gap worth closing rather than a cluster worth standing up."
//
// This closes it for this lane. The cost of not having it, measured: a release
// e2e failed here, the log could not distinguish the three causes, and answering
// it needed a cluster kept alive at ~45 minutes and real money per attempt.
//
// ── WHAT IT PRINTS, AND WHY THAT SET ──────────────────────────────────────────
//
// The ServiceMonitor's own selector, namespaceSelector and endpoint port names,
// beside every Service in the selected namespaces with its labels and port
// names. Those are exactly the three things that have to line up, so a reader
// diffs two lists instead of forming a hypothesis.
//
// ADVISORY AND NON-FATAL. The verdict is already decided by the caller; this only
// adds evidence. A cluster that will not answer a `kubectl get svc` must not turn
// a precise assertion failure into a diagnostic error — the finding is the same
// either way, and losing it to a follow-up failure is how a gate's message gets
// worse instead of better.

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// smSpec is the part of a ServiceMonitor that decides what it can select.
type smSpec struct {
	Spec struct {
		NamespaceSelector struct {
			Any        bool     `json:"any"`
			MatchNames []string `json:"matchNames"`
		} `json:"namespaceSelector"`
		Selector struct {
			MatchLabels map[string]string `json:"matchLabels"`
		} `json:"selector"`
		Endpoints []struct {
			Port string `json:"port"`
		} `json:"endpoints"`
	} `json:"spec"`
}

// svcList is the part of a Service list that decides whether it can be selected.
type svcList struct {
	Items []struct {
		Metadata struct {
			Name      string            `json:"name"`
			Namespace string            `json:"namespace"`
			Labels    map[string]string `json:"labels"`
		} `json:"metadata"`
		Spec struct {
			Ports []struct {
				Name string `json:"name"`
				Port int    `json:"port"`
			} `json:"ports"`
		} `json:"spec"`
	} `json:"items"`
}

// ExplainNoScrapeTarget writes the both-sides evidence for one undiscovered
// ServiceMonitor. `monitor` is "<namespace>/<name>" as the assertion reports it.
func ExplainNoScrapeTarget(kubectl func(args ...string) (string, error), monitor string, out io.Writer) {
	ns, name, ok := strings.Cut(monitor, "/")
	if !ok {
		return
	}
	raw, err := kubectl("-n", ns, "get", "servicemonitor", name, "-o", "json")
	if err != nil {
		fmt.Fprintf(out, "  (could not read ServiceMonitor %s to explain: %v)\n", monitor, err)
		return
	}
	var sm smSpec
	if err := json.Unmarshal([]byte(raw), &sm); err != nil {
		fmt.Fprintf(out, "  (could not parse ServiceMonitor %s: %v)\n", monitor, err)
		return
	}

	var ports []string
	for _, e := range sm.Spec.Endpoints {
		if e.Port != "" {
			ports = append(ports, e.Port)
		}
	}
	fmt.Fprintf(out, "  it selects: labels %s, port name(s) %s, in namespace(s) %s\n",
		labelString(sm.Spec.Selector.MatchLabels), orNone(ports), namespaceScope(sm, ns))

	// WHERE TO LOOK is the namespaceSelector's answer, not the ServiceMonitor's
	// own namespace — getting that backwards is one of the three causes, so the
	// dump must not quietly assume it away.
	targets := sm.Spec.NamespaceSelector.MatchNames
	args := []string{"get", "svc", "-o", "json"}
	switch {
	case sm.Spec.NamespaceSelector.Any:
		args = append(args, "-A")
	case len(targets) == 0:
		args = append([]string{"-n", ns}, args...)
	default:
		// One namespace per call keeps the output attributable; the set is small.
		for _, t := range targets {
			dumpServices(kubectl, append([]string{"-n", t}, args...), t, sm, out)
		}
		return
	}
	dumpServices(kubectl, args, namespaceScope(sm, ns), sm, out)
}

func dumpServices(kubectl func(args ...string) (string, error), args []string, where string, sm smSpec, out io.Writer) {
	raw, err := kubectl(args...)
	if err != nil {
		fmt.Fprintf(out, "  (could not list Services in %s: %v)\n", where, err)
		return
	}
	var list svcList
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		fmt.Fprintf(out, "  (could not parse Services in %s: %v)\n", where, err)
		return
	}
	// FAIL LOUD ON AN EMPTY NAMESPACE rather than printing a bare header. "No
	// Services at all" and "Services that do not match" are different repairs, and
	// an empty list under a heading reads as the second.
	if len(list.Items) == 0 {
		fmt.Fprintf(out, "  what IS in %s: NO Services at all — the namespace is empty or does not exist\n", where)
		return
	}
	fmt.Fprintf(out, "  what IS in %s (%d Service(s)):\n", where, len(list.Items))
	for _, s := range list.Items {
		var pn []string
		for _, p := range s.Spec.Ports {
			pn = append(pn, fmt.Sprintf("%s:%d", orDash(p.Name), p.Port))
		}
		mark := " "
		if matchesSelector(s.Metadata.Labels, sm.Spec.Selector.MatchLabels) {
			// The label half matched — so the miss is the PORT NAME, which is the
			// one cause the original message never names.
			mark = "*"
		}
		fmt.Fprintf(out, "   %s %s/%s  ports=[%s]  labels=%s\n",
			mark, s.Metadata.Namespace, s.Metadata.Name, strings.Join(pn, " "), labelString(s.Metadata.Labels))
	}
	fmt.Fprintf(out, "  (* = its labels DO match the selector; if one is starred, the miss is the port name)\n")
}

// matchesSelector is matchLabels semantics: every wanted pair must be present.
// An EMPTY selector matches everything, which is Kubernetes' own rule and worth
// keeping rather than special-casing to "matches nothing".
func matchesSelector(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

func labelString(m map[string]string) string {
	if len(m) == 0 {
		return "(none)"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, ",")
}

func namespaceScope(sm smSpec, own string) string {
	switch {
	case sm.Spec.NamespaceSelector.Any:
		return "(any)"
	case len(sm.Spec.NamespaceSelector.MatchNames) > 0:
		return strings.Join(sm.Spec.NamespaceSelector.MatchNames, ",")
	default:
		return own + " (its own — no namespaceSelector)"
	}
}

func orNone(s []string) string {
	if len(s) == 0 {
		return "(none)"
	}
	return strings.Join(s, ",")
}

func orDash(s string) string {
	if s == "" {
		return "(unnamed)"
	}
	return s
}
