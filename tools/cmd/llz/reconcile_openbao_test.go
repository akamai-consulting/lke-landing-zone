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
}

func (f *fakeProbe) SealStatus(context.Context) (openbao.SealInfo, error) { return f.seal, f.sealErr }
func (f *fakeProbe) MetadataUpdatedTime(_ context.Context, path string) (time.Time, bool, error) {
	if f.metaErr != nil {
		return time.Time{}, false, f.metaErr
	}
	t, ok := f.updated[path]
	return t, ok, nil
}

func withOpenbaoSeams(t *testing.T, p openbaoProbe, loginErr, jwtErr error) {
	t.Helper()
	oc, ol, oj := openbaoClientFn, openbaoLoginFn, openbaoJWTFn
	openbaoClientFn = func(string, string) openbaoProbe { return p }
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
		`llz_credential_age_days{cred="loki-object-store"} 100`,
		`llz_credential_age_days{cred="harbor-registry-s3"} 10`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
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
