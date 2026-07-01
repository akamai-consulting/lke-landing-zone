package main

// ci_cis_evidence.go implements `llz ci cis-evidence` — the auditor evidence-pack
// generator for the CIS Kubernetes Benchmark + org-security-control mapping
// (docs/infosec/control-mapping.md).
//
// It harvests the machine-readable evidence the platform already produces:
//   - the trivy-operator ClusterComplianceReports (the standard `cis` report and
//     the custom `llz-org-controls` report — see
//     instance-template/apl-values/_shared/manifest/compliance/),
//   - the credential-SLA result from `llz ci cred-audit` (--cred-audit-file),
//   - NetworkPolicy coverage, restricted-PSS namespaces, and the default
//     StorageClass's encryption parameter,
// folds them into one pack (internal/evidence), writes the JSON bundle + a
// Markdown summary an auditor signs, appends the step summary, and — with
// --attest — cosign-attests the bundle for chain-of-custody.
//
// Every harvest is best-effort: a missing CRD or an unreachable cluster records
// the signal as "not collected" rather than failing, so the pack is always
// produced. The pack's result is FAIL only when a collected compliance report or
// the credential audit actually failed.

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/cli"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/evidence"
	"github.com/spf13/cobra"
)

type cisEvidenceOpts struct {
	cluster       string
	outDir        string
	credAuditFile string
	attest        bool
	strict        bool
}

func ciCISEvidenceCmd() *cobra.Command {
	var o cisEvidenceOpts
	c := &cobra.Command{
		Use:   "cis-evidence",
		Short: "harvest CIS + org-control compliance evidence into a signed auditor pack",
		Long: "Builds the auditor evidence pack for docs/infosec/control-mapping.md. Harvests\n" +
			"the trivy-operator ClusterComplianceReports (cis + llz-org-controls), the\n" +
			"cred-audit SLA result (--cred-audit-file), NetworkPolicy coverage,\n" +
			"restricted-PSS namespaces, and the default StorageClass encryption parameter.\n" +
			"Writes <out-dir>/cis-evidence-<cluster>-<ts>.{json,md} and the step summary;\n" +
			"with --attest, cosign-attests the JSON bundle (keyless OIDC in CI). Every\n" +
			"harvest is best-effort — a missing CRD/unreachable cluster is recorded as\n" +
			"'not collected'. Exits non-zero only on a real compliance/credential failure\n" +
			"(or, with --strict, when nothing could be collected).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
			return runCICISEvidence(o)
		},
	}
	f := c.Flags()
	f.StringVar(&o.cluster, "cluster", os.Getenv("CLUSTER_NAME"), "cluster name for the pack heading (defaults to $CLUSTER_NAME)")
	f.StringVar(&o.outDir, "out-dir", cli.EnvOr("EVIDENCE_OUT_DIR", "."), "directory to write the pack into")
	f.StringVar(&o.credAuditFile, "cred-audit-file", os.Getenv("CRED_AUDIT_FILE"), "path to a `llz ci cred-audit` JSON record to fold in (optional)")
	f.BoolVar(&o.attest, "attest", cli.EnvBool("EVIDENCE_ATTEST", false), "cosign-attest the JSON bundle (keyless OIDC; no-op if cosign is absent)")
	f.BoolVar(&o.strict, "strict", cli.EnvBool("EVIDENCE_STRICT", false), "fail if no compliance evidence could be collected at all")
	return c
}

// attestBlob signs an in-toto attestation over the pack with cosign (keyless).
// A package var so tests can stub it; the real impl shells out only when cosign
// is on PATH.
var attestBlob = func(bundlePath, predicatePath string) error {
	if _, err := execLookPath("cosign"); err != nil {
		fmt.Fprintln(os.Stderr, "::warning::cosign not found on PATH — skipping attestation of the evidence pack.")
		return nil
	}
	// --yes: non-interactive (CI). Keyless signing uses the ambient OIDC token.
	// The exact predicate type is "custom"; the resulting attestation lands next
	// to the bundle. Validate the cosign version's flags in CI before relying on
	// the attestation gate.
	_, err := execOutput("cosign", "attest-blob", "--yes",
		"--predicate", predicatePath, "--type", "custom",
		"--bundle", bundlePath+".cosign.bundle", predicatePath)
	return err
}

