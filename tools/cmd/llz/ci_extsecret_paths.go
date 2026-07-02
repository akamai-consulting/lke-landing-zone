package main

// ci_extsecret_paths.go implements `llz ci externalsecret-paths` — the native
// port of the former template-scripts/linting-and-validation/
// validate-externalsecret-paths.py (invoked by the Makefile's
// externalsecret-paths-check target, after render-charts).
//
// It cross-validates every ExternalSecret remoteRef.key and remoteRef.property
// in apl-values/ + the rendered chart output against the paths and field names
// seeded by the bootstrap workflows (llz-bootstrap-openbao.yml /
// llz-bootstrap-dns.yml), the `llz ci bao-seed-all` data-driven seed table
// (ci_bao_seed_all.go), and the native Go seeders (ci_harbor.go
// provision-harbor-robots, ci_seed_special.go), then verifies the bootstrap
// (`llz ci bao-configure`) platform-ci OpenBao policy covers those KV v2 paths.
// `llz ci bao-configure` is the SOLE owner of OpenBao auth/policy config (the
// former terraform-modules/llz-openbao vault-provider module was retired), so
// it is the only policy source cross-checked here. Every bootstrap-seeded KV
// path must have matching policy coverage even when it is consumed by CI rather
// than an ExternalSecret.
//
// Output (the `  ok:` / `::error file=…::` lines) and the exit semantics are
// kept identical to the Python validator: exit 0 when everything is seeded and
// policy-covered, non-zero otherwise.

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// esManualPaths are KV paths intentionally seeded outside of any bootstrap
// workflow's `bao kv put` steps. Add entries here only after confirming the
// manual step is documented.
var esManualPaths = map[string]bool{}

// Policy source parsed for literal `path "secret/data/…" { capabilities }`
// stanzas — the OpenBao read/seed policy HCL lives as Go string constants in
// `llz ci bao-configure`, the sole owner of OpenBao policy config.
const (
	esBaoConfigureLabel = "llz ci bao-configure (ci_openbao_configure.go)"
	esBaoConfigurePath  = "tools/cmd/llz/ci_openbao_configure.go"
)

// esRepoPath resolves a repo-relative path, tolerating the template layout
// where the instance content (bootstrap workflows, apl-values) lives under
// instance-template/ rather than at the repo root.
func esRepoPath(root, rel string) string {
	direct := filepath.Join(root, filepath.FromSlash(rel))
	if _, err := os.Stat(direct); err == nil {
		return direct
	}
	nested := filepath.Join(root, "instance-template", filepath.FromSlash(rel))
	if _, err := os.Stat(nested); err == nil {
		return nested
	}
	return direct
}

// esRef is one (remoteRef.key, remoteRef.property) pair; hasProp distinguishes
// "no property line" (whole-secret ref) from an empty property.
type esRef struct {
	key     string
	prop    string
	hasProp bool
}

var (
	esRemoteRefRx = regexp.MustCompile(`remoteRef:\s*\n\s+key:\s+(\S+)`)
	esPropertyRx  = regexp.MustCompile(`property:\s+(\S+)`)
)

// collectExternalSecretRefs returns {(remoteRef.key, property): [file, …]} from
// every *.yaml under apl-values/ and the rendered chart output, skipping
// vendored chart subtrees (/charts/).
func collectExternalSecretRefs(root, renderDir string) map[esRef][]string {
	refs := map[esRef][]string{}
	var sources []string
	for _, dir := range []string{filepath.Join(root, "apl-values"), filepath.Join(root, filepath.FromSlash(renderDir))} {
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d == nil || d.IsDir() {
				return nil
			}
			if strings.HasSuffix(p, ".yaml") {
				sources = append(sources, p)
			}
			return nil
		})
	}
	for _, f := range sources {
		if strings.Contains(filepath.ToSlash(f), "/charts/") {
			continue
		}
		b, _ := os.ReadFile(f)
		text := string(b)
		if !strings.Contains(text, "kind: ExternalSecret") {
			continue
		}
		for _, loc := range esRemoteRefRx.FindAllStringSubmatchIndex(text, -1) {
			key := text[loc[2]:loc[3]]
			lookahead := text[loc[1]:min(len(text), loc[1]+300)]
			if next := strings.Index(lookahead, "remoteRef:"); next >= 0 {
				lookahead = lookahead[:next]
			}
			ref := esRef{key: key}
			if m := esPropertyRx.FindStringSubmatch(lookahead); m != nil {
				ref.prop, ref.hasProp = m[1], true
			}
			relf, err := filepath.Rel(root, f)
			if err != nil {
				relf = f
			}
			refs[ref] = append(refs[ref], filepath.ToSlash(relf))
		}
	}
	return refs
}

