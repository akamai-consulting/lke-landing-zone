package evidencepack

// harvest.go implements `llz ci cis-evidence` — the auditor evidence-pack
// generator for the instance's answered security checklist
// (docs/infosec/control-mapping.md).
//
// It harvests the machine-readable evidence the platform already produces:
//   - the trivy-operator ClusterComplianceReports apl-core's trivy-operator chart
//     ships (DefaultReports — the chart's charts/trivy-operator/templates/specs/),
//     and whether the operator's cluster-compliance scanner is actually on;
//   - NetworkPolicy coverage, restricted-PSS namespaces, and the default
//     StorageClass's encryption parameter;
//   - the outcomes of the gates that ran beside it (--gate <verb>=<outcome>; the
//     scheduled job passes the credential single pane's results);
//   - the instance's answered checklist (--questionnaire, a data file in the
//     instance repo — questionnaire.go),
// folds them into one pack, writes the JSON bundle + a Markdown summary an
// auditor signs + a sha256 of the bundle, and appends the summary to the step
// summary.
//
// Every cluster harvest is best-effort: a missing CRD or an unreachable cluster
// records the signal as "not collected" rather than failing, so the pack is always
// produced. But "not collected" is never "passed" (see evidence.go), and --strict
// turns a NOT_COLLECTED pack — including one built while the compliance scanner is
// off — into a failure. The questionnaire is NOT best-effort: a file that is
// present but does not validate fails the run.

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

// DefaultReports are the ClusterComplianceReports apl-core's vendored
// trivy-operator chart creates (compliance.specs default; one object per spec,
// named after the spec id). The pack harvests these rather than shipping its own:
// a duplicate CIS object with an empty control list scores nothing, and the
// chart's objects are what the operator reconciles when its compliance scanner is
// on. Override with --report when an apl-core upgrade renames a spec.
var DefaultReports = []string{"k8s-cis-1.23", "k8s-nsa-1.0", "k8s-pss-baseline-0.1", "k8s-pss-restricted-0.1"}

// DefaultMaxReportAge bounds how old a report's status.updateTimestamp may be.
// The chart's compliance cron is every 6h; a day leaves room for a missed run and
// still refuses a report from a scanner that has stopped.
const DefaultMaxReportAge = 24 * time.Hour

type cisEvidenceOpts struct {
	cluster            string
	outDir             string
	gates              []string
	strict             bool
	reports            []string
	maxReportAge       time.Duration
	questionnaire      string
	checkQuestionnaire bool
	// knownVerbs is the `llz ci` verb set the questionnaire's gates must belong
	// to (filled from the cobra tree; nil skips the check).
	knownVerbs map[string]bool
}

// kubectlGet is the transport seam: one read through the declared cluster-read
// handle. Tests replace it.
var kubectlGet = func(args ...string) ([]byte, error) {
	return capability.MustCluster(Extension().MustBinding("cis-evidence")).Run(append([]string{"get"}, args...)...)
}

// readRepoFile is the questionnaire seam: a read through the declared read-repo
// handle, fenced to the working tree (the instance checkout). Tests replace it.
var readRepoFile = func(rel string) ([]byte, error) {
	return capability.RepoAt(Extension().MustBinding("cis-evidence"), ".").ReadFile(rel)
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
	q, err := LoadQuestionnaire(readRepoFile, o.questionnaire, o.knownVerbs)
	if err != nil {
		return err
	}
	if o.checkQuestionnaire {
		if q == nil {
			return fmt.Errorf("--check-questionnaire: %s does not exist", o.questionnaire)
		}
		fmt.Printf("questionnaire %s is valid: %q, %d requirements, gates cited: %s\n",
			o.questionnaire, q.Metadata.Name, len(q.Requirements), strings.Join(q.CitedGates(), ", "))
		return nil
	}
	if q == nil {
		slog.Warn("no questionnaire found — the pack will carry no answered checklist", "path", o.questionnaire)
	}

	gates, err := parseGates(o.gates)
	if err != nil {
		return err
	}
	reportNames := o.reports
	if len(reportNames) == 0 {
		reportNames = DefaultReports
	}
	in := Inputs{
		Cluster:           firstNonEmpty(o.cluster, "unknown"),
		TimestampUnix:     now().Unix(),
		ComplianceScanner: harvestScanner(),
		Supplemental:      Supplemental{NetworkPolicyCount: -1}, // -1 until collected
		Gates:             gates,
		Questionnaire:     q,
		QuestionnairePath: o.questionnaire,
	}
	for _, name := range reportNames {
		in.Reports = append(in.Reports, harvestComplianceReport(name, o.maxReportAge))
	}

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
		fmt.Fprintf(os.Stderr, "::error::Compliance evidence pack for %s reports FAIL — a compliance report or a recorded gate failed. See the pack and docs/infosec/control-mapping.md.\n", pack.Cluster)
		return fmt.Errorf("compliance evidence pack result: FAIL for %s", pack.Cluster)
	case ResultNotCollected:
		msg := fmt.Sprintf("compliance evidence pack for %s is NOT_COLLECTED: %s — it is not evidence of compliance", pack.Cluster, NotCollectedReason(pack))
		if o.strict {
			fmt.Fprintf(os.Stderr, "::error::%s.\n", msg)
			return fmt.Errorf("%s", msg)
		}
		fmt.Fprintf(os.Stderr, "::warning::%s.\n", msg)
	}
	fmt.Printf("Compliance evidence pack written for %s (result: %s, sha256 %s).\n", pack.Cluster, pack.Result, digest)
	return nil
}

// harvestScanner reads the trivy-operator compliance-scanner switch. Unreadable
// (no trivy-operator, no cluster) is "not collected", which BuildPack treats
// exactly like "disabled": the pack cannot PASS.
func harvestScanner() string {
	out, err := kubectlGet("configmap", trivyOperatorConfigMap, "-n", trivyOperatorNamespace, "-o", "json")
	if err != nil {
		slog.Warn("trivy-operator config not readable — compliance scanner state not collected", "err", err)
		return ScannerNotCollected
	}
	state, err := ComplianceScannerState(out)
	if err != nil {
		slog.Warn("could not parse trivy-operator config", "err", err)
	}
	return state
}

// harvestComplianceReport fetches, parses and evaluates one
// ClusterComplianceReport. An absent or unreadable report is returned as a
// NOT_COLLECTED entry, never dropped and never passed.
func harvestComplianceReport(name string, maxAge time.Duration) ComplianceReport {
	out, err := kubectlGet("clustercompliancereports.aquasecurity.github.io", name, "-o", "json")
	if err != nil {
		slog.Warn("ClusterComplianceReport not collected", "name", name, "err", err)
		return MissingReport(name, "not readable: "+firstLine(err.Error()))
	}
	rep, err := ParseComplianceReport(out)
	if err != nil {
		slog.Warn("could not parse ClusterComplianceReport", "name", name, "err", err)
		return MissingReport(name, "unparseable: "+firstLine(err.Error()))
	}
	if rep.Name == "" {
		rep.Name = name
	}
	EvaluateReport(&rep, now(), maxAge)
	return rep
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
	questionnaire := ""
	if p.Questionnaire != nil {
		questionnaire = p.Questionnaire.Metadata.Name
	}
	return map[string]any{
		"event":                p.Event,
		"timestamp_unix":       p.TimestampUnix,
		"cluster":              p.Cluster,
		"compliance_scanner":   p.ComplianceScanner,
		"compliance_reports":   reports,
		"gates":                gates,
		"questionnaire":        questionnaire,
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

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
