// Package evidencepack is the `cis-evidence` extension: the auditor evidence pack
// for the instance's answered security checklist (docs/infosec/control-mapping.md).
//
// evidence.go is the pure half: it parses the trivy-operator
// ClusterComplianceReport status `llz ci cis-evidence` harvests, judges whether
// each report is real evidence (EvaluateReport), folds in the supplemental
// signals (NetworkPolicy coverage, restricted-PSS namespaces, encrypted
// StorageClass), the state of the compliance scanner, and the outcomes of the
// gates that ran beside it, and renders both the machine-readable pack and its
// Markdown summary — including the instance's answered checklist
// (questionnaire.go). All kubectl harvesting lives in harvest.go behind a seam, so
// the assembly and PASS/FAIL logic are unit-tested against canned inputs.
//
// FAIL CLOSED ON VACUITY, AT EVERY LEVEL. A report that exists is not a report
// that was scanned: apl-core's trivy-operator ships the compliance specs as
// objects but runs with clusterComplianceEnabled:false, so those objects sit with
// no status at all. A report with 0 pass / 0 fail, or no status.updateTimestamp,
// or one older than the freshness bound, is NOT_COLLECTED — and so is the whole
// pack whenever the compliance scanner is off. Only a fresh, non-empty, passing
// set of reports from a running scanner makes the pack PASS.
package evidencepack

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

// Compliance-scanner states (the trivy-operator `OPERATOR_CLUSTER_COMPLIANCE_ENABLED`
// setting, read from its config map).
const (
	ScannerEnabled      = "enabled"
	ScannerDisabled     = "disabled"
	ScannerNotCollected = "not collected"
)

// ControlResult is one control's outcome within a ClusterComplianceReport.
type ControlResult struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Severity  string `json:"severity,omitempty"`
	FailCount int    `json:"fail_count"`
}

