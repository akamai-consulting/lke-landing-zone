package clusterspec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every component whose manifests reference the llz image MUST be registered for
// retagging, or it ships a mutable `:latest` that resolves at ADMISSION rather than
// to the commit under test.
//
// objProxy was not registered. On an e2e cluster where `:latest` had been rebuilt
// several times that day, nodes pinned different digests and one landed on an image
// predating the obj-proxy subcommand — `unknown flag: --listen`, CrashLoopBackOff on
// that node while its four siblings ran fine. A component running mixed code across
// a fleet is not something any static check in the repo would otherwise notice.
//
// clusterHealthWorkflow is the documented exception: its image sits in an Argo
// WorkflowTemplate the images: transformer cannot reach, so RenderManifestKustomization
// retags it with a JSON6902 patch instead.
//
// "RUNS the image" is not "mentions the image", and the first revision of this test
// conflated them — it flagged imageSignature, whose Kyverno policy names
// `ghcr.io/…/llz:*` as a VERIFICATION PATTERN and runs nothing. So the match is on a
// container `image:` key specifically, not on the string appearing anywhere.
func TestComponentsUsingTheLLZImageAreRetagged(t *testing.T) {
	root := "../../../../platform-apl/components"
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("could not read the component tree (%v) — a skip here would reproduce the gap this closes", err)
	}
	exempt := map[string]bool{"clusterHealthWorkflow": true}

	checked := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		comp := e.Name()
		uses := false
		_ = filepath.Walk(filepath.Join(root, comp), func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || filepath.Ext(p) != ".yaml" {
				return nil
			}
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				return nil
			}
			for _, ln := range strings.Split(string(b), "\n") {
				t := strings.TrimSpace(ln)
				t = strings.TrimPrefix(t, "- ")
				if strings.HasPrefix(t, "image:") && strings.Contains(t, llzImageName+":") {
					uses = true
				}
			}
			return nil
		})
		if !uses {
			continue
		}
		checked++
		if exempt[comp] {
			continue
		}
		if !carvedLLZImageComponents[comp] {
			t.Errorf("component %q ships %s but is NOT in carvedLLZImageComponents — its image is a "+
				"MUTABLE tag resolved at admission, so nodes can run different builds and one can be "+
				"arbitrarily stale", comp, llzImageName)
		}
	}
	if checked == 0 {
		t.Fatal("found no component referencing the llz image — the scan is broken, not the tree")
	}
}
