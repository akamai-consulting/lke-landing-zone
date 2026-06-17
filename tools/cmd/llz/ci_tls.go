package main

// ci_tls.go implements `llz ci gen-bootstrap-tls` — the native port of
// gen-bootstrap-tls.sh, generating the temporary CA + SAN serving cert with
// crypto/x509 instead of shelling to openssl. Going native drops the openssl
// intermediates (-tls.csr, -san.cnf, .srl): the only caller (the "Ensure OTel
// Collector bootstrap TLS cert exists" workflow step) consumes just the
// -tls.crt/-tls.key pair, and the CSR/config/serial files were artifacts of
// the openssl request flow, not outputs anyone reads.
//
// With --k8s-secret the command also absorbs that caller's kubectl plumbing:
// it applies the kubernetes.io/tls Secret (and, with --ensure-namespace, the
// Namespace) directly from the in-memory PEMs — no key material ever touches
// the runner's filesystem — and --skip-if-secret-exists makes the whole step
// idempotent without an inline kubectl probe.

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func ciGenBootstrapTLSCmd() *cobra.Command {
	var prefix, caSubj, certSubj, dns, k8sSecret string
	var ensureNamespace, skipIfSecretExists bool
	c := &cobra.Command{
		Use:   "gen-bootstrap-tls",
		Short: "generate a temporary CA + SAN TLS cert pair ($RUNNER_TEMP files or a K8s Secret)",
		Long: "Native port of gen-bootstrap-tls.sh. Generates a throwaway RSA-4096 CA and a\n" +
			"CA-signed serving cert with the given DNS SANs (365-day validity each).\n" +
			"Subjects use the openssl one-line form (/CN=…/O=…/OU=…). cert-manager\n" +
			"replaces the cert once the real issuer is up. Without --k8s-secret the pair\n" +
			"lands in $RUNNER_TEMP (or /tmp) as <prefix>-ca.{key,crt} + <prefix>-tls.\n" +
			"{key,crt} for the caller to consume. With --k8s-secret NS/NAME the\n" +
			"kubernetes.io/tls Secret is applied directly from memory (no key files on\n" +
			"the runner); --ensure-namespace applies the Namespace first, and\n" +
			"--skip-if-secret-exists exits 0 when the Secret is already there.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runCIGenBootstrapTLS(prefix, caSubj, certSubj, dns, k8sSecret, ensureNamespace, skipIfSecretExists)
		},
	}
	f := c.Flags()
	f.StringVar(&prefix, "prefix", "", "output filename prefix, e.g. platform-otel-bootstrap (required)")
	f.StringVar(&caSubj, "ca-subj", "", "CA subject in openssl form, e.g. /CN=my-ca/O=app (required)")
	f.StringVar(&certSubj, "cert-subj", "", "serving-cert subject in openssl form (required)")
	f.StringVar(&dns, "dns", "", "comma-separated DNS SANs for the serving cert (required)")
	f.StringVar(&k8sSecret, "k8s-secret", "", "apply the pair as a kubernetes.io/tls Secret NAMESPACE/NAME instead of writing files")
	f.BoolVar(&ensureNamespace, "ensure-namespace", false, "apply the --k8s-secret Namespace before the Secret")
	f.BoolVar(&skipIfSecretExists, "skip-if-secret-exists", false, "exit 0 without generating when the --k8s-secret Secret already exists")
	return c
}

// parseOpenSSLSubject parses an openssl one-line subject (/CN=…/O=…/OU=…) into
// a pkix.Name. Only the RDN types the bootstrap subjects use (CN, O, OU) are
// supported — anything else is an error rather than a silent drop, so a
// workflow typo fails the step instead of issuing a cert with a wrong subject.
func parseOpenSSLSubject(subj string) (pkix.Name, error) {
	var name pkix.Name
	s := strings.TrimPrefix(subj, "/")
	if s == "" {
		return name, fmt.Errorf("empty subject %q", subj)
	}
	for _, rdn := range strings.Split(s, "/") {
		typ, val, ok := strings.Cut(rdn, "=")
		if !ok {
			return name, fmt.Errorf("subject %q: %q is not TYPE=value", subj, rdn)
		}
		switch typ {
		case "CN":
			name.CommonName = val
		case "O":
			name.Organization = append(name.Organization, val)
		case "OU":
			name.OrganizationalUnit = append(name.OrganizationalUnit, val)
		default:
			return name, fmt.Errorf("subject %q: unsupported RDN type %q (only CN, O, OU)", subj, typ)
		}
	}
	return name, nil
}

