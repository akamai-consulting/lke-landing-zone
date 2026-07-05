// Package metrics is a dependency-free Prometheus text-exposition writer for the
// in-cluster llz reconciler (see docs/designs/kube-native-reconciler.md).
//
// The tools module deliberately avoids client-go (internal/kube is a hand-rolled
// REST client for the slim distroless image) and, in the same spirit, avoids
// prometheus/client_golang: the text-exposition format is a handful of lines
// (# HELP / # TYPE / name{labels} value), so we hand-roll it and keep it
// unit-testable rather than pull the client library's transitive tree onto the
// image. Only the gauge type is implemented — the reconciler's surface is a set
// of point-in-time levels (convergence state, node counts, credential age, last-
// success timestamps), which are all gauges.
package metrics

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Registry is a concurrency-safe set of gauge families. The reconciler's sample
// loop calls SetGauge; the /metrics HTTP handler calls WriteTo — hence the lock.
type Registry struct {
	mu       sync.Mutex
	families map[string]*family // by metric name
}

// family is one metric name: its HELP text plus the current value per label set.
type family struct {
	help    string
	samples map[string]sample // keyed by the rendered label string (e.g. `{a="1"}`)
}

type sample struct {
	labels string // rendered, sorted `{k="v",...}` or "" for no labels
	value  float64
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{families: map[string]*family{}}
}

// SetGauge upserts the value of metric `name` for the given label set. Passing
// the same name with a different help string updates the help (last write wins);
// passing nil/empty labels registers the unlabelled sample. It is safe to call
// concurrently with WriteTo.
func (r *Registry) SetGauge(name, help string, labels map[string]string, value float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f := r.families[name]
	if f == nil {
		f = &family{samples: map[string]sample{}}
		r.families[name] = f
	}
	if help != "" {
		f.help = help
	}
	rendered := renderLabels(labels)
	f.samples[rendered] = sample{labels: rendered, value: value}
}

// WriteTo renders the registry in Prometheus text-exposition format v0.0.4.
// Output is deterministic: families sorted by name, samples sorted by their
// rendered label string — so tests can assert on it and scrapes are stable.
func (r *Registry) WriteTo(w io.Writer) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	names := make([]string, 0, len(r.families))
	for name := range r.families {
		names = append(names, name)
	}
	sort.Strings(names)

	var b strings.Builder
	for _, name := range names {
		f := r.families[name]
		if f.help != "" {
			fmt.Fprintf(&b, "# HELP %s %s\n", name, escapeHelp(f.help))
		}
		fmt.Fprintf(&b, "# TYPE %s gauge\n", name)

		keys := make([]string, 0, len(f.samples))
		for k := range f.samples {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			s := f.samples[k]
			fmt.Fprintf(&b, "%s%s %s\n", name, s.labels, formatValue(s.value))
		}
	}
	n, err := io.WriteString(w, b.String())
	return int64(n), err
}

// renderLabels renders a label map as `{k1="v1",k2="v2"}` with keys sorted (so
// the output is deterministic) and values escaped per the exposition spec. An
// empty/nil map renders as "".
func renderLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteString(`="`)
		b.WriteString(escapeLabelValue(labels[k]))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

// escapeLabelValue escapes a label value's backslash, double-quote, and newline
// per the exposition format. (%q would also escape these, but it additionally
// escapes other control chars in Go-specific ways; the spec names exactly these
// three, so we normalise to them and let %q emit the surrounding quotes.)
func escapeLabelValue(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	return v
}

// escapeHelp escapes a HELP line's backslash and newline (a HELP value may
// contain quotes unescaped, but never a raw newline).
func escapeHelp(h string) string {
	h = strings.ReplaceAll(h, `\`, `\\`)
	h = strings.ReplaceAll(h, "\n", `\n`)
	return h
}

// formatValue renders a float the way Prometheus expects: shortest round-trip
// decimal, with the special +Inf/-Inf/NaN tokens.
func formatValue(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}
