package upgrade

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/templatecommit"
)

// THE GATE ON THE MISSING STEP. `llz upgrade`'s whole deliverable at this point
// is the checklist, and the re-pin is the item on it with a deadline: TF_IMAGE
// and KUBE_IMAGE are computed FROM the pin the upgrade just moved, so they are
// stale by construction and the first pipeline run fails `llz ci
// assert-image-fresh` until something re-pins them.
//
// It went missing because the reporter returned "" whenever no commit-pinned
// answer existed — and that is the NORMAL state right after a release, while
// build-images.yml is still publishing the images the new pin names. Measured on
// akamai/gsap-apl's v0.0.47 → v0.0.48: three steps, none of them the re-pin, and
// `llz tokens` afterwards re-pinned both variables off the previous commit.
//
// A 40-hex ref resolves without a round-trip, so stubbing the registry is enough
// to keep this offline.
func instanceWithRecordedImages(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mkdirAll(t, filepath.Join(dir, ".llz"))
	write(t, filepath.Join(dir, ".copier-answers.yml"), "instance_repo: akamai/example\n")
	write(t, filepath.Join(dir, ".llz", "vars.env"),
		"TF_IMAGE=ghcr.io/akamai-consulting/ci-tofu:sha-0000000000000000000000000000000000000000\n"+
			"KUBE_IMAGE=ghcr.io/akamai-consulting/ci-kubernetes:sha-0000000000000000000000000000000000000000\n")
	return dir
}

func stubImagePublished(t *testing.T, published, asked bool) {
	t.Helper()
	prev := templatecommit.ImagePublished
	t.Cleanup(func() { templatecommit.ImagePublished = prev })
	templatecommit.ImagePublished = func(string) (bool, bool) { return published, asked }
}

const testPinSHA = "b9fe2721b55e2cb196d418f8d0bc6069957e3bd3"

func TestAnUnverifiableImagePinStillReachesTheChecklist(t *testing.T) {
	chdir(t, instanceWithRecordedImages(t))
	stubImagePublished(t, false, true) // the release's images are not up yet

	var step string
	_, errOut := captureStdoutStderr(t, func() { step = reportCIImageSkew(testPinSHA) })

	if step == "" {
		t.Fatal("no next step for a pin this run could not verify — the operator's checklist " +
			"omits the one instruction with a deadline, which is exactly how gsap-apl's " +
			"v0.0.48 upgrade shipped with both image variables naming the previous commit")
	}
	if !strings.Contains(step, "llz tokens") {
		t.Errorf("next step = %q, want it to name `llz tokens`", step)
	}
	if !strings.Contains(errOut, "could not check") {
		t.Errorf("stderr = %q, want it to say the check did not happen", errOut)
	}
	// IT MUST NOT NAME A VALUE. With no commit-pinned answer the only thing to
	// re-pin onto is the floating tags, and #407 exists to keep instances off
	// them; `llz tokens` refuses to write them for the same reason.
	if strings.Contains(errOut, "gh variable set") {
		t.Error("advised a `gh variable set` with no pinned answer to set — that pins the " +
			"instance onto the floating tags this whole mechanism exists to avoid")
	}
}

// The verified paths must keep their old behaviour: a real skew still reports the
// have/want pair, and an instance already on the pin still contributes nothing —
// a checklist that lists a step which does not apply stops being read.
func TestVerifiedImagePinsAreUnchanged(t *testing.T) {
	t.Run("real skew still reports", func(t *testing.T) {
		chdir(t, instanceWithRecordedImages(t))
		stubImagePublished(t, true, true)

		var step string
		_, errOut := captureStdoutStderr(t, func() { step = reportCIImageSkew(testPinSHA) })
		if step == "" || !strings.Contains(errOut, "sha-"+testPinSHA) {
			t.Errorf("a genuine skew stopped reporting: step=%q stderr=%q", step, errOut)
		}
	})

	t.Run("already pinned is silent", func(t *testing.T) {
		dir := t.TempDir()
		mkdirAll(t, filepath.Join(dir, ".llz"))
		write(t, filepath.Join(dir, ".copier-answers.yml"), "instance_repo: akamai/example\n")
		write(t, filepath.Join(dir, ".llz", "vars.env"),
			"TF_IMAGE=ghcr.io/akamai-consulting/ci-tofu:sha-"+testPinSHA+"\n"+
				"KUBE_IMAGE=ghcr.io/akamai-consulting/ci-kubernetes:sha-"+testPinSHA+"\n")
		chdir(t, dir)
		stubImagePublished(t, true, true)

		var step string
		_, errOut := captureStdoutStderr(t, func() { step = reportCIImageSkew(testPinSHA) })
		if step != "" || errOut != "" {
			t.Errorf("an up-to-date instance produced step=%q stderr=%q, want both empty", step, errOut)
		}
	})
}
