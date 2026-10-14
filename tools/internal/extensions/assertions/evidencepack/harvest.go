package evidencepack

// harvest.go implements `llz ci cis-evidence` — the auditor evidence-pack
// generator for the CIS Kubernetes Benchmark + org-security-control mapping
// (docs/infosec/control-mapping.md).
//
// It harvests the machine-readable evidence the platform already produces:
//   - the trivy-operator ClusterComplianceReports (the standard `cis` report and
//     the custom `llz-org-controls` report — the complianceReports component,
//     platform-apl/components/complianceReports/),
//   - NetworkPolicy coverage, restricted-PSS namespaces, and the default
//     StorageClass's encryption parameter,
//   - the outcomes of the gates that ran beside it (--gate <verb>=<outcome>; the
//     scheduled job passes the credential single pane's and the Wiz enrollment
//     gate's results),
// folds them into one pack with the pre-filled checklist answers, writes the JSON
// bundle + a Markdown summary an auditor signs + a sha256 of the bundle, and
// appends the summary to the step summary.
//
// Every harvest is best-effort: a missing CRD or an unreachable cluster records
// the signal as "not collected" rather than failing, so the pack is always
// produced. But "not collected" is never "passed": a pack that collected nothing
// says NOT_COLLECTED, and --strict turns that into a failure.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cli"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghaout"
)

type cisEvidenceOpts struct {
	cluster string
	outDir  string
	gates   []string
	strict  bool
}

// kubectlGet is the transport seam: one read through the declared cluster-read
// handle. Tests replace it.
var kubectlGet = func(args ...string) ([]byte, error) {
	return capability.MustCluster(Extension().MustBinding("cis-evidence")).Run(append([]string{"get"}, args...)...)
}

var now = time.Now

// parseGates turns repeated `<verb>=<PASS|FAIL|SKIPPED>` flags into a map. An
// unknown outcome is an error rather than a silently dropped row: a typo that
// turned FAIL into nothing would launder a failing gate out of the pack.
func parseGates(specs []string) (map[string]string, error) {
	out := map[string]string{}
	for _, s := range specs {
		verb, outcome, ok := strings.Cut(s, "=")
		verb, outcome = strings.TrimSpace(verb), strings.ToUpper(strings.TrimSpace(outcome))
		if !ok || verb == "" {
			return nil, fmt.Errorf("--gate %q: want <verb>=<PASS|FAIL|SKIPPED>", s)
		}
		// GitHub step outcomes, so a workflow can pass steps.<id>.outcome straight in.
		switch outcome {
		case "SUCCESS":
			outcome = GatePass
		case "FAILURE", "CANCELLED":
			outcome = GateFail
		}
		switch outcome {
		case GatePass, GateFail, GateSkipped:
		default:
			return nil, fmt.Errorf("--gate %q: outcome %q is not PASS, FAIL or SKIPPED (or a step outcome success/failure/skipped)", s, outcome)
		}
		out[verb] = outcome
	}
	return out, nil
}

