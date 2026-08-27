package onboard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/templatecommit"
)

// A TICK IS AN ASSERTION. doctor printed "TF_IMAGE / KUBE_IMAGE match the
// template pin" whenever the skew slice came back empty — including when
// computeCIImageVars had no commit-pinned answer to compare against, which is the
// normal state in the minutes after a release while the images for the new pin
// are still publishing.
//
// That is not a cosmetic overstatement. It is the readiness report `llz upgrade`
// runs as its closing verdict, and on akamai/gsap-apl's v0.0.48 upgrade it ticked
// green minutes before `llz tokens` re-pinned both variables off the previous
// commit.
//
// REPORTED, NOT DECIDED: doctor cannot see whether the variables are right when
// there is nothing to compare them to, so it must not fail on the guess either.
func TestDoctorDoesNotTickAnImagePinItCouldNotCheck(t *testing.T) {
	const sha = "b9fe2721b55e2cb196d418f8d0bc6069957e3bd3"
	recorded := func(k string) string {
		return map[string]string{
			"TF_IMAGE":   "ghcr.io/akamai-consulting/ci-tofu:sha-0000000000000000000000000000000000000000",
			"KUBE_IMAGE": "ghcr.io/akamai-consulting/ci-kubernetes:sha-0000000000000000000000000000000000000000",
		}[k]
	}

	dir := chdirTempDir(t)
	if err := os.WriteFile(filepath.Join(dir, ".copier-answers.yml"),
		[]byte("_commit: "+sha+"\nllz_version: "+sha+"\ninstance_repo: akamai/example\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prev := templatecommit.ImagePublished
	t.Cleanup(func() { templatecommit.ImagePublished = prev })
	templatecommit.ImagePublished = func(string) (bool, bool) { return false, true }

	var err error
	out := captureStdout(t, func() { err = checkCIImagePins("llz tokens --env prod --yes", recorded) })

	if strings.Contains(out, "match the template pin") {
		t.Errorf("doctor claimed the pins match on a comparison that never ran:\n%s", out)
	}
	if !strings.Contains(out, "not verified") {
		t.Errorf("doctor said nothing about the check it could not make:\n%s", out)
	}
	if !strings.Contains(out, "llz tokens --env prod --yes") {
		t.Errorf("doctor named no way forward:\n%s", out)
	}
	if err != nil {
		t.Errorf("checkCIImagePins = %v, want nil — doctor cannot see whether these are "+
			"right, so it may report but must not decide", err)
	}
}
