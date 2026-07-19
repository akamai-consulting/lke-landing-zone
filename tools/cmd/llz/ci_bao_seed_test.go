package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseSeedField(t *testing.T) {
	cases := []struct {
		spec    string
		want    seedField
		wantErr bool
	}{
		{spec: "token=env:OPENBAO_SECRETS_WRITE_TOKEN",
			want: seedField{key: "token", src: seedSource{kind: "env", ref: "OPENBAO_SECRETS_WRITE_TOKEN"}}},
		{spec: "token=gen:hex:32",
			want: seedField{key: "token", src: seedSource{kind: "gen-hex", n: 32}}},
		{spec: "password=gen:base64:24",
			want: seedField{key: "password", src: seedSource{kind: "gen-base64", n: 24}}},
		{spec: "password=k8s:harbor/harbor-admin-password/HARBOR_ADMIN_PASSWORD",
			want: seedField{key: "password", src: seedSource{kind: "k8s", ref: "harbor/harbor-admin-password/HARBOR_ADMIN_PASSWORD"}}},
		{spec: "username=literal:admin",
			want: seedField{key: "username", src: seedSource{kind: "literal", ref: "admin"}}},
		// literal values keep their colons/equals after the first '='.
		{spec: "url=literal:https://x:8200?a=b",
			want: seedField{key: "url", src: seedSource{kind: "literal", ref: "https://x:8200?a=b"}}},
		{spec: "no-equals", wantErr: true},
		{spec: "=env:X", wantErr: true},
		{spec: "k=env:", wantErr: true},
		{spec: "k=file:/etc/passwd", wantErr: true},
		{spec: "k=gen:hex:", wantErr: true},
		{spec: "k=gen:hex:0", wantErr: true},
		{spec: "k=gen:dice:6", wantErr: true},
		{spec: "k=k8s:ns/secret", wantErr: true},
		{spec: "k=k8s:ns//key", wantErr: true},
	}
	for _, tc := range cases {
		got, err := parseSeedField(tc.spec)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseSeedField(%q) = %+v, want error", tc.spec, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSeedField(%q): %v", tc.spec, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseSeedField(%q) = %+v, want %+v", tc.spec, got, tc.want)
		}
	}
}

func TestResolveSeedFields(t *testing.T) {
	getenv := func(k string) string {
		return map[string]string{"SET_VAR": "from-env"}[k]
	}
	k8sGet := func(ns, name, key string) string {
		if ns == "harbor" && name == "harbor-admin-password" && key == "HARBOR_ADMIN_PASSWORD" {
			return "hunter2"
		}
		return ""
	}
	// Deterministic "random": 0xfb 0xff 0xbf … — StdEncoding emits +, / and =
	// for these, so the gen:base64 stripping is exercised.
	randRead := func(b []byte) error {
		pattern := []byte{0xfb, 0xff, 0xbf}
		for i := range b {
			b[i] = pattern[i%len(pattern)]
		}
		return nil
	}
	mustParse := func(specs ...string) []seedField {
		t.Helper()
		fields := make([]seedField, 0, len(specs))
		for _, s := range specs {
			f, err := parseSeedField(s)
			if err != nil {
				t.Fatal(err)
			}
			fields = append(fields, f)
		}
		return fields
	}

	// All sources resolve.
	values, missing, err := resolveSeedFields(
		mustParse("a=env:SET_VAR", "b=k8s:harbor/harbor-admin-password/HARBOR_ADMIN_PASSWORD",
			"c=literal:plain", "d=gen:hex:4", "e=gen:base64:4"),
		getenv, k8sGet, randRead)
	if err != nil || len(missing) != 0 {
		t.Fatalf("resolve: err=%v missing=%v", err, missing)
	}
	if values["a"] != "from-env" || values["b"] != "hunter2" || values["c"] != "plain" {
		t.Errorf("env/k8s/literal values wrong: %+v", values)
	}
	if values["d"] != "fbffbffb" {
		t.Errorf("gen:hex:4 = %q, want fbffbffb", values["d"])
	}
	// base64(0xfb 0xff 0xbf 0xfb) = "+/+/+w==" → stripped to "w".
	if values["e"] != "w" || strings.ContainsAny(values["e"], "/+=") {
		t.Errorf("gen:base64:4 = %q, want %q with /+= stripped", values["e"], "w")
	}

	// Missing env + k8s sources are reported by name, in field order.
	values, missing, err = resolveSeedFields(
		mustParse("a=env:UNSET_VAR", "b=k8s:ns/absent/key", "c=env:SET_VAR"),
		getenv, k8sGet, randRead)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"UNSET_VAR", "ns/absent/key"}; !reflect.DeepEqual(missing, want) {
		t.Errorf("missing = %v, want %v", missing, want)
	}
	if values["c"] != "from-env" {
		t.Errorf("resolvable fields must still resolve alongside missing ones: %+v", values)
	}

	// A rand failure is a hard error, never a silent weak secret.
	if _, _, err := resolveSeedFields(mustParse("d=gen:hex:4"), getenv, k8sGet,
		func([]byte) error { return errors.New("entropy gone") }); err == nil {
		t.Error("rand failure must error")
	}
}

