package doctor

// token_table.go — the two halves of the old token_validate.go that did
// NOT go to internal/tokeninv.
//
// They read the same credentials, which is why they were filed together, but
// they answer a different question for a different caller: these render `llz
// doctor`'s readiness TABLE from the local .llz cache, keyed by the wizard's
// `requirement`/`liveState`. The extension probes credentials in CI and returns a
// VERDICT. Sharing a subject is not sharing a purpose — the coupling to the
// wizard is what makes these package main's.

import (
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/envreq"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/tokenprobe"
)

// ProbeTokenValidities probes every probeable requirement and returns a verdict
// keyed by credential NAME, plus the count of INVALID ones. It does NOT print —
// envreq.ReportReadiness renders the results as the table's VALID column. A probeable
// token with no locally-readable value gets a vSkipped verdict (probe it in CI);
// non-credential requirements (plain vars, image refs) get no entry.
func ProbeTokenValidities(reqs []envreq.Requirement, secrets, vars map[string]string, instance envreq.LiveState, ghcrUser string) (map[string]tokenprobe.TokenValidity, int) {
	now := time.Now()
	out := map[string]tokenprobe.TokenValidity{}

	// The OBJ state-bucket key PAIR is validated together (both keys + endpoint +
	// bucket, via SigV4); the one verdict is mirrored onto both rows so neither
	// shows a bare N/A. Values come from the local .llz cache.
	endpoint := firstNonEmpty(vars["TF_STATE_ENDPOINT"], instance.Value("TF_STATE_ENDPOINT"))
	bucket := firstNonEmpty(vars["TF_STATE_BUCKET"], instance.Value("TF_STATE_BUCKET"))
	s3v := tokenprobe.ProbeS3Pair(secrets["TF_STATE_ACCESS_KEY"], secrets["TF_STATE_SECRET_KEY"], endpoint, bucket)

	invalid := 0
	for _, r := range reqs {
		k := tokenprobe.KindFor(r.Name)
		if k == tokenprobe.KindNone {
			continue // not a probeable credential (plain vars, image refs, …)
		}
		if k == tokenprobe.KindS3 {
			tv := s3v
			tv.Name = r.Name
			// No local value but set on GitHub → clarify it's a cache miss, not absent.
			if tv.Status == tokenprobe.VSkipped && strings.HasPrefix(tv.Detail, "not cached") && instance.Has(r.Name, true) {
				tv.Detail = "set on GitHub — not in .llz cache; gather locally or use `llz ci validate-tokens`"
			}
			out[r.Name] = tv
			if r.Name == "TF_STATE_ACCESS_KEY" && tv.Status == tokenprobe.VInvalid {
				invalid++ // count the pair once
			}
			continue
		}
		val, haveLocal := localValue(r, secrets, vars)
		if !haveLocal {
			// No local value: distinguish "set on GitHub, just not cached" from
			// "not configured anywhere" — neither is a bare N/A.
			if instance.Has(r.Name, r.Secret) {
				out[r.Name] = tokenprobe.TokenValidity{Name: r.Name, Status: tokenprobe.VSkipped, Detail: "set on GitHub — not in .llz cache; gather locally or use `llz ci validate-tokens`"}
			} else {
				out[r.Name] = tokenprobe.TokenValidity{Name: r.Name, Status: tokenprobe.VSkipped, Detail: "not set"}
			}
			continue
		}
		tv := tokenprobe.ProbeToken(r.Name, val, ghcrUser, now)
		if tv.Status == tokenprobe.VInvalid {
			invalid++
		}
		out[r.Name] = tv
	}
	return out, invalid
}

// localValue returns a requirement's value from the local .llz cache (secrets or
// vars, by kind) and whether it was present.
func localValue(r envreq.Requirement, secrets, vars map[string]string) (string, bool) {
	m := vars
	if r.Secret {
		m = secrets
	}
	v, ok := m[r.Name]
	return v, ok && v != ""
}

