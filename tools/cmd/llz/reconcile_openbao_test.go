package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/metrics"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/openbao"
)

type fakeProbe struct {
	seal    openbao.SealInfo
	sealErr error
	updated map[string]time.Time // path → updated_time; absent → ok=false
	metaErr error
	listed  []string // db-admin collection children; nil → ok=false (no databases)
	listErr error
}

func (f *fakeProbe) SealStatus(context.Context) (openbao.SealInfo, error) { return f.seal, f.sealErr }
func (f *fakeProbe) MetadataUpdatedTime(_ context.Context, path string) (time.Time, bool, error) {
	if f.metaErr != nil {
		return time.Time{}, false, f.metaErr
	}
	t, ok := f.updated[path]
	return t, ok, nil
}
func (f *fakeProbe) MetadataList(_ context.Context, _ string) ([]string, bool, error) {
	if f.listErr != nil {
		return nil, false, f.listErr
	}
	if f.listed == nil {
		return nil, false, nil
	}
	return f.listed, true, nil
}

func withOpenbaoSeams(t *testing.T, p openbaoProbe, loginErr, jwtErr error) {
	t.Helper()
	oc, ol, oj := openbaoClientFn, openbaoLoginFn, openbaoJWTFn
	openbaoClientFn = func(string, string) (openbaoProbe, error) { return p, nil }
	openbaoLoginFn = func(context.Context, string, string) (string, error) { return "tok", loginErr }
	openbaoJWTFn = func() (string, error) { return "jwt", jwtErr }
	t.Cleanup(func() { openbaoClientFn, openbaoLoginFn, openbaoJWTFn = oc, ol, oj })
}

func obMetrics(t *testing.T, reg *metrics.Registry) string {
	t.Helper()
	var b strings.Builder
	if _, err := reg.WriteTo(&b); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	return b.String()
}