var (
	esContinuationRx = regexp.MustCompile(`\\\n\s*`)
	esKVPutRx        = regexp.MustCompile(`kv put\s+secret/(\S+)(.*)`)
	esFieldRx        = regexp.MustCompile(`\b(\w+)=`)
	// The generic `llz ci bao-seed --path secret/<path> --field <name>=…` step
	// (ci_bao_seed.go) replaced the inline `bao kv put` blocks for most paths;
	// the seeded path is the --path flag and the written fields are the --field
	// names. Specialized seed-* commands write literal paths in Go and are
	// caught by collectSeededGo over ci_seed_special.go instead.
	esBaoSeedPathRx  = regexp.MustCompile(`--path\s+secret/(\S+)`)
	esBaoSeedFieldRx = regexp.MustCompile(`--field\s+(\w+)=`)
)

// collectSeeded returns (seeded paths, {kv path: field set}) from the seeding
// source files. Matches both `kv put secret/<path>` (any bao wrapper: bao, llz
// openbao exec, $BAO, …) and the `llz ci bao-seed --path secret/<path>` step;
// backslash line continuations are joined first so multi-line puts/seeds parse.
func collectSeeded(sources []string) (map[string]bool, map[string]map[string]bool, error) {
	paths := map[string]bool{}
	fields := map[string]map[string]bool{}
	addField := func(path, name string) {
		if fields[path] == nil {
			fields[path] = map[string]bool{}
		}
		fields[path][name] = true
	}
	for _, src := range sources {
		b, err := os.ReadFile(src)
		if err != nil {
			return nil, nil, fmt.Errorf("read seeding source: %w", err)
		}
		joined := esContinuationRx.ReplaceAllString(string(b), " ")
		for _, m := range esKVPutRx.FindAllStringSubmatch(joined, -1) {
			path, args := m[1], m[2]
			paths[path] = true
			for _, fm := range esFieldRx.FindAllStringSubmatch(args, -1) {
				addField(path, fm[1])
			}
		}
		// `bao-seed --path secret/<path> --field <name>=…` (one logical line
		// each once continuations are joined).
		for _, line := range strings.Split(joined, "\n") {
			if !strings.Contains(line, "bao-seed") {
				continue
			}
			pm := esBaoSeedPathRx.FindStringSubmatch(line)
			if pm == nil {
				continue
			}
			path := pm[1]
			paths[path] = true
			for _, fm := range esBaoSeedFieldRx.FindAllStringSubmatch(line, -1) {
				addField(path, fm[1])
			}
		}
	}
	return paths, fields, nil
}

var (
	esGoPutRx      = regexp.MustCompile(`(?s)baoKVPutFn\(\s*"secret/([^"]+)",\s*map\[string\]string\{(.*?)\}`)
	esGoFieldRx    = regexp.MustCompile(`"(\w+)":`)
	esGoSpecPathRx = regexp.MustCompile(`kvPath:\s*"secret/([^"]+)"`)
	// The rotation table's paths (ci_rotate_linode_creds.go) — seeded by
	// mint-bootstrap-objkeys at bootstrap and rewritten by the rotator
	// in-cluster. Their field sets live in the table's fields-builder map
	// literals in the same file (lokiObjectStoreFields, harborRegistryS3Fields,
	// the dns token literal), so the collector unions every map-literal key in
	// the file (+ rotated_at, stamped at write time) for these paths.
	esGoBaoPathRx = regexp.MustCompile(`baoPath:\s*"secret/([^"]+)"`)
	// Matches both the CI-side root-token put (baoKVPutFn) and the in-cluster
	// provisioner's k8s-auth write (bao.Write(ctx, spec.kvPath, …)) driving a
	// harborRobotSpec kvPath.
	esGoSpecPutRx = regexp.MustCompile(`(?s)(?:baoKVPutFn\(\s*\w+\.kvPath|\w+\.Write\(ctx,\s*\w+\.kvPath),\s*map\[string\]string\{(.*?)\}`)
	// The bootstrapSeeds() table (ci_bao_seed_all.go) declares each generic seed
	// as a baoSeedOpts literal: `path: "secret/<p>", … fieldSpecs: []string{…}`.
	// path: precedes fieldSpecs: in every entry (skipIfPresent: may sit between),
	// so a lazy match from one path: to its next fieldSpecs: stays inside the
	// entry. Each fieldSpecs string is "<name>=<source>".
	esSeedTableEntryRx = regexp.MustCompile(`(?s)path:\s*"secret/([^"]+)",.*?fieldSpecs:\s*\[\]string\{(.*?)\}`)
	esSeedTableFieldRx = regexp.MustCompile(`"(\w+)=`)
)

