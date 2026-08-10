package bootstrapcluster

// dns_token_notice.go — name the state an unset LINODE_DNS_TOKEN puts the cluster
// in, at the moment it is entered.
//
// LINODE_DNS_TOKEN is genuinely optional: `llz tokens` offers it with "Enter to
// skip", the requirement table marks it not-required, the quickstart says the
// cluster still bootstraps, and internal/shared/health/allowlists.go expects external-dns
// to be degraded without it. That design is fine.
//
// What was missing is that nobody SAYS so. The workflow substitutes a literal
// placeholder — `placeholder-set-LINODE_DNS_TOKEN-to-enable-dns` — rather than an
// empty value, because the apl-values var contract requires the key to be
// present. So apl-core is handed a syntactically valid credential that cannot
// authenticate, and the resulting failures are auth errors from
// cert-manager-webhook-linode and external-dns rather than "this feature is off".
// An operator who pressed Enter forty minutes earlier meets a converged cluster
// whose public certificates never issue, with nothing connecting the two.
//
// So: one line, at bootstrap, in the job summary. No behaviour change — this is
// the missing sentence, not a missing feature.

import (
	"fmt"
	"os"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghaout"
)

// dnsTokenPlaceholder is the literal llz-bootstrap-openbao.yml substitutes when
// the secret is unset. Duplicated deliberately (a workflow default cannot import
// a Go constant); the notice degrades to the empty-string case if they diverge,
// which is the safe direction.
const dnsTokenPlaceholder = "placeholder-set-LINODE_DNS_TOKEN-to-enable-dns"

// dnsTokenProvisioned reports whether the value is a usable credential rather
// than the unset sentinel.
func dnsTokenProvisioned(tok string) bool {
	t := strings.TrimSpace(tok)
	return t != "" && t != dnsTokenPlaceholder
}

// reportDNSTokenPlaceholder states, once, what an unprovisioned DNS token means
// for the cluster being bootstrapped.
func reportDNSTokenPlaceholder(tok string) {
	if dnsTokenProvisioned(tok) {
		return
	}
	fmt.Fprintf(os.Stderr, "::warning::LINODE_DNS_TOKEN is not set — DNS-01 certificate issuance and ExternalDNS are DISABLED for this cluster.\n")
	fmt.Fprintln(os.Stderr, "  apl-core receives a placeholder in dns.provider.linode.apiToken, so external-dns")
	fmt.Fprintln(os.Stderr, "  and cert-manager-webhook-linode will fail to authenticate rather than sit idle —")
	fmt.Fprintln(os.Stderr, "  that is expected here, and external-dns is allowlisted in the health check.")
	fmt.Fprintln(os.Stderr, "  Public certificates will NOT issue until you provide one:")
	fmt.Fprintln(os.Stderr, "    gh secret set LINODE_DNS_TOKEN --env infra-<env>   # Linode PAT, Domains: read_write")
	fmt.Fprintln(os.Stderr, "  then re-dispatch the apply — the letsencrypt ClusterIssuers sync via Argo CD.")
	// Also into the run summary: the operator reading this a day later is reading
	// the summary, not scrolling a 40-minute log.
	if err := ghaout.Append("GITHUB_STEP_SUMMARY",
		"> ⚠️ **`LINODE_DNS_TOKEN` was not set** — DNS-01 certificate issuance and ExternalDNS are disabled "+
			"for this cluster. Public certificates will not issue. Set the secret on `infra-<env>` and re-dispatch the apply.\n"); err != nil {
		fmt.Fprintf(os.Stderr, "  (could not write the run summary: %v)\n", err)
	}
}
