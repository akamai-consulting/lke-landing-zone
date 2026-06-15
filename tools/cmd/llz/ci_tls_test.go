package main

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseOpenSSLSubject(t *testing.T) {
	cases := []struct {
		subj    string
		wantCN  string
		wantO   []string
		wantOU  []string
		wantErr bool
	}{
		{subj: "/CN=platform-otel-bootstrap-ca/O=app/OU=bootstrap",
			wantCN: "platform-otel-bootstrap-ca", wantO: []string{"app"}, wantOU: []string{"bootstrap"}},
		{subj: "/CN=just-a-cn", wantCN: "just-a-cn"},
		{subj: "/O=a/O=b/OU=x/OU=y", wantO: []string{"a", "b"}, wantOU: []string{"x", "y"}},
		{subj: "CN=no-leading-slash", wantCN: "no-leading-slash"},
		{subj: "", wantErr: true},
		{subj: "/", wantErr: true},
		{subj: "/CN", wantErr: true},        // no '='
		{subj: "/=value", wantErr: true},    // empty RDN type
		{subj: "/L=Austin", wantErr: true},  // unsupported RDN type
		{subj: "/CN=a/X=b", wantErr: true},  // unsupported after a good one
		{subj: "/CN=a//O=b", wantErr: true}, // empty RDN between slashes
	}
	for _, tc := range cases {
		name, err := parseOpenSSLSubject(tc.subj)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseOpenSSLSubject(%q) = %+v, want error", tc.subj, name)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseOpenSSLSubject(%q) error: %v", tc.subj, err)
			continue
		}
		if name.CommonName != tc.wantCN ||
			!reflect.DeepEqual(name.Organization, tc.wantO) ||
			!reflect.DeepEqual(name.OrganizationalUnit, tc.wantOU) {
			t.Errorf("parseOpenSSLSubject(%q) = CN=%q O=%v OU=%v, want CN=%q O=%v OU=%v",
				tc.subj, name.CommonName, name.Organization, name.OrganizationalUnit,
				tc.wantCN, tc.wantO, tc.wantOU)
		}
	}
}

// genTLSArgs returns a full valid arg set with the given overrides applied
// ("" deletes the flag), so validation cases stay one line each.
func genTLSArgs(overrides map[string]string) []string {
	flags := map[string]string{
		"--prefix":    "tp",
		"--ca-subj":   "/CN=test-ca/O=app/OU=bootstrap",
		"--cert-subj": "/CN=test-leaf/O=app",
		"--dns":       "svc.ns.svc,svc.ns.svc.cluster.local",
	}
	for k, v := range overrides {
		if v == "" {
			delete(flags, k)
		} else {
			flags[k] = v
		}
	}
	var args []string
	for k, v := range flags {
		args = append(args, k, v)
	}
	return args
}

// runGenTLS executes the cobra command end to end (flag parsing + RunE).
func runGenTLS(args []string) error {
	c := ciGenBootstrapTLSCmd()
	c.SetOut(io.Discard)
	c.SetErr(io.Discard)
	c.SetArgs(args)
	return c.Execute()
}

func TestGenBootstrapTLSValidation(t *testing.T) {
	t.Setenv("RUNNER_TEMP", t.TempDir()) // nothing should be written, but never touch /tmp
	cases := []struct {
		name      string
		overrides map[string]string
	}{
		{"missing prefix", map[string]string{"--prefix": ""}},
		{"missing ca-subj", map[string]string{"--ca-subj": ""}},
		{"missing cert-subj", map[string]string{"--cert-subj": ""}},
		{"missing dns", map[string]string{"--dns": ""}},
		{"bad ca-subj", map[string]string{"--ca-subj": "/CN=x/L=nope"}},
		{"bad cert-subj", map[string]string{"--cert-subj": "no-equals"}},
		{"blank dns list", map[string]string{"--dns": " , ,"}},
	}
	for _, tc := range cases {
		if err := runGenTLS(genTLSArgs(tc.overrides)); err == nil {
			t.Errorf("%s: expected an error", tc.name)
		}
	}
}

func TestGenBootstrapTLSHappyPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RUNNER_TEMP", dir)
	wantDNS := []string{"svc.ns.svc", "svc.ns.svc.cluster.local"}

	// RSA-4096 keygen is slow, so the full pipeline runs once and every
	// property is asserted off this single run.
	if err := runGenTLS(genTLSArgs(nil)); err != nil {
		t.Fatal(err)
	}

	caCert := readCert(t, filepath.Join(dir, "tp-ca.crt"))
	leaf := readCert(t, filepath.Join(dir, "tp-tls.crt"))
	caKey := readKey(t, filepath.Join(dir, "tp-ca.key"))
	tlsKey := readKey(t, filepath.Join(dir, "tp-tls.key"))

	// CA shape: self-signed v3 CA with the requested subject.
	if !caCert.IsCA || !caCert.BasicConstraintsValid {
		t.Errorf("CA cert IsCA=%v BasicConstraintsValid=%v, want both true", caCert.IsCA, caCert.BasicConstraintsValid)
	}
	if caCert.Subject.CommonName != "test-ca" ||
		!reflect.DeepEqual(caCert.Subject.Organization, []string{"app"}) ||
		!reflect.DeepEqual(caCert.Subject.OrganizationalUnit, []string{"bootstrap"}) {
		t.Errorf("CA subject = %v, want CN=test-ca O=[app] OU=[bootstrap]", caCert.Subject)
	}

	// Leaf shape: requested subject + SANs, and it chains to the CA.
	if leaf.Subject.CommonName != "test-leaf" || !reflect.DeepEqual(leaf.Subject.Organization, []string{"app"}) {
		t.Errorf("leaf subject = %v, want CN=test-leaf O=[app]", leaf.Subject)
	}
	if !reflect.DeepEqual(leaf.DNSNames, wantDNS) {
		t.Errorf("leaf SANs = %v, want %v", leaf.DNSNames, wantDNS)
	}
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: wantDNS[0]}); err != nil {
		t.Errorf("leaf does not verify against the generated CA: %v", err)
	}

	// RSA-4096 throughout, with each key matching its certificate.
	for _, p := range []struct {
		what string
		cert *x509.Certificate
		key  *rsa.PrivateKey
	}{{"ca", caCert, caKey}, {"tls", leaf, tlsKey}} {
		if got := p.key.N.BitLen(); got != 4096 {
			t.Errorf("%s key is RSA-%d, want 4096", p.what, got)
		}
		if !p.key.PublicKey.Equal(p.cert.PublicKey) {
			t.Errorf("%s key does not match its certificate", p.what)
		}
	}

	// ~365-day validity (AddDate may shift an hour across a DST boundary).
	for _, c := range []*x509.Certificate{caCert, leaf} {
		life := c.NotAfter.Sub(c.NotBefore)
		if d := (life - 365*24*time.Hour).Abs(); d > 2*time.Hour {
			t.Errorf("cert %q validity = %v, want ~365d", c.Subject.CommonName, life)
		}
	}

	// Private keys are 0600; no openssl intermediates (csr/cnf/srl) appear.
	for _, k := range []string{"tp-ca.key", "tp-tls.key"} {
		fi, err := os.Stat(filepath.Join(dir, k))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, want 0600", k, fi.Mode().Perm())
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	want := []string{"tp-ca.crt", "tp-ca.key", "tp-tls.crt", "tp-tls.key"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("output files = %v, want exactly %v", names, want)
	}
}

func TestNamespaceManifest(t *testing.T) {
	want := "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: llz-observability\n"
	if got := namespaceManifest("llz-observability"); got != want {
		t.Errorf("namespaceManifest = %q, want %q", got, want)
	}
}

func TestTLSSecretManifest(t *testing.T) {
	m := tlsSecretManifest("ns1", "sec1", []byte("CERTPEM"), []byte("KEYPEM"))
	for _, want := range []string{
		"kind: Secret",
		"name: sec1",
		"namespace: ns1",
		"type: kubernetes.io/tls",
		"tls.crt: " + base64.StdEncoding.EncodeToString([]byte("CERTPEM")),
		"tls.key: " + base64.StdEncoding.EncodeToString([]byte("KEYPEM")),
	} {
		if !strings.Contains(m, want) {
			t.Errorf("manifest missing %q:\n%s", want, m)
		}
	}
}

