package identityconfig

import (
	"regexp"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
)

// THE SECRET HALF OF spec.alerting IS A SPLIT CONTRACT. clusterspec renders an
// ExternalSecret that reads alerts/* through the `openbao` store, and that store
// authenticates as the `eso` role, which carries platform-ci. If a path the
// renderer reads is missing here, ESO gets a 403, the Secret never appears, the
// prometheus-operator rejects the AlertmanagerConfig, and the pager is silent —
// with every manifest valid. That is the "BOTH HALVES DEAD" history
// docs/upstream-asks.md §4 records, so both sides are read from the code that
// ships: the renderer's real refs against the real policy constant.
func TestESOCanReadEveryAlertingSecretTheRendererReferences(t *testing.T) {
	refs := clusterspec.AlertingOpenBaoRefs(clusterspec.Alerting{Receivers: []string{"pagerduty", "slack"}})
	if len(refs) == 0 {
		t.Fatal("the renderer references no OpenBao path for pagerduty+slack — nothing to check, which is itself the bug")
	}
	for _, ref := range refs {
		data := regexp.MustCompile(`(?m)^path "secret/data/` + regexp.QuoteMeta(ref) + `"\s+\{ capabilities = \["read"\] \}$`)
		if !data.MatchString(policyPlatformCI) {
			t.Errorf("platform-ci (the eso role) cannot read secret/data/%s — the rendered ExternalSecret would 403 "+
				"and the AlertmanagerConfig would be rejected for want of its Secret", ref)
		}
		// Metadata read is what the reconciler's credential-age gauge samples; a
		// path ESO consumes and nothing ages is a key that never gets rotated.
		meta := regexp.MustCompile(`(?m)^path "secret/metadata/` + regexp.QuoteMeta(ref) + `"\s+\{ capabilities = \["read"\] \}$`)
		if !meta.MatchString(policyReconcilerRead) {
			t.Errorf("reconciler-read has no metadata read on secret/metadata/%s — its age is never measured", ref)
		}
	}
}
