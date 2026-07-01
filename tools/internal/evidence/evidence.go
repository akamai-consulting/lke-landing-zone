// Package evidence assembles the auditor evidence pack for the CIS Kubernetes
// Benchmark + org-security-control mapping (docs/infosec/control-mapping.md).
//
// It is pure: it parses the trivy-operator ClusterComplianceReport status that
// `llz ci cis-evidence` harvests, folds in the supplemental signals (cred-audit
// result, NetworkPolicy coverage, restricted-PSS namespaces, encrypted
// StorageClass), and renders both the machine-readable pack and its Markdown
// summary. All shell-out / kubectl harvesting lives in the command; this package
// has no I/O so the assembly and PASS/FAIL logic can be unit-tested against
// canned inputs.
package evidence

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Result values for the pack and per-report rollups.
const (
	ResultPass         = "PASS"
	ResultFail         = "FAIL"
	ResultNotCollected = "NOT_COLLECTED"
)

// ControlResult is one control's outcome within a ClusterComplianceReport.
type ControlResult struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Severity  string `json:"severity,omitempty"`
	FailCount int    `json:"fail_count"`
}

// ComplianceReport is the harvested, normalized status of one trivy-operator
// ClusterComplianceReport (e.g. the standard CIS report or the org-control one).
type ComplianceReport struct {
	Name            string          `json:"name"`             // metadata.name (cis | llz-org-controls)
	ID              string          `json:"id"`               // spec.compliance.id
	Title           string          `json:"title"`            // spec.compliance.title
	PassCount       int             `json:"pass_count"`       // rolled-up checks/controls passing
	FailCount       int             `json:"fail_count"`       // rolled-up checks/controls failing
	FailingControls []ControlResult `json:"failing_controls"` // controls with fail_count > 0
}

// Result reports PASS when no control failed, else FAIL.
func (r ComplianceReport) Result() string {
	if r.FailCount > 0 {
		return ResultFail
	}
	return ResultPass
}

// Supplemental holds the non-trivy evidence signals harvested for the pack. A nil
// pointer / -1 count marks a signal that was not collected (graceful degradation
// when the cluster or a source is unavailable), distinct from a collected zero.
type Supplemental struct {
	CredAuditResult       string   `json:"cred_audit_result,omitempty"` // PASS | FAIL | PASS_WITH_WARNINGS | "" (not collected)
	NetworkPolicyCount    int      `json:"network_policy_count"`        // -1 = not collected
	RestrictedNamespaces  []string `json:"restricted_namespaces"`       // nil = not collected
	EncryptedStorageClass *bool    `json:"encrypted_storage_class"`     // nil = not collected
}

// Pack is the full evidence pack — the machine-readable artifact the auditor
// signs (and which `llz ci cis-evidence` cosign-attests).
type Pack struct {
	Event         string             `json:"event"`
	Cluster       string             `json:"cluster"`
	TimestampUnix int64              `json:"timestamp_unix"`
	GeneratedAt   string             `json:"generated_at"`
	MappingDoc    string             `json:"mapping_doc"`
	Reports       []ComplianceReport `json:"compliance_reports"`
	Supplemental  Supplemental       `json:"supplemental"`
	Result        string             `json:"result"`
}

// Inputs is what the command harvests and hands to BuildPack. A nil report means
// its CRD was absent (trivy-operator not ready / compliance disabled) — recorded
// as not-collected rather than treated as a pass.
type Inputs struct {
	Cluster       string
	TimestampUnix int64
	CISReport     *ComplianceReport
	OrgReport     *ComplianceReport
	Supplemental  Supplemental
}

// mappingDocPath is the auditor-facing control mapping the pack is evidence for.
const mappingDocPath = "docs/infosec/control-mapping.md"

