package tokeninv

// validate.go — `llz ci validate-tokens`: the CI counterpart of the
// local `llz doctor` validity probe. It reads each pipeline credential from the
// ENVIRONMENT (where CI injects the repo/infra-<env> secrets) and actively probes
// it, so a set-but-dead token — the GHCR_READ_TOKEN 403 that failed a run
// mid-bootstrap being the motivating case — fails FAST with "rotate it" instead
// of 401/403-ing deep inside a 45-minute provision.
//
// It probes two independent things per credential: VALIDITY (does it
// authenticate? — token_validate.go) and CAPABILITY (is it scoped for the job it
// exists for? — token_capability.go). The second exists because the first is not
// sufficient: an under-scoped PAT authenticates perfectly and still 403s on the
// operation, which is how a "✓ valid, expires in 77d" verdict was followed six
// minutes later by `gh secret set --env infra-prod` failing 403 — after the
// cluster was already up. See token_capability.go for that scar in full.
//
// This is what closes the gap the local wizard can't: GitHub exposes secret
// values only inside the job, never to `llz doctor` on a laptop. Wire it as an
// early preflight in a workflow that already has the credentials in env. Probe
// logic is shared with token_validate.go (probeToken); this file is the env read
// + exit-code shell. Exit 0 all-valid (or only warnings/unreachable), 1 if any
// probed credential is INVALID and --fail-on-invalid (default).

import (
	"fmt"
	"os"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cigate"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/envreq"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/tokenprobe"
)

// validatableTokens is the ordered set of pipeline credentials this verb probes
// from the environment. Only auth-bearing tokens with a probe (tokenprobe.KindFor != none)
// belong here; each is checked only when its env var is set.
var validatableTokens = []string{
	"LINODE_API_TOKEN",
	"LINODE_DNS_TOKEN",
	"OPENBAO_SECRETS_WRITE_TOKEN",
	"APL_VALUES_REPO_TOKEN",
	"E2E_DISPATCH_TOKEN",
	"GHCR_READ_TOKEN",
}

// optional reports whether a credential may be invalid or under-scoped without
// blocking the run — GHCR_READ_TOKEN (the charts are public, and ghcrPullToken
// falls back to anonymous) and LINODE_DNS_TOKEN (DNS-01 certs are opt-in), today.
// An invalid or denied REQUIRED token (Linode API, the GitHub PATs) is a hard
// fail: it WILL break the run downstream.
//
// IT READS THE REQUIREMENT TABLE, IT IS NOT A COPY OF IT. This was a hand-kept
// map of two names sitting beside envreq.E2ERequirements, which carries the same
// fact in its Required column for every credential — two lists, agreeing by
// nobody's arrangement, in a repo where configreadiness and secretscope both
// already read the table and say so in their comments. They happened to agree,
// which is the state a divergence starts from; and `llz doctor` decides the same
// question off r.Required, so the two commands' claim to "agree on what stops a
// build" rested on that coincidence holding.
//
// FAIL CLOSED ON AN UNKNOWN NAME. A credential absent from the table is treated
// as REQUIRED (blocking), never as optional — an entry that quietly stopped
// gating would be indistinguishable from one that never gated. That cannot happen
// silently either: TestEveryValidatableTokenIsInTheRequirementTable asserts the
// two sets line up.
//
// admin=true is the SUPERSET — E2E_DISPATCH_TOKEN exists only on that side of the
// table, and reading the non-admin list would make it an unknown name here.
func optional(name string) bool {
	for _, r := range envreq.E2ERequirements(true) {
		if r.Name == name {
			return !r.Required
		}
	}
	return false // unknown → treated as required
}