func TestEffectiveOnMissing(t *testing.T) {
	cases := []struct {
		onMissing, standby, haRole, want string
	}{
		{"error", "", "active", "error"},
		{"error", "", "standby", "error"},      // no override configured
		{"error", "skip", "standby", "skip"},   // the dispatch-token case
		{"error", "skip", "active", "error"},   // override only fires on standby
		{"error", "skip", "", "error"},         // unset role ≠ standby
		{"warn", "error", "standby", "error"},  // override can also escalate
		{"skip", "skip", "standalone", "skip"}, // standalone uses the base mode
	}
	for _, tc := range cases {
		if got := effectiveOnMissing(tc.onMissing, tc.standby, tc.haRole); got != tc.want {
			t.Errorf("effectiveOnMissing(%q,%q,%q) = %q, want %q",
				tc.onMissing, tc.standby, tc.haRole, got, tc.want)
		}
	}
}

func TestDefaultMissingAnnotation(t *testing.T) {
	got := defaultMissingAnnotation("secret/loki/object-store", []string{"LOKI_S3_ACCESS_KEY", "LOKI_S3_SECRET_KEY"})
	want := "LOKI_S3_ACCESS_KEY / LOKI_S3_SECRET_KEY not set — secret/loki/object-store not seeded"
	if got != want {
		t.Errorf("defaultMissingAnnotation = %q, want %q", got, want)
	}
}

func TestK8sSecretField(t *testing.T) {
	withKubectl(t, func(a string) ([]byte, error) {
		switch {
		case strings.Contains(a, "get secret good"):
			return []byte("aHVudGVyMg=="), nil // "hunter2"
		case strings.Contains(a, "get secret badb64"):
			return []byte("!!not-base64!!"), nil
		// Dotted keys must be jsonpath-escaped or kubectl resolves
		// .data.tls.crt as a nested path.
		case strings.Contains(a, `jsonpath={.data.tls\.crt}`):
			return []byte("Y2VydA=="), nil // "cert"
		default:
			return nil, errors.New("NotFound")
		}
	})
	if got := k8sSecretField("ns", "good", "k"); got != "hunter2" {
		t.Errorf("k8sSecretField good = %q", got)
	}
	if got := k8sSecretField("ns", "dotted", "tls.crt"); got != "cert" {
		t.Errorf("k8sSecretField dotted key = %q", got)
	}
	if got := k8sSecretField("ns", "absent", "k"); got != "" {
		t.Errorf("absent Secret must read as empty, got %q", got)
	}
	if got := k8sSecretField("ns", "badb64", "k"); got != "" {
		t.Errorf("bad base64 must read as empty, got %q", got)
	}
}

// stubBaoSeedKV stubs baoExecFn for bao-seed runs: `kv get` of presentPath/
// presentField returns presentValue; every `kv put` is recorded.
func lastArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[len(args)-1]
}

func stubBaoSeedKV(t *testing.T, presentField, presentValue string) *[][]string {
	t.Helper()
	var puts [][]string
	prev := baoExecFn
	baoExecFn = func(_, _, _ string, args ...string) (string, string, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(joined, "kv get"):
			if presentField != "" && strings.Contains(joined, "-field="+presentField) {
				return presentValue + "\n", "", nil
			}
			// bao's own words for an absent path. A bare error with no stderr
			// would now mean "the read never got an answer" — which fails the
			// seed closed instead of overwriting a possibly-live credential.
			return "", "No value found at " + lastArg(args), errors.New("exit 2")
		case strings.HasPrefix(joined, "kv put"):
			puts = append(puts, args)
			return "", "", nil
		}
		return "", "unexpected: " + joined, errors.New("unexpected")
	}
	t.Cleanup(func() { baoExecFn = prev })
	return &puts
}

func withGHASummaryFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "summary")
	t.Setenv("GITHUB_STEP_SUMMARY", p)
	return p
}

