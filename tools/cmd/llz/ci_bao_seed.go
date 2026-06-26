package main

// ci_bao_seed.go implements `llz ci bao-seed` — the generic OpenBao KV seeder
// that replaced eight near-identical "Seed … in OpenBao" inline-bash steps in
// llz-bootstrap-openbao.yml (harbor admin, github-dispatch-token, cert-
// automation token, grafana admin, otel bearer, loki object-store). Every one
// of those steps was the same
// shape — resolve some secret material (an env secret, fresh random bytes, or
// another K8s Secret), ::add-mask:: it, `kv put` ONE path, and when an input
// is missing either skip with a step-summary note, warn, or defer failure via
// BOOTSTRAP_ERRORS=true + exit 0 (so the remaining seed steps still run and
// the job's final 'Fail on bootstrap errors' gate reports everything at once).
// The shape lives here once; the workflow carries only the per-step texts and
// field wiring as flags.
//
// --field <key>=<source> sources:
//   env:VARNAME       value of $VARNAME; empty triggers the on-missing path
//   gen:hex:N         hex of N crypto/rand bytes      (openssl rand -hex N)
//   gen:base64:N      base64 of N crypto/rand bytes with /+= stripped
//                     (openssl rand -base64 N | tr -d '/+=')
//   k8s:NS/SECRET/KEY one key of a K8s Secret, base64-decoded; absent/empty
//                     triggers the on-missing path
//   literal:VALUE     the value verbatim (NOT masked — literals are already
//                     visible in the workflow file, and masking a common word
//                     like "admin" would corrupt every log line containing it)

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// seedRandRead fills b from crypto/rand — a seam so the gen:* sources are
// deterministic under test.
var seedRandRead = func(b []byte) error {
	_, err := rand.Read(b)
	return err
}

// seedSource is one parsed --field value source.
type seedSource struct {
	kind string // "env" | "gen-hex" | "gen-base64" | "k8s" | "literal"
	ref  string // env var name / "ns/secret/key" / the literal value
	n    int    // random byte count for gen-*
}

// seedField pairs a KV field name with its source.
type seedField struct {
	key string
	src seedSource
}

// parseSeedField parses one --field spec, <key>=<source>.
func parseSeedField(spec string) (seedField, error) {
	key, src, ok := strings.Cut(spec, "=")
	if !ok || key == "" {
		return seedField{}, fmt.Errorf("--field must be <key>=<source>, got %q", spec)
	}
	kind, rest, _ := strings.Cut(src, ":")
	switch kind {
	case "env":
		if rest == "" {
			return seedField{}, fmt.Errorf("--field %s: env source needs a variable name (env:VARNAME)", key)
		}
		return seedField{key: key, src: seedSource{kind: "env", ref: rest}}, nil
	case "gen":
		enc, nStr, ok := strings.Cut(rest, ":")
		n, err := strconv.Atoi(nStr)
		if !ok || err != nil || n <= 0 || (enc != "hex" && enc != "base64") {
			return seedField{}, fmt.Errorf("--field %s: gen source must be gen:hex:N or gen:base64:N, got %q", key, src)
		}
		return seedField{key: key, src: seedSource{kind: "gen-" + enc, n: n}}, nil
	case "k8s":
		parts := strings.SplitN(rest, "/", 3)
		if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
			return seedField{}, fmt.Errorf("--field %s: k8s source must be k8s:NAMESPACE/SECRET/KEY, got %q", key, src)
		}
		return seedField{key: key, src: seedSource{kind: "k8s", ref: rest}}, nil
	case "literal":
		return seedField{key: key, src: seedSource{kind: "literal", ref: rest}}, nil
	default:
		return seedField{}, fmt.Errorf("--field %s: unknown source kind %q (env|gen|k8s|literal)", key, kind)
	}
}

