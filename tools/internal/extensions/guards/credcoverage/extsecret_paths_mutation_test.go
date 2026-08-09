package credcoverage

// Mutation-coverage tests for ci_extsecret_paths.go.
//
// Every collector here walks source text with FindAllStringSubmatch(x, -1) —
// "give me EVERY match". The existing fixtures happened to hold exactly ONE
// match per input, so capping the scan at the first match (-1 → 1) changed
// nothing anyone asserted: a real seeding source that writes two OpenBao paths
// (or one path with two fields) would have been read as writing only the first,
// and the guard would still report color.Green while the second went unvalidated.
//
// Each test below therefore feeds a fixture with TWO OR MORE matches per input
// and asserts that ALL of them are extracted.

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

// esSortedSet renders a set as a stable slice for comparison/diagnostics.
func esSortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// A single `llz ci bao-seed` step commonly writes SEVERAL --field flags; all of
// them must be recorded against the path, not just the first.
func TestCollectSeededBaoSeedMultipleFields(t *testing.T) {
	root := t.TempDir()
	fixWrite(t, root, "bootstrap.yml", strings.Join([]string{
		"      - run: |",
		`          llz ci bao-seed --path secret/multi/fields \`,
		`            --field token=env:TOK \`,
		`            --field ca_crt=env:CA \`,
		`            --field ca_key=env:KEY`,
	}, "\n"))

	paths, fields, err := collectSeeded(ccRepo(root), []string{"bootstrap.yml"})
	if err != nil {
		t.Fatal(err)
	}
	if !paths["multi/fields"] {
		t.Fatalf("paths = %v", esSortedSet(paths))
	}
	want := []string{"ca_crt", "ca_key", "token"}
	if got := esSortedSet(fields["multi/fields"]); !reflect.DeepEqual(got, want) {
		t.Errorf("multi/fields fields = %v, want %v (all --field flags on the step, not just the first)", got, want)
	}
}

// One Go seeding source normally holds SEVERAL literal
// baoKVPutFn("secret/<path>", map[string]string{…}) calls, each writing SEVERAL
// fields. Both the per-file call scan and the per-call field scan must be
// exhaustive.
func TestCollectSeededGoMultiplePutsAndFields(t *testing.T) {
	root := t.TempDir()
	fixWrite(t, root, "ci_seed_special.go", `package main

func seedTwo() {
	_ = baoKVPutFn("secret/alpha/one", map[string]string{
		"alpha_first":  a,
		"alpha_second": b,
	})
	_ = baoKVPutFn("secret/beta/two", map[string]string{
		"beta_first":  c,
		"beta_second": d,
	})
}
`)
	paths, fields, err := collectSeededGo(ccRepo(root), "ci_seed_special.go")
	if err != nil {
		t.Fatal(err)
	}
	wantPaths := []string{"alpha/one", "beta/two"}
	if got := esSortedSet(paths); !reflect.DeepEqual(got, wantPaths) {
		t.Errorf("paths = %v, want %v (every baoKVPutFn call, not just the first)", got, wantPaths)
	}
	for path, want := range map[string][]string{
		"alpha/one": {"alpha_first", "alpha_second"},
		"beta/two":  {"beta_first", "beta_second"},
	} {
		if got := esSortedSet(fields[path]); !reflect.DeepEqual(got, want) {
			t.Errorf("%s fields = %v, want %v (every map-literal key, not just the first)", path, got, want)
		}
	}
}

// harborRobotSpec kvPath: literals are seeded at MORE THAN ONE call site — the
// CI-side baoKVPutFn(spec.kvPath, …) and the in-cluster
// bao.Write(ctx, spec.kvPath, …). The union of both sites' fields is what a
// spec-seeded path is credited with; stopping at the first site would silently
// drop the in-cluster provisioner's fields.
func TestCollectSeededGoMultipleSpecPutSites(t *testing.T) {
	root := t.TempDir()
	fixWrite(t, root, "ci_harbor_provisioner.go", `package main

func provision() {
	specs := []harborRobotSpec{
		{kvPath: "secret/robots/first"},
	}
	_ = baoKVPutFn(spec.kvPath, map[string]string{
		"ci_name": n,
	})
	_ = bao.Write(ctx, spec.kvPath, map[string]string{
		"incluster_token": tk,
	})
}
`)
	paths, fields, err := collectSeededGo(ccRepo(root), "ci_harbor_provisioner.go")
	if err != nil {
		t.Fatal(err)
	}
	if !paths["robots/first"] {
		t.Fatalf("paths = %v", esSortedSet(paths))
	}
	want := []string{"ci_name", "incluster_token"}
	if got := esSortedSet(fields["robots/first"]); !reflect.DeepEqual(got, want) {
		t.Errorf("robots/first fields = %v, want %v (union of EVERY spec.kvPath write site)", got, want)
	}
}

// The rotation table (ci_rotate_linode_creds.go) declares SEVERAL baoPath:
// entries; each one is a seeded path. Reading only the first left the rest
// looking unseeded — the exact failure this validator exists to report.
func TestCollectSeededGoMultipleRotationBaoPaths(t *testing.T) {
	root := t.TempDir()
	fixWrite(t, root, "ci_rotate_linode_creds.go", `package main

var rotations = []rotationSpec{
	{baoPath: "secret/loki/object-store", fields: lokiObjectStoreFields},
	{baoPath: "secret/harbor/registry-s3", fields: harborRegistryS3Fields},
}

func lokiObjectStoreFields() map[string]string {
	return map[string]string{
		"access_key_id":     id,
		"secret_access_key": sk,
	}
}
`)
	paths, fields, err := collectSeededGo(ccRepo(root), "ci_rotate_linode_creds.go")
	if err != nil {
		t.Fatal(err)
	}
	wantPaths := []string{"harbor/registry-s3", "loki/object-store"}
	if got := esSortedSet(paths); !reflect.DeepEqual(got, wantPaths) {
		t.Errorf("paths = %v, want %v (every baoPath: entry, not just the first)", got, wantPaths)
	}
	// Union of every map-literal key in the file plus the write-time stamp.
	want := []string{"access_key_id", "rotated_at", "secret_access_key"}
	for _, p := range wantPaths {
		if got := esSortedSet(fields[p]); !reflect.DeepEqual(got, want) {
			t.Errorf("%s fields = %v, want %v", p, got, want)
		}
	}

	// No baoPath: at all → the rotation branch contributes nothing (no phantom
	// path, no rotated_at grafted onto unrelated seeds).
	fixWrite(t, root, "no_rotation.go", `package main

func seed() {
	_ = baoKVPutFn("secret/plain/one", map[string]string{"only": v})
}
`)
	paths, fields, err = collectSeededGo(ccRepo(root), "no_rotation.go")
	if err != nil {
		t.Fatal(err)
	}
	if got := esSortedSet(paths); !reflect.DeepEqual(got, []string{"plain/one"}) {
		t.Errorf("paths = %v, want [plain/one]", got)
	}
	if got := esSortedSet(fields["plain/one"]); !reflect.DeepEqual(got, []string{"only"}) {
		t.Errorf("plain/one fields = %v, want [only]", got)
	}
}

// bootstrapSeeds() (ci_bao_seed_all.go) is a TABLE — several baoSeedOpts
// literals, each with its own fieldSpecs. Every entry must fold in.
func TestCollectSeededSeedTableIntoMultipleEntries(t *testing.T) {
	text := `package main

func bootstrapSeeds() []baoSeedOpts {
	return []baoSeedOpts{
		{
			path:       "secret/cert-automation/github-token",
			fieldSpecs: []string{"token=env:A", "ca_crt=env:B"},
		},
		{
			path:          "secret/infra/github-dispatch-token",
			skipIfPresent: true,
			fieldSpecs:    []string{"dispatch_token=env:C"},
		},
	}
}
`
	paths := map[string]bool{}
	fields := map[string]map[string]bool{}
	collectSeededSeedTableInto(text, paths, fields)

	wantPaths := []string{"cert-automation/github-token", "infra/github-dispatch-token"}
	if got := esSortedSet(paths); !reflect.DeepEqual(got, wantPaths) {
		t.Errorf("paths = %v, want %v (every table entry, not just the first)", got, wantPaths)
	}
	for path, want := range map[string][]string{
		"cert-automation/github-token": {"ca_crt", "token"},
		"infra/github-dispatch-token":  {"dispatch_token"},
	} {
		if got := esSortedSet(fields[path]); !reflect.DeepEqual(got, want) {
			t.Errorf("%s fields = %v, want %v", path, got, want)
		}
	}
}