func TestRunCIBaoSeedSkipIfPresent(t *testing.T) {
	puts := stubBaoSeedKV(t, "password", "already-there")
	t.Setenv("OPENBAO_ROOT_TOKEN", "root")
	err := runCIBaoSeed(baoSeedOpts{
		path:          "secret/grafana/admin",
		fieldSpecs:    []string{"password=gen:base64:24"},
		skipIfPresent: "password",
		onMissing:     "error",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(*puts) != 0 {
		t.Errorf("skip-if-present must not kv put, got %v", *puts)
	}
}

func TestRunCIBaoSeedMissingModes(t *testing.T) {
	for _, tc := range []struct {
		name, onMissing, onMissingStandby, haRole string
		wantBootstrapErrors                       bool
		wantNote                                  string
	}{
		{name: "error", onMissing: "error", wantBootstrapErrors: true, wantNote: "base note"},
		{name: "warn", onMissing: "warn", wantNote: "base note"},
		{name: "skip", onMissing: "skip", wantNote: "base note"},
		{name: "standby override skips", onMissing: "error", onMissingStandby: "skip",
			haRole: "standby", wantNote: "standby note"},
		{name: "active ignores override", onMissing: "error", onMissingStandby: "skip",
			haRole: "active", wantBootstrapErrors: true, wantNote: "base note"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			puts := stubBaoSeedKV(t, "", "")
			t.Setenv("OPENBAO_ROOT_TOKEN", "root")
			t.Setenv("HA_ROLE", tc.haRole)
			t.Setenv("UNSET_SEED_VAR", "")
			envFile := withGHAEnvFile(t)
			sum := withGHASummaryFile(t)
			err := runCIBaoSeed(baoSeedOpts{
				path:                "secret/x/y",
				fieldSpecs:          []string{"token=env:UNSET_SEED_VAR"},
				onMissing:           tc.onMissing,
				onMissingStandby:    tc.onMissingStandby,
				missingNotes:        []string{"base note"},
				missingNotesStandby: []string{"standby note"},
				missingAnnotations:  []string{"the annotation"},
			})
			if err != nil {
				t.Fatalf("missing inputs must exit 0: %v", err)
			}
			if len(*puts) != 0 {
				t.Errorf("missing inputs must not kv put, got %v", *puts)
			}
			if got := ghaEnvContains(t, envFile, "BOOTSTRAP_ERRORS=true"); got != tc.wantBootstrapErrors {
				t.Errorf("BOOTSTRAP_ERRORS=true present=%v, want %v", got, tc.wantBootstrapErrors)
			}
			b, _ := os.ReadFile(sum)
			if !strings.Contains(string(b), tc.wantNote) {
				t.Errorf("summary %q missing note %q", b, tc.wantNote)
			}
		})
	}
}

func TestRunCIBaoSeedSeeds(t *testing.T) {
	puts := stubBaoSeedKV(t, "", "")
	t.Setenv("OPENBAO_ROOT_TOKEN", "root")
	t.Setenv("SEED_A", "alpha")
	withKubectl(t, func(a string) ([]byte, error) {
		if strings.Contains(a, "get secret s") {
			return []byte("YmV0YQ=="), nil // "beta"
		}
		return nil, errors.New("NotFound")
	})
	envFile := withGHAEnvFile(t)
	sum := withGHASummaryFile(t)
	err := runCIBaoSeed(baoSeedOpts{
		path:          "secret/x/y",
		fieldSpecs:    []string{"a=env:SEED_A", "b=k8s:ns/s/k", "c=literal:lit"},
		onMissing:     "error",
		summaryOnSeed: []string{"seed summary line"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(*puts) != 1 {
		t.Fatalf("want exactly one kv put, got %d", len(*puts))
	}
	got := strings.Join((*puts)[0], " ")
	// baoKVPutFn sorts fields for a deterministic argv.
	if want := "kv put secret/x/y a=alpha b=beta c=lit"; got != want {
		t.Errorf("kv put argv = %q, want %q", got, want)
	}
	if ghaEnvContains(t, envFile, "BOOTSTRAP_ERRORS") {
		t.Error("a successful seed must not flag BOOTSTRAP_ERRORS")
	}
	b, _ := os.ReadFile(sum)
	if !strings.Contains(string(b), "seed summary line") {
		t.Errorf("summary-on-seed line missing from %q", b)
	}
}

func TestRunCIBaoSeedValidation(t *testing.T) {
	for _, o := range []baoSeedOpts{
		{fieldSpecs: []string{"a=env:X"}, onMissing: "error"}, // no path
		{path: "secret/x", onMissing: "error"},                // no fields
		{path: "secret/x", fieldSpecs: []string{"a=env:X"}, onMissing: "explode"},
		{path: "secret/x", fieldSpecs: []string{"a=env:X"}, onMissing: "error", onMissingStandby: "loudly"},
		{path: "secret/x", fieldSpecs: []string{"bogus"}, onMissing: "error"},
	} {
		if err := runCIBaoSeed(o); err == nil {
			t.Errorf("opts %+v must fail validation", o)
		}
	}
}

func TestMaskGHALines(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "true")
	// maskGHA prints to stdout; just assert it doesn't panic on multiline +
	// blank-line input. (Output assertion would need stdout capture; the
	// per-line split is the behavior under test and is exercised via fmt.)
	maskGHALines("-----BEGIN PRIVATE KEY-----\nabc\n\ndef\n-----END PRIVATE KEY-----\n")
}

func TestBaoSeedCmdFlagWiring(t *testing.T) {
	c := ciBaoSeedCmd()
	for _, f := range []string{"path", "field", "skip-if-present", "on-missing",
		"on-missing-standby", "missing-note", "missing-note-standby",
		"missing-annotation", "summary-on-seed", "seeded-message"} {
		if c.Flags().Lookup(f) == nil {
			t.Errorf("bao-seed must define --%s", f)
		}
	}
	if got := c.Flags().Lookup("on-missing").DefValue; got != "error" {
		t.Errorf("--on-missing default = %q, want error", got)
	}
}