// resolveSeedFields resolves every field value. Sources that can legitimately
// be absent (env:, k8s:) report into missing — named by their env var /
// NS/SECRET/KEY ref — instead of erroring; gen:/literal: always produce a
// value. The lookups are injected so the decision logic is pure under test.
func resolveSeedFields(fields []seedField, getenv func(string) string,
	k8sGet func(ns, name, key string) string, randRead func([]byte) error) (map[string]string, []string, error) {
	values := make(map[string]string, len(fields))
	var missing []string
	for _, f := range fields {
		switch f.src.kind {
		case "env":
			v := getenv(f.src.ref)
			if v == "" {
				missing = append(missing, f.src.ref)
				continue
			}
			values[f.key] = v
		case "k8s":
			parts := strings.SplitN(f.src.ref, "/", 3)
			v := k8sGet(parts[0], parts[1], parts[2])
			if v == "" {
				missing = append(missing, f.src.ref)
				continue
			}
			values[f.key] = v
		case "gen-hex":
			b := make([]byte, f.src.n)
			if err := randRead(b); err != nil {
				return nil, nil, fmt.Errorf("crypto/rand for field %s: %w", f.key, err)
			}
			values[f.key] = hex.EncodeToString(b)
		case "gen-base64":
			b := make([]byte, f.src.n)
			if err := randRead(b); err != nil {
				return nil, nil, fmt.Errorf("crypto/rand for field %s: %w", f.key, err)
			}
			// `openssl rand -base64 N | tr -d '/+='` — strip the
			// non-alphanumeric base64 chars and the padding.
			values[f.key] = strings.NewReplacer("/", "", "+", "", "=", "").
				Replace(base64.StdEncoding.EncodeToString(b))
		case "literal":
			values[f.key] = f.src.ref
		}
	}
	return values, missing, nil
}

// effectiveOnMissing returns the on-missing mode in effect: the standby
// override applies only when it is set AND this deployment's HA_ROLE is
// standby (e.g. the dispatch-token seed errors on active but skips on a
// standby, where harbor-ready is the active peer's concern).
func effectiveOnMissing(onMissing, standbyOverride, haRole string) string {
	if standbyOverride != "" && haRole == "standby" {
		return standbyOverride
	}
	return onMissing
}

// defaultMissingAnnotation is the single-line annotation used when the
// workflow supplies no --missing-annotation.
func defaultMissingAnnotation(path string, missing []string) string {
	return fmt.Sprintf("%s not set — %s not seeded", strings.Join(missing, " / "), path)
}

// k8sSecretField reads one key of a K8s Secret, "" on any failure (absent
// Secret/key, bad base64) — the bash `kubectl get secret … -o jsonpath …
// 2>/dev/null | base64 -d || true`.
func k8sSecretField(ns, name, key string) string {
	out, err := execOutput("kubectl", "-n", ns, "get", "secret", name,
		"-o", "jsonpath={.data."+strings.ReplaceAll(key, ".", `\.`)+"}")
	if err != nil {
		return ""
	}
	dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(out)))
	if err != nil {
		return ""
	}
	return string(dec)
}

// maskGHALines registers a possibly-multiline secret with ::add-mask:: —
// one directive per line, since Actions masking is line-oriented.
func maskGHALines(v string) {
	for _, line := range strings.Split(v, "\n") {
		if strings.TrimSpace(line) != "" {
			maskGHA(line)
		}
	}
}

type baoSeedOpts struct {
	path                string
	fieldSpecs          []string
	skipIfPresent       string
	onMissing           string
	onMissingStandby    string
	missingNotes        []string
	missingNotesStandby []string
	missingAnnotations  []string
	summaryOnSeed       []string
	seededMessage       string
}

func ciBaoSeedCmd() *cobra.Command {
	var o baoSeedOpts
	c := &cobra.Command{
		Use:   "bao-seed",
		Short: "seed one OpenBao KV path from env/random/K8s-Secret sources (generic seed step)",
		Long: "Generic native port of the \"Seed … in OpenBao\" inline steps of\n" +
			"llz-bootstrap-openbao.yml. Resolves each --field <key>=<source> (env:VAR,\n" +
			"gen:hex:N, gen:base64:N, k8s:NS/SECRET/KEY, literal:VALUE), ::add-mask::es\n" +
			"every resolved secret (literals excepted — they're already visible in the\n" +
			"workflow), and writes them in ONE `kv put` through the in-pod bao CLI.\n" +
			"--skip-if-present makes re-runs idempotent (don't rotate a live credential).\n" +
			"A missing env:/k8s: source follows --on-missing: skip (summary notes only),\n" +
			"warn (::warning:: + notes), or error (::error:: + BOOTSTRAP_ERRORS=true for\n" +
			"the job's final gate) — all exit 0 so the remaining seed steps still run.\n" +
			"--on-missing-standby/--missing-note-standby override the mode/notes when\n" +
			"HA_ROLE=standby. Reads OPENBAO_ROOT_TOKEN.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIBaoSeed(o) },
	}
	f := c.Flags()
	f.StringVar(&o.path, "path", "", "KV path to seed, e.g. secret/grafana/admin (required)")
	f.StringArrayVar(&o.fieldSpecs, "field", nil, "<key>=<source> field spec (repeatable, required)")
	f.StringVar(&o.skipIfPresent, "skip-if-present", "", "skip (exit 0) when this field of --path already has a value")
	f.StringVar(&o.onMissing, "on-missing", "error", "behavior when an env:/k8s: source is empty: skip|warn|error")
	f.StringVar(&o.onMissingStandby, "on-missing-standby", "", "on-missing override applied when HA_ROLE=standby")
	f.StringArrayVar(&o.missingNotes, "missing-note", nil, "$GITHUB_STEP_SUMMARY line emitted on missing sources (repeatable)")
	f.StringArrayVar(&o.missingNotesStandby, "missing-note-standby", nil, "summary lines replacing --missing-note when the standby override applies (repeatable)")
	f.StringArrayVar(&o.missingAnnotations, "missing-annotation", nil, "::warning::/::error:: line(s) on missing sources (repeatable; default derived from the missing names)")
	f.StringArrayVar(&o.summaryOnSeed, "summary-on-seed", nil, "$GITHUB_STEP_SUMMARY line appended only when a fresh seed happened (repeatable)")
	f.StringVar(&o.seededMessage, "seeded-message", "", "stdout line after a successful seed (default '<path> seeded.')")
	return c
}