// BuildPack folds the harvested inputs into the pack and computes the overall
// result: FAIL if any collected compliance report failed or the credential audit
// failed; otherwise PASS. Uncollected signals never fail the pack — they surface
// as warnings in the Markdown so an auditor knows coverage was incomplete.
func BuildPack(in Inputs) Pack {
	reports := make([]ComplianceReport, 0, 2)
	if in.CISReport != nil {
		reports = append(reports, *in.CISReport)
	}
	if in.OrgReport != nil {
		reports = append(reports, *in.OrgReport)
	}

	result := ResultPass
	for _, r := range reports {
		if r.FailCount > 0 {
			result = ResultFail
		}
	}
	if in.Supplemental.CredAuditResult == ResultFail {
		result = ResultFail
	}

	ts := in.TimestampUnix
	return Pack{
		Event:         "cis-kubernetes-evidence",
		Cluster:       in.Cluster,
		TimestampUnix: ts,
		GeneratedAt:   time.Unix(ts, 0).UTC().Format(time.RFC3339),
		MappingDoc:    mappingDocPath,
		Reports:       reports,
		Supplemental:  in.Supplemental,
		Result:        result,
	}
}

// ParseComplianceReport normalizes the JSON of one trivy-operator
// ClusterComplianceReport (`kubectl get clustercompliancereport <name> -o json`)
// into a ComplianceReport. It tolerates the schema variation across
// trivy-operator versions: it reads spec.compliance.{id,title}, the rolled-up
// status.summary.{passCount,failCount}, and per-control failures from either
// status.summaryReport.controls[] or status.detailReport.results[].
func ParseComplianceReport(raw []byte) (ComplianceReport, error) {
	var doc struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			Compliance struct {
				ID    string `json:"id"`
				Title string `json:"title"`
			} `json:"compliance"`
		} `json:"spec"`
		Status struct {
			Summary struct {
				PassCount json.Number `json:"passCount"`
				FailCount json.Number `json:"failCount"`
			} `json:"summary"`
			SummaryReport struct {
				Controls []struct {
					ID        string      `json:"id"`
					Name      string      `json:"name"`
					Severity  string      `json:"severity"`
					TotalFail json.Number `json:"totalFail"`
				} `json:"controls"`
			} `json:"summaryReport"`
			DetailReport struct {
				Results []struct {
					ID       string `json:"id"`
					Name     string `json:"name"`
					Severity string `json:"severity"`
					Status   string `json:"status"`
					Checks   []struct {
						Success bool `json:"success"`
					} `json:"checks"`
				} `json:"results"`
			} `json:"detailReport"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return ComplianceReport{}, fmt.Errorf("parse ClusterComplianceReport: %w", err)
	}

	rep := ComplianceReport{
		Name:      doc.Metadata.Name,
		ID:        doc.Spec.Compliance.ID,
		Title:     doc.Spec.Compliance.Title,
		PassCount: numToInt(doc.Status.Summary.PassCount),
		FailCount: numToInt(doc.Status.Summary.FailCount),
	}

	// Prefer the rolled-up summaryReport for per-control failures.
	for _, c := range doc.Status.SummaryReport.Controls {
		if fc := numToInt(c.TotalFail); fc > 0 {
			rep.FailingControls = append(rep.FailingControls, ControlResult{
				ID: c.ID, Name: c.Name, Severity: c.Severity, FailCount: fc,
			})
		}
	}
	// Fall back to detailReport when summaryReport is empty (reportType: all).
	if len(rep.FailingControls) == 0 {
		for _, r := range doc.Status.DetailReport.Results {
			fc := 0
			for _, ck := range r.Checks {
				if !ck.Success {
					fc++
				}
			}
			if fc == 0 && strings.EqualFold(r.Status, "FAIL") {
				fc = 1
			}
			if fc > 0 {
				rep.FailingControls = append(rep.FailingControls, ControlResult{
					ID: r.ID, Name: r.Name, Severity: r.Severity, FailCount: fc,
				})
			}
		}
	}

	// Derive summary counts from the detail when status.summary was absent.
	if rep.PassCount == 0 && rep.FailCount == 0 {
		for _, r := range doc.Status.DetailReport.Results {
			if strings.EqualFold(r.Status, "FAIL") {
				rep.FailCount++
			} else if strings.EqualFold(r.Status, "PASS") {
				rep.PassCount++
			}
		}
	}

	sort.Slice(rep.FailingControls, func(i, j int) bool {
		return rep.FailingControls[i].ID < rep.FailingControls[j].ID
	})
	return rep, nil
}

func numToInt(n json.Number) int {
	if n == "" {
		return 0
	}
	i, err := strconv.Atoi(n.String())
	if err != nil {
		return 0
	}
	return i
}

// RenderMarkdown renders the pack as the auditor-facing Markdown summary that
// accompanies the JSON bundle and feeds the GitHub step summary.
func RenderMarkdown(p Pack) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# CIS Kubernetes Benchmark — Evidence Pack\n\n")
	fmt.Fprintf(&b, "**Cluster:** %s  \n", dash(p.Cluster))
	fmt.Fprintf(&b, "**Generated:** %s  \n", p.GeneratedAt)
	fmt.Fprintf(&b, "**Result:** %s\n\n", resultBadge(p.Result))
	fmt.Fprintf(&b, "Evidence for the controls in [%s](../../%s). The managed LKE-Enterprise control plane (CIS §1–3) is out of scope by design — see that document.\n\n", p.MappingDoc, p.MappingDoc)

	fmt.Fprintf(&b, "## Compliance Reports (trivy-operator)\n\n")
	if len(p.Reports) == 0 {
		fmt.Fprintf(&b, "> ⚠️ No ClusterComplianceReport was collected — trivy-operator may not be installed or compliance reports are not yet reconciled. Controls in Sections A/B were **not scored** this run.\n\n")
	} else {
		fmt.Fprintf(&b, "| Report | Pass | Fail | Result |\n|--------|------|------|--------|\n")
		for _, r := range p.Reports {
			fmt.Fprintf(&b, "| %s (`%s`) | %d | %d | %s |\n", dash(r.Title), r.Name, r.PassCount, r.FailCount, r.Result())
		}
		b.WriteString("\n")
		for _, r := range p.Reports {
			if len(r.FailingControls) == 0 {
				continue
			}
			fmt.Fprintf(&b, "### Failing controls — %s\n\n", dash(r.Title))
			fmt.Fprintf(&b, "| Control | Name | Severity | Failures |\n|---------|------|----------|----------|\n")
			for _, c := range r.FailingControls {
				fmt.Fprintf(&b, "| %s | %s | %s | %d |\n", c.ID, dash(c.Name), dash(c.Severity), c.FailCount)
			}
			b.WriteString("\n")
		}
	}

	fmt.Fprintf(&b, "## Supplemental Evidence\n\n")
	fmt.Fprintf(&b, "| Signal | Value |\n|--------|-------|\n")
	fmt.Fprintf(&b, "| Credential audit (PAT/OBJ-key SLA) | %s |\n", notCollectedStr(p.Supplemental.CredAuditResult))
	fmt.Fprintf(&b, "| NetworkPolicies present | %s |\n", notCollectedInt(p.Supplemental.NetworkPolicyCount))
	fmt.Fprintf(&b, "| Namespaces enforcing restricted PSS | %s |\n", restrictedNSStr(p.Supplemental.RestrictedNamespaces))
	fmt.Fprintf(&b, "| Default StorageClass encrypted | %s |\n\n", boolStr(p.Supplemental.EncryptedStorageClass))

	fmt.Fprintf(&b, "## Sign-Off\n\n")
	fmt.Fprintf(&b, "Manual controls (account type, CSPM enrollment, SSO, training, port-exposure tickets) are attested via the InfoSec process documented in the control mapping. Attach this pack as the evidence reference.\n\n")
	fmt.Fprintf(&b, "| Role | Name | Date | Approval ref |\n|------|------|------|--------------|\n")
	fmt.Fprintf(&b, "| Project / Product owner | | | |\n")
	fmt.Fprintf(&b, "| InfoSec reviewer | | | |\n")
	return b.String()
}

func resultBadge(r string) string {
	switch r {
	case ResultPass:
		return "✅ PASS"
	case ResultFail:
		return "❌ FAIL"
	default:
		return r
	}
}

func notCollectedStr(s string) string {
	if s == "" {
		return "⚠️ not collected"
	}
	return s
}

func notCollectedInt(n int) string {
	if n < 0 {
		return "⚠️ not collected"
	}
	return strconv.Itoa(n)
}

func restrictedNSStr(ns []string) string {
	if ns == nil {
		return "⚠️ not collected"
	}
	if len(ns) == 0 {
		return "0"
	}
	return fmt.Sprintf("%d (%s)", len(ns), strings.Join(ns, ", "))
}

func boolStr(b *bool) string {
	if b == nil {
		return "⚠️ not collected"
	}
	if *b {
		return "yes"
	}
	return "no"
}

func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}