// ProbeTokenCapabilities probes AUTHORIZATION for every requirement whose value
// is readable locally, and returns the verdicts keyed by credential NAME plus
// the count of REQUIRED CREDENTIALS that were denied at least one grant.
//
// CREDENTIALS, NOT CHECKS, and the distinction is not pedantic: a credential can
// be refused several grants, and counting the refusals made one under-scoped PAT
// report as "2 required credential(s) … lack a required permission". The caller
// renders that number into a sentence whose noun is "credential", so the counter
// has to mean what the sentence says or the operator goes looking for a second
// broken token that does not exist. Like ProbeTokenValidities
// it does not print — envreq.ReportReadiness renders the results as the table's
// PERMS column.
//
// WHY THE LOCAL SIDE NEEDED THIS AT ALL. Authorization probing shipped CI-only,
// on the reasoning that GitHub never hands a secret VALUE to a laptop. True of a
// secret that lives only on GitHub — and irrelevant to the ones the operator
// gathered, which sit in .llz/secrets.env and which the validity probe beside
// this one has always read from there. So the table said
//
//	OPENBAO_SECRETS_WRITE_TOKEN  secret  REQUIRED  ✓ set  ⚠ warn (expires in 10d)
//
// about a PAT that could not write the repo-level secrets the cluster needs, and
// the operator's next command was `llz build`. Every column was accurate; the one
// that would have said "under-scoped" was not asked. That is the whole defect:
// not a wrong answer, a question deferred to a place nobody was reading.
//
// SKIPPED, NEVER FAILED, WHEN THE VALUE IS NOT LOCAL. A secret set on GitHub but
// absent from the cache cannot be probed here and reports as much — `llz ci
// validate-tokens` asks the same catalog in CI where the value IS in the
// environment.
func ProbeTokenCapabilities(reqs []envreq.Requirement, secrets, vars map[string]string, instance envreq.LiveState, cc tokenprobe.CapContext) (map[string][]tokenprobe.CapabilityResult, int) {
	out := map[string][]tokenprobe.CapabilityResult{}
	denied := 0
	for _, r := range reqs {
		val, haveLocal := localValue(r, secrets, vars)
		if !haveLocal {
			// Only say something for a credential that HAS a scope requirement —
			// otherwise every plain variable would grow an empty note.
			ops := tokenprobe.CapabilityChecksFor(r.Name)
			if len(ops) == 0 {
				continue
			}
			// INSTANCE STATE EVEN FOR A TEMPLATE-SCOPED REQUIREMENT, which is wrong
			// the moment one of those gets a capability check. envreq.Requirement.Template
			// marks a credential that lives on the TEMPLATE repo (the e2e harness's
			// own), and ReportReadiness picks the template LiveState for those; this
			// asks the instance, so such a credential would be described as absent
			// when it is merely elsewhere.
			//
			// LEFT AS PARITY, DELIBERATELY. No Template requirement has a registered
			// check today, so the branch is unreachable, and ProbeTokenValidities
			// beside it has the identical pre-existing shape — fixing one and not the
			// other would replace a latent bug with a live inconsistency, and fixing
			// both means a signature change on a function this branch did not
			// otherwise touch. Whoever registers the first template-scoped check
			// should thread the template LiveState through both and delete this note;
			// it affects the skip WORDING only, never a verdict.
			detail := tokenprobe.SkipNotSet
			if instance.Has(r.Name, r.Secret) {
				detail = "set on GitHub — not in .llz cache; gather locally or use `llz ci validate-tokens`"
			}
			// ONE SKIP PER REGISTERED CHECK, each naming its own op. A single
			// Op-less row would say "we could not ask" about a credential with two
			// separate grants without saying which two went unasked — and it is the
			// only result in the model that a hint cannot be looked up for, in a
			// package that had just been made plural precisely so a credential's
			// grants stop being spoken about as one thing.
			skips := make([]tokenprobe.CapabilityResult, 0, len(ops))
			for _, op := range ops {
				skips = append(skips, tokenprobe.CapabilityResult{Name: r.Name, Op: op, Status: tokenprobe.CapSkipped, Detail: detail})
			}
			out[r.Name] = skips
			continue
		}
		rs := tokenprobe.CheckCapabilities(cc, r.Name, val)
		if len(rs) == 0 {
			continue
		}
		out[r.Name] = rs
		// An OPTIONAL credential's denial is reported and does not count: the same
		// rule `llz ci validate-tokens` applies, so doctor and CI agree on what
		// stops a build.
		if !r.Required {
			continue
		}
		if anyDenied(rs) {
			denied++
		}
	}
	return out, denied
}

// anyDenied reports whether any of a credential's checks was refused. One
// credential, one vote — see ProbeTokenCapabilities on why the count is of
// credentials rather than of refusals. Unexported: it has no caller outside this
// package, and an exported helper nothing needs is API surface someone has to
// keep working.
func anyDenied(rs []tokenprobe.CapabilityResult) bool {
	return tokenprobe.AnyStatus(rs, tokenprobe.CapDenied)
}