// ComplianceReport is the harvested, normalized status of one trivy-operator
// ClusterComplianceReport (e.g. apl-core's k8s-cis-1.23).
type ComplianceReport struct {
	Name            string          `json:"name"`             // metadata.name
	ID              string          `json:"id"`               // spec.compliance.id
	Title           string          `json:"title"`            // spec.compliance.title
	PassCount       int             `json:"pass_count"`       // rolled-up checks/controls passing
	FailCount       int             `json:"fail_count"`       // rolled-up checks/controls failing
	FailingControls []ControlResult `json:"failing_controls"` // controls with fail_count > 0
	// UpdatedAt is status.updateTimestamp — when the scanner last reconciled the
	// report. Empty means it never has.
	UpdatedAt string `json:"updated_at,omitempty"`
	// Status is the report's verdict (EvaluateReport); Reason says why when it is
	// not PASS.
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// Result is the report's verdict. A report nobody evaluated is NOT_COLLECTED.
func (r ComplianceReport) Result() string {
	if r.Status == "" {
		return ResultNotCollected
	}
	return r.Status
}

// EvaluateReport sets r.Status/r.Reason. FAIL wins over everything (a failing
// control is never laundered by staleness); otherwise the report must have been
// reconciled, recently, and have actually scored something to PASS.
func EvaluateReport(r *ComplianceReport, now time.Time, maxAge time.Duration) {
	r.Status, r.Reason = ResultNotCollected, ""
	switch {
	case r.FailCount > 0:
		r.Status = ResultFail
		return
	case r.UpdatedAt == "":
		r.Reason = "status.updateTimestamp is unset — the compliance scanner has never reconciled this report"
		return
	}
	ts, err := time.Parse(time.RFC3339, r.UpdatedAt)
	if err != nil {
		r.Reason = fmt.Sprintf("status.updateTimestamp %q is not RFC 3339", r.UpdatedAt)
		return
	}
	if age := now.Sub(ts); maxAge > 0 && age > maxAge {
		r.Reason = fmt.Sprintf("stale: last reconciled %s ago (bound %s)", age.Round(time.Minute), maxAge)
		return
	}
	if r.PassCount == 0 {
		r.Reason = "0 pass / 0 fail — the report scored nothing"
		return
	}
	r.Status = ResultPass
}

// MissingReport is the pack entry for an expected report that could not be read.
func MissingReport(name, reason string) ComplianceReport {
	return ComplianceReport{Name: name, Status: ResultNotCollected, Reason: reason}
}

// Gate outcomes the pack records (`--gate <verb>=<outcome>`).
const (
	GatePass    = "PASS"
	GateFail    = "FAIL"
	GateSkipped = "SKIPPED"
)

// Supplemental holds the non-trivy evidence signals harvested for the pack. A nil
// pointer / -1 count marks a signal that was not collected (graceful degradation
// when the cluster or a source is unavailable), distinct from a collected zero.
type Supplemental struct {
	NetworkPolicyCount    int      `json:"network_policy_count"`    // -1 = not collected
	RestrictedNamespaces  []string `json:"restricted_namespaces"`   // nil = not collected
	EncryptedStorageClass *bool    `json:"encrypted_storage_class"` // nil = not collected
}

// Pack is the full evidence pack — the machine-readable artifact the auditor
// signs off (its sha256 is written beside it).
type Pack struct {
	Event         string `json:"event"`
	Cluster       string `json:"cluster"`
	TimestampUnix int64  `json:"timestamp_unix"`
	GeneratedAt   string `json:"generated_at"`
	MappingDoc    string `json:"mapping_doc"`
	// ComplianceScanner is the trivy-operator cluster-compliance scanner state.
	ComplianceScanner string             `json:"compliance_scanner"`
	Reports           []ComplianceReport `json:"compliance_reports"`
	Supplemental      Supplemental       `json:"supplemental"`
	// Gates are the outcomes of the `llz ci` gates that ran beside the pack (the
	// scheduled job passes them in), keyed by verb. Absent = not run this time.
	Gates map[string]string `json:"gates"`
	// Questionnaire is the instance's answered checklist, carried in the pack so
	// the answers and the evidence travel together. Nil = none was provided.
	Questionnaire     *Questionnaire `json:"questionnaire"`
	QuestionnairePath string         `json:"questionnaire_path,omitempty"`
	Result            string         `json:"result"`
}

// Inputs is what the command harvests and hands to BuildPack. Reports carries one
// entry per EXPECTED report, already evaluated — absent ones as MissingReport, so
// an absence is visible in the pack rather than shrinking it.
type Inputs struct {
	Cluster           string
	TimestampUnix     int64
	ComplianceScanner string
	Reports           []ComplianceReport
	Supplemental      Supplemental
	Gates             map[string]string
	Questionnaire     *Questionnaire
	QuestionnairePath string
}

// mappingDocPath is the auditor-facing control mapping the pack is evidence for.
const mappingDocPath = "docs/infosec/control-mapping.md"

// BuildPack folds the harvested inputs into the pack and computes its result:
//
//   - FAIL if any report failed or any recorded gate failed;
//   - otherwise NOT_COLLECTED unless the compliance scanner is enabled AND every
//     expected report is a fresh, non-empty PASS;
//   - otherwise PASS.
//
// Gate outcomes alone never make a pack PASS: they are recorded on the rows that
// cite them, but a pack whose compliance scan did not happen is not evidence of
// compliance, however green the credential gates beside it were.
func BuildPack(in Inputs) Pack {
	reports := append([]ComplianceReport(nil), in.Reports...)
	scanner := in.ComplianceScanner
	if scanner == "" {
		scanner = ScannerNotCollected
	}

	failed := false
	allPass := len(reports) > 0
	for _, r := range reports {
		switch r.Result() {
		case ResultFail:
			failed = true
			allPass = false
		case ResultPass:
		default:
			allPass = false
		}
	}
	gates := map[string]string{}
	for verb, outcome := range in.Gates {
		gates[verb] = outcome
		if outcome == GateFail {
			failed = true
		}
	}

	result := ResultNotCollected
	switch {
	case failed:
		result = ResultFail
	case scanner == ScannerEnabled && allPass:
		result = ResultPass
	}

	ts := in.TimestampUnix
	return Pack{
		Event:             "cis-kubernetes-evidence",
		Cluster:           in.Cluster,
		TimestampUnix:     ts,
		GeneratedAt:       time.Unix(ts, 0).UTC().Format(time.RFC3339),
		MappingDoc:        mappingDocPath,
		ComplianceScanner: scanner,
		Reports:           reports,
		Supplemental:      in.Supplemental,
		Gates:             gates,
		Questionnaire:     in.Questionnaire,
		QuestionnairePath: in.QuestionnairePath,
		Result:            result,
	}
}

// NotCollectedReason explains a NOT_COLLECTED pack in one line.
func NotCollectedReason(p Pack) string {
	if p.ComplianceScanner != ScannerEnabled {
		return "the trivy-operator cluster-compliance scanner is " + p.ComplianceScanner +
			" (OPERATOR_CLUSTER_COMPLIANCE_ENABLED) — no compliance report is being scored"
	}
	var parts []string
	for _, r := range p.Reports {
		if r.Result() == ResultNotCollected {
			parts = append(parts, r.Name+": "+r.Reason)
		}
	}
	if len(parts) == 0 {
		return "no compliance report was expected"
	}
	return strings.Join(parts, "; ")
}

// ParseComplianceReport normalizes the JSON of one trivy-operator
// ClusterComplianceReport (`kubectl get clustercompliancereport <name> -o json`)
// into a ComplianceReport. It tolerates the schema variation across
// trivy-operator versions: it reads spec.compliance.{id,title},
// status.updateTimestamp, the rolled-up status.summary.{passCount,failCount}, and
// per-control failures from either status.summaryReport.controls[] or
// status.detailReport.results[]. It does not judge the report — EvaluateReport
// does.
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
			UpdateTimestamp string `json:"updateTimestamp"`
			Summary         struct {
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
		UpdatedAt: doc.Status.UpdateTimestamp,
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
	fmt.Fprintf(&b, "# Compliance Evidence Pack\n\n")
	fmt.Fprintf(&b, "**Cluster:** %s  \n", dash(p.Cluster))
	fmt.Fprintf(&b, "**Generated:** %s  \n", p.GeneratedAt)
	fmt.Fprintf(&b, "**Result:** %s\n\n", resultBadge(p.Result))
	if p.Result == ResultNotCollected {
		fmt.Fprintf(&b, "> ⚠️ Not collected because %s.\n\n", NotCollectedReason(p))
	}
	fmt.Fprintf(&b, "Evidence for the controls in `%s` (shipped with the landing-zone template). The managed LKE-Enterprise control plane is out of scope by design — see that document.\n\n", p.MappingDoc)

	fmt.Fprintf(&b, "## Compliance Reports (trivy-operator)\n\n")
	fmt.Fprintf(&b, "**Compliance scanner:** %s\n\n", p.ComplianceScanner)
	if len(p.Reports) == 0 {
		fmt.Fprintf(&b, "> ⚠️ No ClusterComplianceReport was expected or collected. Nothing was scored this run.\n\n")
	} else {
		fmt.Fprintf(&b, "| Report | Pass | Fail | Last reconciled | Result |\n|--------|------|------|-----------------|--------|\n")
		for _, r := range p.Reports {
			res := r.Result()
			if r.Reason != "" {
				res += " — " + r.Reason
			}
			fmt.Fprintf(&b, "| %s (`%s`) | %d | %d | %s | %s |\n", dash(r.Title), r.Name, r.PassCount, r.FailCount, dash(r.UpdatedAt), res)
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
	fmt.Fprintf(&b, "| NetworkPolicies present | %s |\n", notCollectedInt(p.Supplemental.NetworkPolicyCount))
	fmt.Fprintf(&b, "| Namespaces enforcing restricted PSS | %s |\n", restrictedNSStr(p.Supplemental.RestrictedNamespaces))
	fmt.Fprintf(&b, "| Default StorageClass encrypted | %s |\n\n", boolStr(p.Supplemental.EncryptedStorageClass))

	fmt.Fprintf(&b, "## Gates run with this pack\n\n")
	if len(p.Gates) == 0 {
		fmt.Fprintf(&b, "> ⚠️ No gate outcome was recorded with this pack — the rows below that cite a gate carry no live result.\n\n")
	} else {
		fmt.Fprintf(&b, "| Gate | Outcome |\n|------|---------|\n")
		for _, v := range sortedGateNames(p.Gates) {
			fmt.Fprintf(&b, "| `llz ci %s` | %s |\n", v, p.Gates[v])
		}
		b.WriteString("\n")
	}

	fmt.Fprintf(&b, "## Answered checklist\n\n")
	if p.Questionnaire == nil {
		fmt.Fprintf(&b, "> ⚠️ No questionnaire was provided (looked for `%s`). Copy `infosec/questionnaire.yaml.example` to `infosec/questionnaire.yaml` and answer it for your org's checklist.\n\n", dash(p.QuestionnairePath))
	} else {
		q := p.Questionnaire
		src := q.Metadata.Name
		if q.Metadata.Source != "" {
			src += " — " + q.Metadata.Source
		}
		fmt.Fprintf(&b, "**Questionnaire:** %s (`%s`)\n\n", src, dash(p.QuestionnairePath))
		fmt.Fprintf(&b, "Who satisfies each requirement: **LLZ** (enforced by the template), **Instance** (a value this instance must set), **Adopter** (organizational process). Gaps are stated, not rounded up.\n\n")
		fmt.Fprintf(&b, "| # | Requirement | LLZ provides | Instance configures | Adopter process | Gap | Evidence |\n|---|---|---|---|---|---|---|\n")
		for _, r := range q.Requirements {
			fmt.Fprintf(&b, "| %d | %s | %s | %s | %s | %s | %s |\n", r.Num, cell(r.Title), cell(r.LLZ), cell(r.Instance), cell(r.Adopter), cell(r.Gap), gateEvidence(r, p.Gates))
		}
		b.WriteString("\n")
	}

	fmt.Fprintf(&b, "## Sign-Off\n\n")
	fmt.Fprintf(&b, "Adopter-process rows are attested through your InfoSec process. Attach this pack and its `.sha256` as the evidence reference.\n\n")
	fmt.Fprintf(&b, "| Role | Name | Date | Approval ref |\n|------|------|------|--------------|\n")
	fmt.Fprintf(&b, "| Project / Product owner | | | |\n")
	fmt.Fprintf(&b, "| InfoSec reviewer | | | |\n")
	return b.String()
}

// gateEvidence renders a row's gates with their recorded outcome, if any.
func gateEvidence(r Requirement, outcomes map[string]string) string {
	if len(r.Gates) == 0 {
		return "—"
	}
	parts := make([]string, 0, len(r.Gates))
	for _, g := range r.Gates {
		o, ok := outcomes[g]
		if !ok {
			o = "not run"
		}
		parts = append(parts, fmt.Sprintf("`%s`: %s", g, o))
	}
	return strings.Join(parts, "<br>")
}

func sortedGateNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func resultBadge(r string) string {
	switch r {
	case ResultPass:
		return "✅ PASS"
	case ResultFail:
		return "❌ FAIL"
	case ResultNotCollected:
		return "⚠️ NOT COLLECTED — this pack is not evidence of compliance"
	default:
		return r
	}
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

// cell makes free text from the questionnaire safe inside a Markdown table cell.
func cell(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return dash(strings.ReplaceAll(s, "|", `\|`))
}
