package teardown

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/linode"
	tf "github.com/akamai-consulting/lke-landing-zone/tools/internal/terraform"
)

// fakeKubectl scripts kubectl responses keyed by a substring of the joined argv,
// and records the calls made. A local copy of package main's helper: the unwedge
// tests moved with the code, the kyverno tests that also use it did not, and a
// test fixture cannot cross a package boundary.
type fakeKubectl struct {
	responses []kubectlRule
	calls     []string
}

type kubectlRule struct {
	match string // substring that must appear in the joined args
	out   string
	ok    bool
}

func (f *fakeKubectl) run(args ...string) (string, bool) {
	joined := strings.Join(args, " ")
	f.calls = append(f.calls, joined)
	for _, r := range f.responses {
		if strings.Contains(joined, r.match) {
			return r.out, r.ok
		}
	}
	return "", true // default: success, no output
}

func (f *fakeKubectl) called(substr string) bool {
	for _, c := range f.calls {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

// testDeps is the Deps a unit test hands the destroy verbs: no cloud, no cluster,
// and Confirm answering true so the delete paths are actually exercised rather
// than short-circuiting into their dry-run branch.
//
// Confirm defaulting to TRUE here is deliberate and slightly uncomfortable. In
// production the same bit defaults to false and an operator has to pass `--yes`.
// A test that forgot to set it would silently exercise only the dry-run half —
// which is exactly the vacuous-pass shape this repo keeps finding — so the fixture
// makes the destructive path the default and the dry-run cases set it explicitly.
func testDeps(t *testing.T) Deps {
	t.Helper()
	return Deps{
		Token: func() (string, error) { return "tok", nil },
		Exec:  func(string, ...string) ([]byte, error) { return nil, nil },
		TFBin: func() string { return "tofu" },
		// Summary does the REAL thing — appends to the file named by the env var —
		// because several cases assert on GITHUB_ENV content. A no-op stub made
		// them pass vacuously, which is the exact failure this repo keeps finding:
		// the assertion still ran, against nothing.
		Summary: func(envVar string, lines ...string) error {
			path := os.Getenv(envVar)
			if path == "" {
				return nil
			}
			f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				return err
			}
			defer f.Close()
			for _, l := range lines {
				if _, err := f.WriteString(l + "\n"); err != nil {
					return err
				}
			}
			return nil
		},
		Confirm: func() bool { return true },
		TempKubeconfig: func(pattern string, raw []byte) (string, func(), error) {
			f, err := os.CreateTemp(t.TempDir(), pattern)
			if err != nil {
				return "", nil, err
			}
			defer f.Close()
			if _, err := f.Write(raw); err != nil {
				return "", nil, err
			}
			return f.Name(), func() {}, nil
		},
		Combined: func(string, ...string) (string, bool) { return "", true },
		Client:   func() (*linode.Client, context.Context, error) { return nil, context.Background(), nil },
		RegionTFVars: func(tfDir, region string) (tf.TFVars, string, error) {
			b, err := os.ReadFile(tfDir + "/" + region + ".tfvars")
			if err != nil {
				return tf.TFVars{}, "", err
			}
			return tf.ParseTFVars(string(b)), "", nil
		},
	}
}

// A NOTE ON WHAT Deps COSTS, since this fixture is where it bites. The struct
// trades a compile error for a nil-pointer panic: `Deps{}` builds fine and then
// dereferences a nil func the moment a path needs a seam the caller forgot. The
// package-level vars this replaced could not fail that way, because every one had
// a real default.
//
// It is still the right trade — a seam that must be supplied is a seam a test can
// vary, and two tests can hold different stubs at once — but a future action ABI
// handing capabilities to extensions should hand ZERO VALUES THAT WORK rather than
// nils, or validate the set up front. Deps.Confirm defaulting to nil would mean a
// destroy verb panicking instead of refusing, which is the wrong direction.

// fakeOrphanScanner implements OrphanScanner from canned data. See the note on the
// duplicate in package main: the interface is the contract that keeps the two in
// step.
type fakeOrphanScanner struct {
	live     map[string]bool
	volumes  []map[string]any
	nbs      []map[string]any
	vpcs     []map[string]any
	backends map[uint64]int
}

func (f *fakeOrphanScanner) LiveClusterIDs(context.Context) (map[string]bool, error) {
	return f.live, nil
}
func (f *fakeOrphanScanner) ListVolumes(context.Context) ([]map[string]any, error) {
	return f.volumes, nil
}
func (f *fakeOrphanScanner) ListNodeBalancers(context.Context) ([]map[string]any, error) {
	return f.nbs, nil
}
func (f *fakeOrphanScanner) NodeBalancerBackendCount(_ context.Context, id uint64) (int, error) {
	return f.backends[id], nil
}
func (f *fakeOrphanScanner) ListVPCs(context.Context) ([]map[string]any, error) {
	return f.vpcs, nil
}

// captureStdout is a local copy of package main's helper. The destroy verbs print
// what they deleted, and that output is the operator's record — several cases here
// assert on it.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = orig
	b, _ := io.ReadAll(r)
	return string(b)
}