func runCIGenBootstrapTLS(prefix, caSubj, certSubj, dnsCSV, k8sSecret string, ensureNamespace, skipIfSecretExists bool) error {
	if prefix == "" || caSubj == "" || certSubj == "" || dnsCSV == "" {
		return fmt.Errorf("--prefix, --ca-subj, --cert-subj and --dns are all required")
	}
	if (ensureNamespace || skipIfSecretExists) && k8sSecret == "" {
		return fmt.Errorf("--ensure-namespace/--skip-if-secret-exists require --k8s-secret")
	}
	var secretNS, secretName string
	if k8sSecret != "" {
		var ok bool
		if secretNS, secretName, ok = strings.Cut(k8sSecret, "/"); !ok || secretNS == "" || secretName == "" {
			return fmt.Errorf("--k8s-secret must be NAMESPACE/NAME, got %q", k8sSecret)
		}
	}
	caName, err := parseOpenSSLSubject(caSubj)
	if err != nil {
		return fmt.Errorf("--ca-subj: %w", err)
	}
	certName, err := parseOpenSSLSubject(certSubj)
	if err != nil {
		return fmt.Errorf("--cert-subj: %w", err)
	}
	var dnsNames []string
	for _, d := range strings.Split(dnsCSV, ",") {
		if d = strings.TrimSpace(d); d != "" {
			dnsNames = append(dnsNames, d)
		}
	}
	if len(dnsNames) == 0 {
		return fmt.Errorf("--dns %q contains no DNS names", dnsCSV)
	}

	// Idempotency probe BEFORE the (slow) keygen: an existing Secret means a
	// previous bootstrap (or cert-manager itself) already provided the cert.
	if skipIfSecretExists {
		if kExists("-n", secretNS, "get", "secret", secretName) {
			fmt.Printf("%s Secret already exists — skipping bootstrap cert generation.\n", secretName)
			return nil
		}
		fmt.Printf("%s not found — generating self-signed bootstrap certificate.\n", secretName)
	}

	caKey, caDER, err := genCA(caName, 365)
	if err != nil {
		return err
	}
	tlsKey, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		return fmt.Errorf("generate TLS key: %w", err)
	}
	// Re-parse the CA so the leaf is signed against the cert as issued (and
	// picks up the generated SubjectKeyId as its AuthorityKeyId).
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return fmt.Errorf("parse CA certificate: %w", err)
	}

	leafTmpl := &x509.Certificate{
		SerialNumber: randomSerial(),
		Subject:      certName,
		DNSNames:     dnsNames,
		NotBefore:    caCert.NotBefore,
		NotAfter:     caCert.NotAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &tlsKey.PublicKey, caKey)
	if err != nil {
		return fmt.Errorf("create TLS certificate: %w", err)
	}

	// --k8s-secret: apply Namespace + Secret straight from memory; no key
	// material ever lands on the runner's filesystem.
	if k8sSecret != "" {
		if ensureNamespace {
			if err := kubectlApplyFn(namespaceManifest(secretNS)); err != nil {
				return fmt.Errorf("apply namespace %s: %w", secretNS, err)
			}
		}
		certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
		keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8(tlsKey)})
		if err := kubectlApplyFn(tlsSecretManifest(secretNS, secretName, certPEM, keyPEM)); err != nil {
			return fmt.Errorf("apply Secret %s/%s: %w", secretNS, secretName, err)
		}
		fmt.Printf("%s bootstrap Secret created in %s namespace.\n", secretName, secretNS)
		fmt.Println("cert-manager will replace it once the custom-ca issuer is up.")
		return nil
	}

	dir := os.Getenv("RUNNER_TEMP")
	if dir == "" {
		dir = "/tmp"
	}
	for _, f := range []struct {
		name, pemType string
		der           []byte
		mode          os.FileMode
	}{
		{prefix + "-ca.key", "PRIVATE KEY", pkcs8(caKey), 0o600},
		{prefix + "-ca.crt", "CERTIFICATE", caDER, 0o644},
		{prefix + "-tls.key", "PRIVATE KEY", pkcs8(tlsKey), 0o600},
		{prefix + "-tls.crt", "CERTIFICATE", leafDER, 0o644},
	} {
		path := filepath.Join(dir, f.name)
		pemBytes := pem.EncodeToMemory(&pem.Block{Type: f.pemType, Bytes: f.der})
		if err := os.WriteFile(path, pemBytes, f.mode); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	fmt.Printf("Wrote %s/%s-{ca,tls}.{key,crt} (SANs: %s)\n", dir, prefix, strings.Join(dnsNames, ", "))
	return nil
}

// genCA generates an RSA-4096 key + self-signed CA certificate valid for
// days — used by gen-bootstrap-tls (365d throwaway CA).
func genCA(subject pkix.Name, days int) (*rsa.PrivateKey, []byte, error) {
	key, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		return nil, nil, fmt.Errorf("generate CA key: %w", err)
	}
	notBefore := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          randomSerial(),
		Subject:               subject,
		NotBefore:             notBefore,
		NotAfter:              notBefore.AddDate(0, 0, days),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("create CA certificate: %w", err)
	}
	return key, der, nil
}

// namespaceManifest renders a bare Namespace — the native form of
// `kubectl create namespace … --dry-run=client -o yaml | kubectl apply -f -`.
func namespaceManifest(ns string) string {
	return fmt.Sprintf("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n", ns)
}

// tlsSecretManifest renders a kubernetes.io/tls Secret from PEM pair — the
// native form of `kubectl create secret tls … --dry-run=client -o yaml |
// kubectl apply -f -`.
func tlsSecretManifest(ns, name string, certPEM, keyPEM []byte) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %s
  namespace: %s
type: kubernetes.io/tls
data:
  tls.crt: %s
  tls.key: %s
`, name, ns, base64.StdEncoding.EncodeToString(certPEM), base64.StdEncoding.EncodeToString(keyPEM))
}

// randomSerial returns a random 128-bit certificate serial (what openssl's
// -CAcreateserial flow provided in the script).
func randomSerial() *big.Int {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		// crypto/rand failing means no key material either — unreachable in
		// practice because rsa.GenerateKey would have failed first.
		panic(err)
	}
	return serial
}

// pkcs8 marshals an RSA key as PKCS#8 DER (the form openssl 3 emits, and what
// kubectl create secret tls expects under "PRIVATE KEY").
func pkcs8(key *rsa.PrivateKey) []byte {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		panic(err) // cannot fail for an *rsa.PrivateKey
	}
	return der
}
