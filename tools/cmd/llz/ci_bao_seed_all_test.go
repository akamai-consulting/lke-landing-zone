package main

import (
	"errors"
	"strings"
	"testing"
)

// TestBootstrapSeedsTable pins the data-driven table the workflow relies on:
// the generic seeds, each with a valid on-missing mode (runCIBaoSeed rejects
// an empty one) and parseable field specs, and region interpolated into the
// infra-<region> references the inline steps built from ${REGION}.
func TestBootstrapSeedsTable(t *testing.T) {
	seeds := bootstrapSeeds("primary")
	// secret/linode/api-token left the table: mint-bootstrap-pat seeds the
	// narrow in-cluster PAT there (ci_incluster_pat.go).
	wantPaths := []string{
		"secret/infra/github-dispatch-token",
		"secret/cert-automation/github-token",
	}
	if len(seeds) != len(wantPaths) {
		t.Fatalf("bootstrapSeeds returned %d entries, want %d", len(seeds), len(wantPaths))
	}
	for i, o := range seeds {
		if o.path != wantPaths[i] {
			t.Errorf("seed %d path = %q, want %q", i, o.path, wantPaths[i])
		}
		if !validOnMissing(o.onMissing) {
			t.Errorf("seed %s has invalid on-missing %q (runCIBaoSeed would reject it)", o.path, o.onMissing)
		}
		if len(o.fieldSpecs) == 0 {
			t.Errorf("seed %s has no field specs", o.path)
		}
		for _, spec := range o.fieldSpecs {
			if _, err := parseSeedField(spec); err != nil {
				t.Errorf("seed %s field %q does not parse: %v", o.path, spec, err)
			}
		}
	}
	// Region interpolation reached the dispatch-token annotation.
	dispatch := seeds[0]
	if !strings.Contains(strings.Join(dispatch.missingAnnotations, " "), "infra-primary") {
		t.Errorf("dispatch-token annotations missing infra-primary: %v", dispatch.missingAnnotations)
	}
}

// TestRunCIBaoSeedAllSeedsEvery drives the whole table with every source
// present (env secrets set, nothing pre-seeded) and asserts every path is
// kv-put, in table order.
func TestRunCIBaoSeedAllSeedsEvery(t *testing.T) {
	t.Setenv("OPENBAO_ROOT_TOKEN", "root")
	t.Setenv("OPENBAO_SECRETS_WRITE_TOKEN", "ghp_dispatch")
	t.Setenv("HA_ROLE", "")
	puts := stubBaoSeedKV(t, "", "") // every `kv get` reports absent → skip-if-present never skips
	if err := runCIBaoSeedAll("primary"); err != nil {
		t.Fatalf("runCIBaoSeedAll: %v", err)
	}
	var gotPaths []string
	for _, p := range *puts {
		gotPaths = append(gotPaths, p[2]) // args = kv put <path> ...
	}
	want := []string{
		"secret/infra/github-dispatch-token",
		"secret/cert-automation/github-token",
	}
	if strings.Join(gotPaths, " ") != strings.Join(want, " ") {
		t.Errorf("seeded paths = %v, want %v", gotPaths, want)
	}
}

// TestRunCIBaoSeedAllAbortsOnPutFailure proves a genuine kv-put failure stops
// the driver before the remaining seeds — the same job-aborting behavior a
// failed inline seed step had (no continue-on-error on the generic seeds).
func TestRunCIBaoSeedAllAbortsOnPutFailure(t *testing.T) {
	t.Setenv("OPENBAO_ROOT_TOKEN", "root")
	t.Setenv("OPENBAO_SECRETS_WRITE_TOKEN", "ghp_dispatch")
	t.Setenv("LOKI_S3_ACCESS_KEY", "ak")
	t.Setenv("LOKI_S3_SECRET_KEY", "sk")
	t.Setenv("HA_ROLE", "")
	puts := 0
	withBaoExec(t, func(_, _, _ string, args ...string) (string, string, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(joined, "kv get"):
			return "", "absent", errors.New("exit 2")
		case strings.HasPrefix(joined, "kv put"):
			puts++
			return "", "Code: 503 sealed", errors.New("exit 2") // every put fails
		}
		return "", "unexpected " + joined, errors.New("unexpected")
	})
	err := runCIBaoSeedAll("primary")
	if err == nil || !strings.Contains(err.Error(), "secret/infra/github-dispatch-token") {
		t.Errorf("err = %v, want abort on the first seed (secret/infra/github-dispatch-token)", err)
	}
	if puts != 1 {
		t.Errorf("kv put attempts = %d, want 1 (driver must abort before later seeds)", puts)
	}
}

func TestRunCIBaoSeedAllRequiresRegion(t *testing.T) {
	if err := runCIBaoSeedAll(""); err == nil || !strings.Contains(err.Error(), "--region") {
		t.Errorf("runCIBaoSeedAll(\"\") = %v, want --region error", err)
	}
	if c := ciBaoSeedAllCmd(); c.Use != "bao-seed-all" {
		t.Errorf("Use = %q, want bao-seed-all", c.Use)
	}
}
