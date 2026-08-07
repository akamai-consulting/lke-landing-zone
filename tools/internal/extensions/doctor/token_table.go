package doctor

// doctor_token_table.go — the two halves of the old token_validate.go that did
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

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/tokeninv"
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
	s3v := tokeninv.ProbeS3Pair(secrets["TF_STATE_ACCESS_KEY"], secrets["TF_STATE_SECRET_KEY"], endpoint, bucket)

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