// collectSeededGo returns seeds written natively by llz Go code (ci_harbor.go)
// rather than via a shell `kv put` line: direct
// baoKVPutFn("secret/<path>", map[string]string{…}) calls, plus paths reaching
// that call indirectly as harborRobotSpec kvPath: literals (every spec is
// seeded at the single baoKVPutFn(spec.kvPath, …) call site, so those paths
// share its field set).
func collectSeededGo(src string) (map[string]bool, map[string]map[string]bool, error) {
	b, err := os.ReadFile(src)
	if os.IsNotExist(err) {
		// A scanned source may be absent (a thin instance checkout, or a renamed
		// file). Skip it rather than crash: any seed it held simply goes
		// undetected and surfaces as the validator's normal "not seeded" error.
		return map[string]bool{}, map[string]map[string]bool{}, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read Go seeding source: %w", err)
	}
	text := string(b)
	paths := map[string]bool{}
	fields := map[string]map[string]bool{}

	for _, m := range esGoPutRx.FindAllStringSubmatch(text, -1) {
		path, body := m[1], m[2]
		paths[path] = true
		if fields[path] == nil {
			fields[path] = map[string]bool{}
		}
		for _, fm := range esGoFieldRx.FindAllStringSubmatch(body, -1) {
			fields[path][fm[1]] = true
		}
	}

	collectSeededSeedTableInto(text, paths, fields)

	specFields := map[string]bool{}
	for _, m := range esGoSpecPutRx.FindAllStringSubmatch(text, -1) {
		for _, fm := range esGoFieldRx.FindAllStringSubmatch(m[1], -1) {
			specFields[fm[1]] = true
		}
	}
	for _, m := range esGoSpecPathRx.FindAllStringSubmatch(text, -1) {
		path := m[1]
		paths[path] = true
		if fields[path] == nil {
			fields[path] = map[string]bool{}
		}
		for f := range specFields {
			fields[path][f] = true
		}
	}

	// Rotation-table paths: the write site takes the fields-builder's return
	// value (not a map literal), so pair each baoPath with the union of every
	// map-literal key in the file (the builders live alongside the table) plus
	// rotated_at. Union over-claims per-path (safe: this validator guards
	// against MISSING seeds, not extra fields).
	if ms := esGoBaoPathRx.FindAllStringSubmatch(text, -1); len(ms) > 0 {
		tableFields := map[string]bool{"rotated_at": true}
		for _, fm := range esGoFieldRx.FindAllStringSubmatch(text, -1) {
			tableFields[fm[1]] = true
		}
		for _, m := range ms {
			path := m[1]
			paths[path] = true
			if fields[path] == nil {
				fields[path] = map[string]bool{}
			}
			for f := range tableFields {
				fields[path][f] = true
			}
		}
	}
	return paths, fields, nil
}

// collectSeededSeedTableInto folds the bootstrapSeeds() table (ci_bao_seed_all.go)
// into an existing paths/fields accumulator: each baoSeedOpts literal's
// path: "secret/<p>" plus the field names parsed from its fieldSpecs strings.
// This is the data-driven replacement for the former one-inline-bao-seed-step-
// per-secret blocks collectSeeded scraped from llz-bootstrap-openbao.yml. A
// no-op on files without the pattern (the other scanned Go sources), so it can
// run over every Go seeding source uniformly.
func collectSeededSeedTableInto(text string, paths map[string]bool, fields map[string]map[string]bool) {
	for _, m := range esSeedTableEntryRx.FindAllStringSubmatch(text, -1) {
		path, body := m[1], m[2]
		paths[path] = true
		if fields[path] == nil {
			fields[path] = map[string]bool{}
		}
		for _, fm := range esSeedTableFieldRx.FindAllStringSubmatch(body, -1) {
			fields[path][fm[1]] = true
		}
	}
}

var (
	esPolicyRx = regexp.MustCompile(`(?s)path\s+"secret/(data|metadata)/([^"]+)"\s*\{[^}]*capabilities\s*=\s*\[([^\]]*)\]`)
	esCapRx    = regexp.MustCompile(`"([^"]+)"`)
)

// esPolicy maps kv path → data|metadata → capability set for one policy source.
type esPolicy = map[string]map[string]map[string]bool