func runCICISEvidence(o cisEvidenceOpts) error {
	gates, err := parseGates(o.gates)
	if err != nil {
		return err
	}
	in := Inputs{
		Cluster:       firstNonEmpty(o.cluster, "unknown"),
		TimestampUnix: now().Unix(),
		Supplemental:  Supplemental{NetworkPolicyCount: -1}, // -1 until collected
		Gates:         gates,
	}

	in.CISReport = harvestComplianceReport("cis")
	in.OrgReport = harvestComplianceReport("llz-org-controls")

	if out, err := kubectlGet("networkpolicies.networking.k8s.io", "-A", "-o", "json"); err != nil {
		slog.Warn("could not list NetworkPolicies — recording as not collected", "err", err)
	} else if n, err := CountListItems(out); err == nil {
		in.Supplemental.NetworkPolicyCount = n
	}
	if out, err := kubectlGet("namespaces", "-o", "json"); err != nil {
		slog.Warn("could not list namespaces — recording as not collected", "err", err)
	} else if ns, err := RestrictedNamespaces(out); err == nil {
		in.Supplemental.RestrictedNamespaces = ns
	}
	if out, err := kubectlGet("storageclasses.storage.k8s.io", "-o", "json"); err != nil {
		slog.Warn("could not list storageclasses — recording as not collected", "err", err)
	} else if enc, err := DefaultStorageClassEncrypted(out); err == nil {
		in.Supplemental.EncryptedStorageClass = enc
	}

	pack := BuildPack(in)
	digest, err := writePack(o.outDir, pack)
	if err != nil {
		return err
	}
	rec := packRecord(pack)
	rec["sha256"] = digest
	if err := cli.PrintRecord(rec); err != nil {
		return err
	}
	if err := ghaout.Append("GITHUB_STEP_SUMMARY", RenderMarkdown(pack), "", "Bundle sha256: `"+digest+"`"); err != nil {
		return err
	}

	switch pack.Result {
	case ResultFail:
		fmt.Fprintf(os.Stderr, "::error::CIS evidence pack for %s reports FAIL — a compliance report or a recorded gate failed. See the pack and docs/infosec/control-mapping.md.\n", pack.Cluster)
		return fmt.Errorf("cis evidence pack result: FAIL for %s", pack.Cluster)
	case ResultNotCollected:
		msg := fmt.Sprintf("CIS evidence pack for %s collected NOTHING (no compliance report, no gate outcome) — it is not evidence of compliance", pack.Cluster)
		if o.strict {
			fmt.Fprintf(os.Stderr, "::error::%s.\n", msg)
			return fmt.Errorf("%s", msg)
		}
		fmt.Fprintf(os.Stderr, "::warning::%s.\n", msg)
	}
	fmt.Printf("CIS evidence pack written for %s (result: %s, sha256 %s).\n", pack.Cluster, pack.Result, digest)
	return nil
}

// harvestComplianceReport fetches and parses one ClusterComplianceReport,
// returning nil when the report/CRD is absent or unreadable (recorded as not
// collected — never as passing).
func harvestComplianceReport(name string) *ComplianceReport {
	out, err := kubectlGet("clustercompliancereports.aquasecurity.github.io", name, "-o", "json")
	if err != nil {
		slog.Warn("ClusterComplianceReport not collected", "name", name, "err", err)
		return nil
	}
	rep, err := ParseComplianceReport(out)
	if err != nil {
		slog.Warn("could not parse ClusterComplianceReport", "name", name, "err", err)
		return nil
	}
	return &rep
}

// writePack writes the JSON bundle, the Markdown summary and the bundle's sha256
// into outDir, returning the digest.
func writePack(outDir string, p Pack) (string, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", outDir, err)
	}
	base := packBaseName(p)
	jsonBytes, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(jsonBytes)
	digest := hex.EncodeToString(sum[:])
	for name, body := range map[string][]byte{
		base + ".json":        jsonBytes,
		base + ".md":          []byte(RenderMarkdown(p)),
		base + ".json.sha256": []byte(digest + "  " + base + ".json\n"),
	} {
		if err := os.WriteFile(filepath.Join(outDir, name), body, 0o644); err != nil {
			return "", fmt.Errorf("write %s: %w", name, err)
		}
	}
	fmt.Fprintf(os.Stderr, "evidence pack: %s.{json,md,json.sha256}\n", filepath.Join(outDir, base))
	return digest, nil
}

func packBaseName(p Pack) string {
	return fmt.Sprintf("cis-evidence-%s-%d", p.Cluster, p.TimestampUnix)
}

// packRecord flattens the pack into the single-line stdout record.
func packRecord(p Pack) map[string]any {
	reports := make([]any, 0, len(p.Reports))
	for _, r := range p.Reports {
		reports = append(reports, map[string]any{
			"name": r.Name, "id": r.ID, "pass": r.PassCount, "fail": r.FailCount, "result": r.Result(),
		})
	}
	gates := make([]string, 0, len(p.Gates))
	for v, o := range p.Gates {
		gates = append(gates, v+"="+o)
	}
	sort.Strings(gates)
	return map[string]any{
		"event":                p.Event,
		"timestamp_unix":       p.TimestampUnix,
		"cluster":              p.Cluster,
		"compliance_reports":   reports,
		"gates":                gates,
		"network_policy_count": p.Supplemental.NetworkPolicyCount,
		"restricted_ns_count":  len(p.Supplemental.RestrictedNamespaces),
		"result":               p.Result,
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