// runCIValidateTokens returns nil when nothing blocking is invalid and an error
// otherwise (cobra exits 1 on it). The ::error:: annotation stays a direct
// write: GitHub parses an annotation only at the start of a line, and a returned
// error reaches stderr behind main.go's "llz: " prefix.
// RunValidate probes every pipeline credential for validity AND capability.
func RunValidate(failOnInvalid bool) error {
	now := time.Now()
	ghcrUser := os.Getenv("GHCR_USERNAME")
	// The deployment the scope checks are asked about. In CI both halves are
	// ambient; `llz doctor` builds the same struct from what the operator typed.
	capCtx := tokenprobe.EnvCapContext()
	// A grant whose consumer this deployment does not deploy is not a finding.
	// The workflow runs with the instance checked out, so the spec is right here;
	// an unreadable one yields an empty set and every check still runs.
	capCtx.ComponentOff = clusterspec.DisabledComponents(capCtx.Region)

	fmt.Printf("%s\n", color.Bold("Token validity — probing pipeline credentials in the environment"))
	probed, blockingInvalid, optionalInvalid, blockingDenied, inertRoutes := 0, 0, 0, 0, 0
	for _, name := range validatableTokens {
		val := os.Getenv(name)
		if val == "" {
			fmt.Printf("  %-30s %s\n", name, color.Dim("– not set — skipped"))
			continue
		}
		// Keep the secret value out of any downstream log capture.
		fmt.Fprintf(os.Stderr, "::add-mask::%s\n", val)
		tv := tokenprobe.ProbeToken(name, val, ghcrUser, now)
		probed++
		suffix := ""
		if tv.Status == tokenprobe.VInvalid {
			if optional(name) {
				optionalInvalid++
				suffix = color.Dim("  (optional — warning only)")
				fmt.Fprintf(os.Stderr, "::warning::%s is invalid but optional — it won't block the run; rotate or unset it.\n", name)
			} else {
				blockingInvalid++
			}
		}
		fmt.Printf("  %-30s %s%s\n", name, tokenprobe.ValidityCell(tv), suffix)

		// Authorization, reported as an indented child of the validity line. Asked
		// only of a credential that authenticated: a dead token has nothing to
		// authorize, and a second verdict would just bury the real cause.
		if tv.Status == tokenprobe.VInvalid {
			continue
		}
		// EVERY registered check, not the first: OPENBAO_SECRETS_WRITE_TOKEN needs
		// two different GitHub permissions (Environments: write for the build's own
		// writeback, repo-level Secrets for the in-cluster harbor-robot-provisioner),
		// and reporting one of them is how the other stayed unmeasured until a
		// CronJob 403ed for a month.
		// deniedHere, not blockingDenied++, because the counter is of CREDENTIALS
		// and this loop is over that credential's GRANTS. Incrementing per refusal
		// reported one under-scoped PAT as "2 REQUIRED pipeline credential(s)",
		// sending the reader to look for a second broken token.
		deniedHere := false
		for _, cr := range tokenprobe.CheckCapabilities(capCtx, name, val) {
			switch {
			case cr.Status == tokenprobe.CapDenied && optional(name):
				fmt.Fprintf(os.Stderr, "::warning::%s is not authorized for its required scope but is optional — it won't block the run.\n", name)
			case cr.Status == tokenprobe.CapDenied:
				deniedHere = true
				fmt.Fprintf(os.Stderr, "::error::%s: %s\n", name, tokenprobe.CapabilityHint(name, cr.Op))
			case cr.Status == tokenprobe.CapRouteRefused:
				// ANNOTATED, NEVER BLOCKING, AND COUNTED. The credential is correctly
				// scoped, so there is nothing to fail it for — but a downstream check has
				// been proven unanswerable in this pipeline, and that is exactly the finding
				// that used to exist only as a warning inside the inert gate's own green
				// step. cigate.Warning so the reason survives into the annotation instead of
				// being truncated at the first newline.
				inertRoutes++
				fmt.Fprintln(os.Stderr, cigate.Warning(fmt.Sprintf("%s: %s", name, cr.Detail)))
			}
			fmt.Printf("  %-30s %s\n", "  └ scope", tokenprobe.CapabilityCell(cr))
		}
		if deniedHere {
			blockingDenied++
		}
	}

	// OBJ state-bucket key pair (REQUIRED) — validated together via SigV4.
	if ak, sk := os.Getenv("TF_STATE_ACCESS_KEY"), os.Getenv("TF_STATE_SECRET_KEY"); ak != "" && sk != "" {
		fmt.Fprintf(os.Stderr, "::add-mask::%s\n", ak)
		fmt.Fprintf(os.Stderr, "::add-mask::%s\n", sk)
		tv := tokenprobe.ProbeS3Pair(ak, sk, os.Getenv("TF_STATE_ENDPOINT"), os.Getenv("TF_STATE_BUCKET"))
		probed++
		if tv.Status == tokenprobe.VInvalid {
			blockingInvalid++
		}
		fmt.Printf("  %-30s %s\n", "TF_STATE_ACCESS_KEY/SECRET", tokenprobe.ValidityCell(tv))
	}

	fmt.Printf("\nprobed %d credential(s): %d blocking-invalid, %d optional-invalid, %d scope-denied, %d route-refused.\n",
		probed, blockingInvalid, optionalInvalid, blockingDenied, inertRoutes)

	// Denial and invalidity are reported separately because the remediation
	// differs: an invalid token needs ROTATING, a denied one needs RE-SCOPING, and
	// telling an operator to rotate a perfectly live PAT sends them down the wrong
	// path. Both are gated by --fail-on-invalid — one switch for "report only".
	if failOnInvalid {
		switch {
		case blockingInvalid > 0 && blockingDenied > 0:
			fmt.Fprintf(os.Stderr, "::error::%d REQUIRED credential(s) are invalid and %d lack a required scope — fix both before this run proceeds.\n", blockingInvalid, blockingDenied)
			return fmt.Errorf("%d REQUIRED pipeline credential(s) are invalid and %d lack a required scope", blockingInvalid, blockingDenied)
		case blockingInvalid > 0:
			fmt.Fprintf(os.Stderr, "::error::%d REQUIRED pipeline credential(s) are invalid — rotate them before this run proceeds.\n", blockingInvalid)
			return fmt.Errorf("%d REQUIRED pipeline credential(s) are invalid — rotate them before this run proceeds", blockingInvalid)
		case blockingDenied > 0:
			fmt.Fprintf(os.Stderr, "::error::%d REQUIRED pipeline credential(s) authenticate but lack a required scope — re-scope them (NOT rotate) before this run proceeds.\n", blockingDenied)
			return fmt.Errorf("%d REQUIRED pipeline credential(s) lack a required scope — re-scope them before this run proceeds", blockingDenied)
		}
	}
	return nil
}