func runCICISEvidence(o cisEvidenceOpts) error {
	now := time.Now().Unix()
	in := evidence.Inputs{
		Cluster:       firstNonEmpty(o.cluster, "unknown"),
		TimestampUnix: now,
		Supplemental: evidence.Supplemental{
			NetworkPolicyCount: -1, // -1 until collected
		},
	}

	// ── trivy-operator ClusterComplianceReports (graceful if CRD/report absent) ──
	in.CISReport = harvestComplianceReport("cis")
	in.OrgReport = harvestComplianceReport("llz-org-controls")

	// ── credential-SLA result (from a prior `llz ci cred-audit` record) ──
	if o.credAuditFile != "" {
		if res, err := readCredAuditResult(o.credAuditFile); err != nil {
			fmt.Fprintf(os.Stderr, "::warning::could not read cred-audit file %s (%v) — recording as not collected.\n", o.credAuditFile, err)
		} else {
			in.Supplemental.CredAuditResult = res
		}
	}

	// ── NetworkPolicy coverage ──
	if out, err := execOutput("kubectl", "get", "networkpolicy", "-A", "-o", "json"); err != nil {
		slog.Warn("could not list NetworkPolicies — recording as not collected", "err", err)
	} else if n, err := evidence.CountListItems(out); err == nil {
		in.Supplemental.NetworkPolicyCount = n
	}

	// ── restricted-PSS namespaces ──
	if out, err := execOutput("kubectl", "get", "namespaces", "-o", "json"); err != nil {
		slog.Warn("could not list namespaces — recording as not collected", "err", err)
	} else if ns, err := evidence.RestrictedNamespaces(out); err == nil {
		in.Supplemental.RestrictedNamespaces = ns
	}

	// ── default StorageClass encryption ──
	if out, err := execOutput("kubectl", "get", "storageclass", "-o", "json"); err != nil {
		slog.Warn("could not list storageclasses — recording as not collected", "err", err)
	} else if enc, err := evidence.DefaultStorageClassEncrypted(out); err == nil {
		in.Supplemental.EncryptedStorageClass = enc
	}

	pack := evidence.BuildPack(in)

	if o.strict && in.CISReport == nil && in.OrgReport == nil {
		return fmt.Errorf("no ClusterComplianceReport could be collected (trivy-operator not ready?) and --strict is set")
	}

	if err := writePack(o.outDir, pack); err != nil {
		return err
	}

	// One JSON line on stdout (the evidence record) + the rendered summary.
	if err := cli.PrintRecord(packRecord(pack)); err != nil {
		return err
	}
	md := evidence.RenderMarkdown(pack)
	if err := appendGHAFile("GITHUB_STEP_SUMMARY", md); err != nil {
		return err
	}

	if o.attest {
		base := packBaseName(pack)
		jsonPath := filepath.Join(o.outDir, base+".json")
		if err := attestBlob(jsonPath, jsonPath); err != nil {
			if o.strict {
				return fmt.Errorf("attest evidence pack: %w", err)
			}
			fmt.Fprintf(os.Stderr, "::warning::evidence pack attestation failed (%v) — pack written but unattested.\n", err)
		}
	}

	if pack.Result == evidence.ResultFail {
		fmt.Fprintf(os.Stderr, "::error::CIS evidence pack for %s reports FAIL — a compliance report or the credential audit failed. See the pack and docs/infosec/control-mapping.md.\n", pack.Cluster)
		return fmt.Errorf("cis evidence pack result: FAIL for %s", pack.Cluster)
	}
	fmt.Printf("CIS evidence pack written for %s (result: %s).\n", pack.Cluster, pack.Result)
	return nil
}

// harvestComplianceReport fetches and parses one ClusterComplianceReport,
// returning nil when the report/CRD is absent or unreadable (graceful).
func harvestComplianceReport(name string) *evidence.ComplianceReport {
	out, err := execOutput("kubectl", "get", "clustercompliancereport", name, "-o", "json")
	if err != nil {
		slog.Warn("ClusterComplianceReport not collected", "name", name, "err", err)
		return nil
	}
	rep, err := evidence.ParseComplianceReport(out)
	if err != nil {
		slog.Warn("could not parse ClusterComplianceReport", "name", name, "err", err)
		return nil
	}
	return &rep
}

// readCredAuditResult extracts the "result" field from a cred-audit JSON record.
// It decodes only the first JSON value so a captured `llz ci cred-audit` stdout
// (one JSON record line followed by the command's trailing human-readable
// summary) parses cleanly.
func readCredAuditResult(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var rec struct {
		Result string `json:"result"`
	}
	if err := json.NewDecoder(f).Decode(&rec); err != nil {
		return "", err
	}
	if rec.Result == "" {
		return "", fmt.Errorf("no result field in %s", path)
	}
	return rec.Result, nil
}

// writePack writes the JSON bundle and the Markdown summary into outDir.
func writePack(outDir string, p evidence.Pack) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", outDir, err)
	}
	base := packBaseName(p)
	jsonBytes, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(outDir, base+".json"), jsonBytes, 0o644); err != nil {
		return fmt.Errorf("write pack json: %w", err)
	}
	if err := os.WriteFile(filepath.Join(outDir, base+".md"), []byte(evidence.RenderMarkdown(p)), 0o644); err != nil {
		return fmt.Errorf("write pack markdown: %w", err)
	}
	fmt.Fprintf(os.Stderr, "evidence pack: %s.{json,md}\n", filepath.Join(outDir, base))
	return nil
}

func packBaseName(p evidence.Pack) string {
	return fmt.Sprintf("cis-evidence-%s-%d", p.Cluster, p.TimestampUnix)
}

// packRecord flattens the pack into the single-line stdout record (the SLA-style
// evidence line CI parses), mirroring cred-audit's shape.
func packRecord(p evidence.Pack) map[string]any {
	reports := make([]any, 0, len(p.Reports))
	for _, r := range p.Reports {
		reports = append(reports, map[string]any{
			"name": r.Name, "id": r.ID, "pass": r.PassCount, "fail": r.FailCount, "result": r.Result(),
		})
	}
	return map[string]any{
		"event":                p.Event,
		"timestamp_unix":       p.TimestampUnix,
		"cluster":              p.Cluster,
		"compliance_reports":   reports,
		"cred_audit_result":    p.Supplemental.CredAuditResult,
		"network_policy_count": p.Supplemental.NetworkPolicyCount,
		"restricted_ns_count":  len(p.Supplemental.RestrictedNamespaces),
		"result":               p.Result,
	}
}
