package teardown

import (
	"context"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/instanceresolve"
)

// stubAccountRegions points accountRegions' client at a fake listing.
func stubAccountRegions(t *testing.T, ids []string) {
	t.Helper()
	orig := instanceresolve.RegionClient
	t.Cleanup(func() { instanceresolve.RegionClient = orig })
	if ids == nil {
		instanceresolve.RegionClient = func() instanceresolve.RegionLister { return nil }
		return
	}
	instanceresolve.RegionClient = func() instanceresolve.RegionLister { return fakeRegionLister{ids} }
}

type fakeRegionLister struct{ ids []string }

func (f fakeRegionLister) ListRegions(context.Context) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(f.ids))
	for _, id := range f.ids {
		out = append(out, map[string]any{"id": id})
	}
	return out, nil
}

func TestReapRefusesADeploymentNameAsRegion(t *testing.T) {
	// THE trap: `llz reap --region` takes a LINODE region, while almost every
	// `llz ci … --region` takes the DEPLOYMENT name. The sweeps compare the value
	// verbatim against each resource's region, so a deployment name matches
	// nothing, every section prints "none matched", and the run ends `deleted=0` —
	// which reads as "the account is clean". A false all-clear has no later signal
	// to catch it, which is why this refuses rather than warns.
	stubAccountRegions(t, []string{"us-ord", "us-sea", "de-fra"})

	err := checkReapRegion("lab")
	if err == nil {
		t.Fatal("a deployment name must be refused — the sweep would silently match nothing")
	}
	for _, want := range []string{"not a Linode region", "report a clean account", "DEPLOYMENT name"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal missing %q:\n%s", want, err)
		}
	}
}

func TestReapAcceptsARealRegionAndAccountWide(t *testing.T) {
	stubAccountRegions(t, []string{"us-ord", "us-sea"})
	if err := checkReapRegion("us-ord"); err != nil {
		t.Errorf("a real region must pass: %v", err)
	}
	// Account-wide is a legitimate, clearly-labelled scope — not a mistake.
	if err := checkReapRegion(""); err != nil {
		t.Errorf("account-wide must pass: %v", err)
	}
}

func TestReapRegionSuggestsTheNearestMatch(t *testing.T) {
	stubAccountRegions(t, []string{"us-ord", "us-sea", "us-east"})
	err := checkReapRegion("us-orb")
	if err == nil || !strings.Contains(err.Error(), "Did you mean") {
		t.Errorf("a typo should get a suggestion, got: %v", err)
	}
}

func TestReapRegionDegradesWhenTheAccountCannotAnswer(t *testing.T) {
	// Best-effort, like every other account-side check: an unanswerable lookup
	// must not block a sweep that would have worked. `llz reap` also runs during
	// incidents, when the last thing an operator needs is a new gate.
	stubAccountRegions(t, nil) // no token
	if err := checkReapRegion("anything"); err != nil {
		t.Errorf("must not block when the account cannot be asked: %v", err)
	}
}

func TestLinodeRegionForDeploymentIsQuietOutsideAnInstance(t *testing.T) {
	// `llz reap` legitimately runs from anywhere (an operator with just a token),
	// so the deployment lookup must be silent rather than an error path.
	chdirTempDir(t)
	if got := linodeRegionForDeployment("lab"); got != "" {
		t.Errorf("got %q outside an instance, want empty", got)
	}
}
