package guardwalk

import (
	"path/filepath"
	"strings"
	"testing"
)

// PlatformTreeDirs arrived with the posture-credential-coverage extraction and had
// no test of its own — seven callers, all exercising it incidentally.
//
// What it encodes is a FACT ABOUT THIS REPO'S LAYOUT that moved once already: since
// the platform-apl move these roots live at the repo ROOT, outside the instance
// scaffold. A guard given the wrong roots does not fail loudly — it scans nothing
// and reports the same green as a full pass, which is the empty-corpus bug this
// package exists to prevent.
func TestPlatformTreeDirs(t *testing.T) {
	got := PlatformTreeDirs(reader("/repo"))
	if len(got) != 3 {
		t.Fatalf("want all three shared manifest roots, got %v", got)
	}
	for _, want := range []string{
		filepath.Join("platform-apl", "manifest"),
		// manifest-secret-store is the one that was MISSING. It is a real deployed
		// unit with its own llz-secret-store Application, and it holds the two
		// ClusterSecretStores every ExternalSecret in the repo binds to — so while
		// it was absent the four guards sharing these roots never opened it. This
		// extraction also found the function's own header still saying "two".
		filepath.Join("platform-apl", "manifest-secret-store"),
		filepath.Join("platform-apl", "components"),
	} {
		var found bool
		for _, g := range got {
			if g == want {
				found = true
			}
		}
		if !found {
			t.Errorf("missing root %q — a guard handed the wrong roots scans nothing and "+
				"reports the same green as a full pass; got %v", want, got)
		}
	}
	// The roots must sit at the repo root, NOT under the instance scaffold — the
	// distinction the platform-apl move created.
	for _, g := range got {
		if strings.Contains(g, "instance-template") {
			t.Errorf("%q points inside the instance scaffold; these roots moved to the repo root", g)
		}
	}
}

// The roots are REPO-RELATIVE whatever the reader is fenced at — that is the
// point of routing them through the reader. An absolute path here would be one
// the fence refuses, so this pins that the two agree.
func TestPlatformTreeDirsRelativeRoot(t *testing.T) {
	for _, g := range PlatformTreeDirs(reader("")) {
		if filepath.IsAbs(g) {
			t.Errorf("PlatformTreeDirs(\"\") returned an absolute path %q — run from the repo "+
				"root the paths must stay relative", g)
		}
	}
}