func TestGenBootstrapTLSK8sSecretFlags(t *testing.T) {
	// --ensure-namespace / --skip-if-secret-exists require --k8s-secret; the
	// ref must be NAMESPACE/NAME.
	for name, overrides := range map[string]map[string]string{
		"ensure-namespace alone":      {"--ensure-namespace": "true"},
		"skip-if-secret-exists alone": {"--skip-if-secret-exists": "true"},
		"bad k8s-secret ref":          {"--k8s-secret": "no-slash"},
	} {
		args := genTLSArgs(nil)
		for k, v := range overrides {
			if v == "true" {
				args = append(args, k)
			} else {
				args = append(args, k, v)
			}
		}
		if err := runGenTLS(args); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestGenBootstrapTLSSkipIfSecretExists(t *testing.T) {
	// Existing Secret → exit 0 BEFORE any keygen or kubectl apply.
	withKubectl(t, func(a string) ([]byte, error) {
		if a == "-n obs get secret bootstrap-tls" {
			return nil, nil // exists
		}
		return nil, errors.New("unexpected: " + a)
	})
	prevApply := kubectlApplyFn
	kubectlApplyFn = func(m string) error {
		t.Errorf("unexpected kubectl apply:\n%s", m)
		return nil
	}
	t.Cleanup(func() { kubectlApplyFn = prevApply })
	err := runGenTLS(append(genTLSArgs(nil),
		"--k8s-secret", "obs/bootstrap-tls", "--skip-if-secret-exists"))
	if err != nil {
		t.Fatalf("existing Secret must skip cleanly: %v", err)
	}
}

func TestGenBootstrapTLSAppliesK8sSecret(t *testing.T) {
	t.Setenv("RUNNER_TEMP", t.TempDir()) // nothing may be written here
	withKubectl(t, func(string) ([]byte, error) { return nil, errors.New("NotFound") })
	var manifests []string
	prevApply := kubectlApplyFn
	kubectlApplyFn = func(m string) error { manifests = append(manifests, m); return nil }
	t.Cleanup(func() { kubectlApplyFn = prevApply })

	err := runGenTLS(append(genTLSArgs(nil),
		"--k8s-secret", "obs/bootstrap-tls", "--ensure-namespace", "--skip-if-secret-exists"))
	if err != nil {
		t.Fatal(err)
	}
	if len(manifests) != 2 {
		t.Fatalf("want Namespace + Secret applies, got %d:\n%s", len(manifests), strings.Join(manifests, "\n---\n"))
	}
	if !strings.Contains(manifests[0], "kind: Namespace") || !strings.Contains(manifests[0], "name: obs") {
		t.Errorf("first apply must be the Namespace:\n%s", manifests[0])
	}
	sec := manifests[1]
	for _, want := range []string{"type: kubernetes.io/tls", "name: bootstrap-tls", "namespace: obs"} {
		if !strings.Contains(sec, want) {
			t.Errorf("Secret manifest missing %q:\n%s", want, sec)
		}
	}
	// The embedded cert must parse, chain shape intact (CA-signed leaf).
	var data struct {
		crt string
	}
	for _, line := range strings.Split(sec, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "tls.crt: "); ok {
			data.crt = v
		}
	}
	der, err := base64.StdEncoding.DecodeString(data.crt)
	if err != nil {
		t.Fatalf("tls.crt not base64: %v", err)
	}
	block, _ := pem.Decode(der)
	if block == nil {
		t.Fatal("tls.crt not PEM")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if leaf.Subject.CommonName != "test-leaf" || len(leaf.DNSNames) != 2 {
		t.Errorf("leaf = CN=%q SANs=%v", leaf.Subject.CommonName, leaf.DNSNames)
	}
	// No key material may have touched the runner's filesystem.
	entries, err := os.ReadDir(os.Getenv("RUNNER_TEMP"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("--k8s-secret must write no files, found %v", entries)
	}
}

func readCert(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	block := readPEM(t, path, "CERTIFICATE")
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return cert
}

func readKey(t *testing.T, path string) *rsa.PrivateKey {
	t.Helper()
	block := readPEM(t, path, "PRIVATE KEY")
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return key.(*rsa.PrivateKey)
}

func readPEM(t *testing.T, path, wantType string) *pem.Block {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	block, rest := pem.Decode(data)
	if block == nil || block.Type != wantType || len(rest) != 0 {
		t.Fatalf("%s: want a single %s PEM block", path, wantType)
	}
	return block
}