// collectPolicyPaths returns {kv path: {data|metadata: capability set}} for
// every literal secret KV v2 policy stanza in the file. When the same path+kind
// appears in more than one stanza (the file holds several policies — platform-ci,
// secret-propagator — and a path may be granted by both), capabilities are
// UNIONed, not overwritten: the file collectively grants the strongest set, so a
// path read+listed by platform-ci stays covered even if secret-propagator also
// grants it read-only.
func collectPolicyPaths(path string) (esPolicy, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read policy source: %w", err)
	}
	policies := esPolicy{}
	for _, m := range esPolicyRx.FindAllStringSubmatch(string(b), -1) {
		kind, kvPath, rawCaps := m[1], m[2], m[3]
		if policies[kvPath] == nil {
			policies[kvPath] = map[string]map[string]bool{}
		}
		if policies[kvPath][kind] == nil {
			policies[kvPath][kind] = map[string]bool{}
		}
		for _, cm := range esCapRx.FindAllStringSubmatch(rawCaps, -1) {
			policies[kvPath][kind][cm[1]] = true
		}
	}
	return policies, nil
}

// validatePolicyCoverage prints a ::error annotation per uncovered grant and
// returns the number of policy coverage errors for one KV path. Each policy
// source must independently cover the path: read on secret/data/<p>, read+list
// on secret/metadata/<p>.
func validatePolicyCoverage(key string, policyPaths map[string]esPolicy, files []string, w io.Writer) int {
	errors := 0
	labels := make([]string, 0, len(policyPaths))
	for l := range policyPaths {
		labels = append(labels, l)
	}
	sort.Strings(labels)
	for _, label := range labels {
		coverage := policyPaths[label][key]
		dataCaps := coverage["data"]
		metaCaps := coverage["metadata"]

		if !dataCaps["read"] {
			for _, f := range files {
				fmt.Fprintf(w, "::error file=%s::KV path '%s' is not covered by %s: expected path 'secret/data/%s' with read capability\n", f, key, label, key)
			}
			errors++
		}
		if !(metaCaps["read"] && metaCaps["list"]) {
			for _, f := range files {
				fmt.Fprintf(w, "::error file=%s::KV path '%s' is not covered by %s: expected path 'secret/metadata/%s' with read and list capabilities\n", f, key, label, key)
			}
			errors++
		}
	}
	return errors
}

// esPropFiles is one property variant of a referenced key with the files that
// reference it.
type esPropFiles struct {
	prop    string
	hasProp bool
	files   []string
}

func (p esPropFiles) sortKey() string {
	if !p.hasProp {
		return ""
	}
	return p.prop
}

