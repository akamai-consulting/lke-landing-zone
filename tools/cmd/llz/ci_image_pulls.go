package main

// ci_image_pulls.go implements `llz ci collect-image-pulls` — the Tier-1
// instrumentation that answers "is a bring-up phase pull-bound or not?" (the
// apl-core foundation-install question: docs/designs/e2e-instrumentation.md).
//
// Kubelet emits a `Pulled` Event per image with a human message
// "Successfully pulled image \"X\" in 1m2.3s (…)". This gathers them cluster-wide,
// parses the duration, and reports per-image + total pull time to the step
// summary + a JSON artifact — so a run shows whether the platform's cold image
// pulls are a meaningful fraction of a phase, or negligible (pods Ready in
// seconds once scheduled), before anyone builds a pre-pull to "fix" it.
//
// Read-only + best-effort: any kubectl/parse failure degrades to a note, never a
// hard failure (this is diagnostics, not a gate).

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// pullDurationRx pulls the "in <dur>" out of kubelet's Pulled message. Matches
// both "Successfully pulled image \"x\" in 1m2.345s" and "… in 4.2s".
var pullDurationRx = regexp.MustCompile(`(?:pulled image|Pulled image).*? in ([0-9hms.µ]+)`)

type imagePull struct {
	Image     string  `json:"image"`
	Namespace string  `json:"namespace"`
	Pod       string  `json:"pod"`
	DurationS float64 `json:"duration_s"`
}

func ciCollectImagePullsCmd() *cobra.Command {
	var out string
	c := &cobra.Command{
		Use:   "collect-image-pulls",
		Short: "report per-image kubelet pull durations (step summary + JSON) — is a phase pull-bound?",
		Long: "Gathers the cluster's `Pulled` Events, parses each image's pull duration, and\n" +
			"writes a per-image + total table to $GITHUB_STEP_SUMMARY plus (with --out) a\n" +
			"JSON artifact. Read-only, best-effort — a kubectl/parse failure is a note.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCollectImagePulls(out) },
	}
	c.Flags().StringVar(&out, "out", "", "write the JSON pull report here (for artifact upload)")
	return c
}

func runCollectImagePulls(out string) error {
	raw, err := execOutput("kubectl", "get", "events", "-A",
		"--field-selector", "reason=Pulled", "-o", "json")
	if err != nil {
		fmt.Fprintf(os.Stderr, "::warning::collect-image-pulls: kubectl get events failed (ignored): %v\n", err)
		return nil
	}
	pulls := parseImagePulls(raw)
	table := renderImagePullTable(pulls)
	fmt.Print(table)
	if err := appendGHAFile("GITHUB_STEP_SUMMARY", strings.TrimRight(table, "\n")); err != nil {
		fmt.Fprintf(os.Stderr, "::warning::collect-image-pulls: step-summary write failed (ignored): %v\n", err)
	}
	if out != "" {
		b, _ := json.MarshalIndent(pulls, "", "  ")
		if err := os.WriteFile(out, append(b, '\n'), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", out, err)
		}
	}
	return nil
}

// parseImagePulls extracts image pull durations from a kubectl Events list JSON.
// Defensive against missing/oddly-typed fields; an unparseable message is
// skipped, never a panic.
func parseImagePulls(eventsJSON []byte) []imagePull {
	var body struct {
		Items []struct {
			Message        string `json:"message"`
			InvolvedObject struct {
				Namespace string `json:"namespace"`
				Name      string `json:"name"`
			} `json:"involvedObject"`
		} `json:"items"`
	}
	if json.Unmarshal(eventsJSON, &body) != nil {
		return nil
	}
	var pulls []imagePull
	for _, it := range body.Items {
		m := pullDurationRx.FindStringSubmatch(it.Message)
		if m == nil {
			continue
		}
		d, err := time.ParseDuration(m[1])
		if err != nil {
			continue
		}
		pulls = append(pulls, imagePull{
			Image:     imageFromPullMessage(it.Message),
			Namespace: it.InvolvedObject.Namespace,
			Pod:       it.InvolvedObject.Name,
			DurationS: d.Seconds(),
		})
	}
	sort.SliceStable(pulls, func(i, j int) bool { return pulls[i].DurationS > pulls[j].DurationS })
	return pulls
}

// imageFromPullMessage extracts the quoted image ref from a Pulled message, or
// "" if it isn't quoted the way kubelet writes it.
func imageFromPullMessage(msg string) string {
	if i := strings.IndexByte(msg, '"'); i >= 0 {
		if j := strings.IndexByte(msg[i+1:], '"'); j >= 0 {
			return msg[i+1 : i+1+j]
		}
	}
	return ""
}

func renderImagePullTable(pulls []imagePull) string {
	var b strings.Builder
	b.WriteString("### image pull durations (kubelet `Pulled` events)\n\n")
	if len(pulls) == 0 {
		b.WriteString("_(no Pulled events — images were already cached, or the window has aged out of the event TTL)_\n")
		return b.String()
	}
	b.WriteString("| image | ns/pod | pull |\n|---|---|---|\n")
	var total float64
	for _, p := range pulls {
		img := p.Image
		if img == "" {
			img = "(unknown)"
		}
		fmt.Fprintf(&b, "| %s | %s/%s | %s |\n", img, p.Namespace, p.Pod, fmtDuration(p.DurationS))
		total += p.DurationS
	}
	fmt.Fprintf(&b, "| **sum of pulls** | %d image(s) | **%s** |\n", len(pulls), fmtDuration(total))
	b.WriteString("\n_Note: pulls across nodes run in parallel; the sum overcounts wall-clock. The signal is per-image magnitude + whether any single pull rivals the phase length._\n")
	return b.String()
}