func TestSampleOpenBaoHealthy(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	p := &fakeProbe{
		seal: openbao.SealInfo{Sealed: false, Initialized: true},
		updated: map[string]time.Time{
			"secret/loki/object-store":  now.Add(-100 * 24 * time.Hour),
			"secret/harbor/registry-s3": now.Add(-10 * 24 * time.Hour),
			// The platform credentials the gauge set was widened to cover. They
			// had no age visibility at all before, and each carries a class that
			// is NOT "automated" — which is what keeps them off the SLA alert.
			"secret/grafana/admin": now.Add(-400 * 24 * time.Hour),
			"secret/otel/ingress":  now.Add(-400 * 24 * time.Hour),
			"secret/harbor/admin":  now.Add(-5 * 24 * time.Hour),
			// The narrow in-cluster PAT: real monthly rotation, so `automated`
			// and on the 90d SLA — it was the one rotating credential with no
			// watchdog over the rotation.
			"secret/linode/api-token": now.Add(-31 * 24 * time.Hour),
			// The broad account read_write PAT: weekly broadPatRotator, so `automated`
			// and on the 90d SLA. Its expiry was already visible via the Linode
			// enumeration; its ROTATION AGE was the blind spot.
			"secret/linode/broad-pat": now.Add(-9 * 24 * time.Hour),
			// Opt-in, operator-re-seeded, documented ≤ 90d policy → on-demand, not static.
			"secret/linode/cloud-firewall": now.Add(-120 * 24 * time.Hour),
			// Bootstrap seeds nothing ever rotates.
			"secret/harbor/robot":                now.Add(-500 * 24 * time.Hour),
			"secret/infra/github-dispatch-token": now.Add(-200 * 24 * time.Hour),
			"secret/infra/apl-values-repo-token": now.Add(-210 * 24 * time.Hour),
		},
	}
	withOpenbaoSeams(t, p, nil, nil)
	reg := metrics.NewRegistry()
	if err := sampleOpenBao(context.Background(), reg, now); err != nil {
		t.Fatalf("sampleOpenBao: %v", err)
	}
	out := obMetrics(t, reg)
	for _, want := range []string{
		"llz_openbao_sealed 0",
		"llz_openbao_initialized 1",
		`llz_credential_age_days{class="automated",cred="loki-object-store"} 100`,
		`llz_credential_age_days{class="automated",cred="harbor-registry-s3"} 10`,
		`llz_credential_age_days{class="generate-once",cred="grafana-admin"} 400`,
		`llz_credential_age_days{class="generate-once",cred="otel-ingress"} 400`,
		`llz_credential_age_days{class="tracks-source",cred="harbor-admin"} 5`,
		`llz_credential_age_days{class="automated",cred="linode-incluster-pat"} 31`,
		`llz_credential_age_days{class="static",cred="harbor-robot"} 500`,
		`llz_credential_age_days{class="static",cred="infra-github-dispatch-token"} 200`,
		`llz_credential_age_days{class="automated",cred="linode-broad-pat"} 9`,
		`llz_credential_age_days{class="on-demand",cred="linode-cloud-firewall"} 120`,
		`llz_credential_age_days{class="static",cred="infra-apl-values-repo-token"} 210`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

// Every credPaths entry must have a matching metadata-read grant in
// policyReconcilerRead. A missing grant is a 403, and sampleOpenBao treats any
// non-404 failure as fatal — so one ungranted path silently takes down the seal
// gauge and every OTHER credential's age with it, which is the opposite of what
// widening the coverage was for.
func TestCredPathsAreGrantedInReconcilerPolicy(t *testing.T) {
	for _, cp := range credPaths {
		meta := strings.Replace(cp.path, "secret/", "secret/metadata/", 1)
		if !strings.Contains(policyReconcilerRead, `path "`+meta+`"`) {
			t.Errorf("credPaths has %s but policyReconcilerRead grants no read on %q — the sampler will 403 and fail the whole lane", cp.path, meta)
		}
		switch cp.class {
		case credClassAutomated, credClassGenerateOnce, credClassTracksSource, credClassStatic, credClassOnDemand:
		default:
			t.Errorf("credPaths entry %s has unknown class %q; the alert rules match on the known set", cp.cred, cp.class)
		}
	}
}

// The db-admin paths are discovered at sample time, so the pin test above cannot
// see them — their grants are a LIST on the collection plus a metadata-read
// prefix, and BOTH are needed: without the list the sampler 403s on discovery,
// without the read it 403s on the first cluster it finds. Either one fails the
// whole lane, not just the db series.
func TestDBAdminGrantsInReconcilerPolicy(t *testing.T) {
	for _, want := range []string{
		`path "secret/metadata/infra/db-admin/" { capabilities = ["list"] }`,
		`path "secret/metadata/infra/db-admin/*" { capabilities = ["read"] }`,
	} {
		if !strings.Contains(policyReconcilerRead, want) {
			t.Errorf("policyReconcilerRead is missing the db-admin grant %q", want)
		}
	}
	// The reconciler must never be able to read a database admin password — only
	// its metadata. A data grant here would be a real privilege escalation.
	// Matched on the `path "` prefix, not the bare string: the policy's own HCL
	// comment names secret/data/infra/db-admin/* to say it is NOT granted.
	if strings.Contains(policyReconcilerRead, `path "secret/data/infra/db-admin`) {
		t.Error("policyReconcilerRead grants DATA on infra/db-admin; the age sampler needs metadata only")
	}
}

// A deployment with databases gets one series per cluster, discovered from the
// KV collection rather than declared in credPaths — so a cluster added later is
// covered with no code change.
func TestSampleOpenBaoDBAdminDiscovered(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	p := &fakeProbe{
		seal:   openbao.SealInfo{Initialized: true},
		listed: []string{"shared", "analytics"},
		updated: map[string]time.Time{
			"secret/infra/db-admin/shared":    now.Add(-30 * 24 * time.Hour),
			"secret/infra/db-admin/analytics": now.Add(-400 * 24 * time.Hour),
		},
	}
	withOpenbaoSeams(t, p, nil, nil)
	reg := metrics.NewRegistry()
	if err := sampleOpenBao(context.Background(), reg, now); err != nil {
		t.Fatalf("sampleOpenBao: %v", err)
	}
	out := obMetrics(t, reg)
	for _, want := range []string{
		`llz_credential_age_days{class="on-demand",cred="db-admin-shared"} 30`,
		`llz_credential_age_days{class="on-demand",cred="db-admin-analytics"} 400`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

// A deployment with no databases lists nothing (KV v2 404s an empty collection).
// That is the common case and must not be an error — it would take the seal
// gauge down with it.
func TestSampleOpenBaoNoDatabasesIsNotAnError(t *testing.T) {
	p := &fakeProbe{seal: openbao.SealInfo{Initialized: true}, updated: map[string]time.Time{}}
	withOpenbaoSeams(t, p, nil, nil)
	reg := metrics.NewRegistry()
	if err := sampleOpenBao(context.Background(), reg, time.Unix(1, 0)); err != nil {
		t.Fatalf("sampleOpenBao with no db-admin collection: %v", err)
	}
	if out := obMetrics(t, reg); strings.Contains(out, "db-admin") {
		t.Errorf("published a db-admin series with nothing listed:\n%s", out)
	}
}

// Discovery must not mutate the package-level credPaths: two passes in a row on
// the same process would otherwise accumulate (or overwrite) entries.
func TestSampleOpenBaoDiscoveryDoesNotMutateCredPaths(t *testing.T) {
	before := len(credPaths)
	p := &fakeProbe{
		seal:    openbao.SealInfo{Initialized: true},
		listed:  []string{"shared"},
		updated: map[string]time.Time{"secret/infra/db-admin/shared": time.Unix(1, 0)},
	}
	withOpenbaoSeams(t, p, nil, nil)
	for i := 0; i < 2; i++ {
		if err := sampleOpenBao(context.Background(), metrics.NewRegistry(), time.Unix(2, 0)); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
	if len(credPaths) != before {
		t.Errorf("credPaths grew from %d to %d across sample passes", before, len(credPaths))
	}
}

func TestSampleOpenBaoSealed(t *testing.T) {
	p := &fakeProbe{seal: openbao.SealInfo{Sealed: true, Initialized: true}}
	withOpenbaoSeams(t, p, nil, nil)
	reg := metrics.NewRegistry()
	if err := sampleOpenBao(context.Background(), reg, time.Unix(1, 0)); err != nil {
		t.Fatalf("sampleOpenBao: %v", err)
	}
	if !strings.Contains(obMetrics(t, reg), "llz_openbao_sealed 1") {
		t.Error("want sealed 1")
	}
}

func TestSampleOpenBaoCredNotSeededSkipped(t *testing.T) {
	// No credential paths present → no age gauge, but seal still published, no error.
	p := &fakeProbe{seal: openbao.SealInfo{Initialized: true}, updated: map[string]time.Time{}}
	withOpenbaoSeams(t, p, nil, nil)
	reg := metrics.NewRegistry()
	if err := sampleOpenBao(context.Background(), reg, time.Unix(1, 0)); err != nil {
		t.Fatalf("sampleOpenBao: %v", err)
	}
	if strings.Contains(obMetrics(t, reg), "llz_credential_age_days") {
		t.Error("no age gauge should be set when a path is not seeded")
	}
}

func TestSampleOpenBaoErrors(t *testing.T) {
	now := time.Unix(1, 0)
	cases := []struct {
		name       string
		p          *fakeProbe
		login, jwt error
	}{
		{"seal error", &fakeProbe{sealErr: errors.New("unreachable")}, nil, nil},
		{"jwt error", &fakeProbe{seal: openbao.SealInfo{Initialized: true}}, nil, errors.New("no token")},
		{"login error", &fakeProbe{seal: openbao.SealInfo{Initialized: true}}, errors.New("403"), nil},
		{"metadata error", &fakeProbe{seal: openbao.SealInfo{Initialized: true}, metaErr: errors.New("500")}, nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withOpenbaoSeams(t, c.p, c.login, c.jwt)
			if err := sampleOpenBao(context.Background(), metrics.NewRegistry(), now); err == nil {
				t.Errorf("%s should surface an error", c.name)
			}
		})
	}
}