func esUniqueSortedFiles(props []esPropFiles) []string {
	seen := map[string]bool{}
	for _, pf := range props {
		for _, f := range pf.files {
			seen[f] = true
		}
	}
	out := make([]string, 0, len(seen))
	for f := range seen {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

func runCIExternalSecretPaths(root string, w io.Writer) error {
	// The landing-zone template ships its ExternalSecrets AS charts, so the refs
	// to validate live in the rendered chart output (template-scripts/ci/
	// render-charts.sh → $RENDER_DIR), not only a raw apl-values/ tree. Both are
	// scanned so this works in the template repo and in a populated instance.
	renderDir := firstNonEmpty(os.Getenv("RENDER_DIR"), "rendered")
	refs := collectExternalSecretRefs(root, renderDir)

	// The `bao kv put secret/…` seeding lives in the reusable workflow BODIES
	// (the per-instance bootstrap-*.yml are thin callers with no seeds) and in
	// `llz ci provision-harbor-robots` (ci_harbor.go, parsed by the Go-aware
	// collector). See docs/templatization-plan.md §"Keeping instances in sync".
	seededPaths, seededFields, err := collectSeeded([]string{
		esRepoPath(root, ".github/workflows/llz-bootstrap-openbao.yml"),
		esRepoPath(root, ".github/workflows/llz-bootstrap-dns.yml"),
	})
	if err != nil {
		return err
	}
	// Native Go seeds: ci_harbor.go (standby robot seed, literal baoKVPutFn
	// calls) + ci_harbor_provisioner.go (in-cluster robot provisioner,
	// harborRobotSpec kvPath: entries); ci_bao_seed_all.go, whose
	// bootstrapSeeds() table declares the generic seeds; and
	// ci_rotate_linode_creds.go, whose rotation table (baoPath: entries) is
	// seeded by mint-bootstrap-objkeys at bootstrap and rewritten by the
	// in-cluster rotator (collectSeededGo runs every parser over every source —
	// no-ops where a pattern is absent).
	for _, goSrc := range []string{
		"tools/cmd/llz/ci_harbor.go",
		"tools/cmd/llz/ci_harbor_provisioner.go",
		"tools/cmd/llz/ci_seed_special.go",
		"tools/cmd/llz/ci_bao_seed_all.go",
		"tools/cmd/llz/ci_rotate_linode_creds.go",
	} {
		goPaths, goFields, err := collectSeededGo(esRepoPath(root, goSrc))
		if err != nil {
			return err
		}
		for p := range goPaths {
			seededPaths[p] = true
		}
		for p, fset := range goFields {
			if seededFields[p] == nil {
				seededFields[p] = map[string]bool{}
			}
			for f := range fset {
				seededFields[p][f] = true
			}
		}
	}

	policyPaths := map[string]esPolicy{}
	if policyPaths[esBaoConfigureLabel], err = collectPolicyPaths(esRepoPath(root, esBaoConfigurePath)); err != nil {
		return err
	}

	errors := 0
	keysToRefs := map[string][]esPropFiles{}
	for ref, files := range refs {
		keysToRefs[ref.key] = append(keysToRefs[ref.key], esPropFiles{ref.prop, ref.hasProp, files})
	}
	keys := make([]string, 0, len(keysToRefs))
	for k := range keysToRefs {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, key := range keys {
		if esManualPaths[key] {
			fmt.Fprintf(w, "  skip (manual): %s\n", key)
			continue
		}
		propFiles := keysToRefs[key]

		if !seededPaths[key] {
			for _, f := range esUniqueSortedFiles(propFiles) {
				fmt.Fprintf(w, "::error file=%s::ExternalSecret remoteRef.key '%s' is not seeded by any bootstrap workflow — add a 'bao kv put secret/%s' step to bootstrap-openbao.yml or bootstrap-dns.yml, or add to MANUAL_PATHS if intentionally manual\n", f, key, key)
			}
			errors++
			continue
		}

		sort.SliceStable(propFiles, func(i, j int) bool { return propFiles[i].sortKey() < propFiles[j].sortKey() })
		for _, pf := range propFiles {
			if pf.hasProp && !seededFields[key][pf.prop] {
				for _, f := range pf.files {
					fmt.Fprintf(w, "::error file=%s::ExternalSecret remoteRef.key '%s' property '%s' is not written by any 'bao kv put secret/%s' step in bootstrap-openbao.yml or bootstrap-dns.yml\n", f, key, pf.prop, key)
				}
				errors++
			} else {
				label := key
				if pf.hasProp {
					label = key + "." + pf.prop
				}
				fmt.Fprintf(w, "  ok: %s\n", label)
			}
		}

		errors += validatePolicyCoverage(key, policyPaths, esUniqueSortedFiles(propFiles), w)
	}

	// Every bootstrap-seeded KV path needs policy coverage even when it is
	// consumed by CI or automation rather than an ExternalSecret.
	var leftovers []string
	for p := range seededPaths {
		if _, referenced := keysToRefs[p]; !referenced && !esManualPaths[p] {
			leftovers = append(leftovers, p)
		}
	}
	sort.Strings(leftovers)
	for _, key := range leftovers {
		keyErrors := validatePolicyCoverage(key, policyPaths, []string{esBaoConfigurePath}, w)
		errors += keyErrors
		if keyErrors == 0 {
			fmt.Fprintf(w, "  ok (seeded policy): %s\n", key)
		}
	}

	if errors > 0 {
		fmt.Fprintf(w, "\n%d ExternalSecret ref(s) failed seed or policy validation.\n", errors)
		return fmt.Errorf("%d ExternalSecret ref(s) failed seed or policy validation", errors)
	}
	fmt.Fprintf(w, "\nAll ExternalSecret refs and bootstrap-seeded paths are policy-covered.\n")
	return nil
}

func ciExternalSecretPathsCmd() *cobra.Command {
	var root string
	c := &cobra.Command{
		Use:   "externalsecret-paths",
		Short: "cross-validate ExternalSecret remoteRefs against OpenBao seeding + policy coverage",
		Long: "Native port of the former template-scripts/linting-and-validation/\n" +
			"validate-externalsecret-paths.py (the Makefile's externalsecret-paths-check,\n" +
			"run after render-charts). Validates every ExternalSecret remoteRef.key/\n" +
			"property in apl-values/ + $RENDER_DIR against the bootstrap-workflow and\n" +
			"ci_harbor.go seeding, then asserts the bao-configure platform-ci policy\n" +
			"covers every consumed and seeded KV v2 path.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runCIExternalSecretPaths(root, os.Stdout)
		},
	}
	c.Flags().StringVar(&root, "root", ".", "repo root to validate")
	return c
}