func validOnMissing(mode string) bool {
	return mode == "skip" || mode == "warn" || mode == "error"
}

func runCIBaoSeed(o baoSeedOpts) error {
	if o.path == "" {
		return fmt.Errorf("--path is required")
	}
	if len(o.fieldSpecs) == 0 {
		return fmt.Errorf("at least one --field <key>=<source> is required")
	}
	if !validOnMissing(o.onMissing) {
		return fmt.Errorf("--on-missing must be skip|warn|error, got %q", o.onMissing)
	}
	if o.onMissingStandby != "" && !validOnMissing(o.onMissingStandby) {
		return fmt.Errorf("--on-missing-standby must be skip|warn|error, got %q", o.onMissingStandby)
	}
	fields := make([]seedField, 0, len(o.fieldSpecs))
	for _, spec := range o.fieldSpecs {
		f, err := parseSeedField(spec)
		if err != nil {
			return err
		}
		fields = append(fields, f)
	}

	// Idempotency guard: a value already at the path means an earlier run
	// seeded it; re-seeding would rotate a credential that is live in-cluster.
	if o.skipIfPresent != "" && baoKVGetField(o.path, o.skipIfPresent) != "" {
		fmt.Printf("%s already exists — skipping.\n", o.path)
		return nil
	}

	values, missing, err := resolveSeedFields(fields, os.Getenv, k8sSecretField, seedRandRead)
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		haRole := os.Getenv("HA_ROLE")
		mode := effectiveOnMissing(o.onMissing, o.onMissingStandby, haRole)
		notes := o.missingNotes
		if o.onMissingStandby != "" && haRole == "standby" && len(o.missingNotesStandby) > 0 {
			notes = o.missingNotesStandby
		}
		if err := appendGHAFile("GITHUB_STEP_SUMMARY", notes...); err != nil {
			return err
		}
		annotations := o.missingAnnotations
		if len(annotations) == 0 {
			annotations = []string{defaultMissingAnnotation(o.path, missing)}
		}
		switch mode {
		case "skip":
			// Summary notes only — an expected not-ready-yet state.
		case "warn":
			for _, a := range annotations {
				fmt.Fprintf(os.Stderr, "::warning::%s\n", a)
			}
		case "error":
			for _, a := range annotations {
				fmt.Fprintf(os.Stderr, "::error::%s\n", a)
			}
			if err := appendGHAFile("GITHUB_ENV", "BOOTSTRAP_ERRORS=true"); err != nil {
				return err
			}
		}
		return nil // exit 0: missing inputs defer/skip, never abort the job
	}

	// Mask everything secret before any output can echo it (literals are
	// workflow-visible non-secrets — see the file header).
	for _, f := range fields {
		if f.src.kind != "literal" {
			maskGHALines(values[f.key])
		}
	}

	if err := baoKVPutFn(o.path, values); err != nil {
		return err
	}
	msg := o.seededMessage
	if msg == "" {
		msg = o.path + " seeded."
	}
	fmt.Println(msg)
	return appendGHAFile("GITHUB_STEP_SUMMARY", o.summaryOnSeed...)
}
