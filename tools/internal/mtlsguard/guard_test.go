package mtlsguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/openbao"
	"gopkg.in/yaml.v3"
)

func mustPod(t *testing.T, y string) mtlsPodDoc {
	t.Helper()
	var d mtlsPodDoc
	if err := yaml.Unmarshal([]byte(y), &d); err != nil {
		t.Fatal(err)
	}
	return d
}

const wiredDeployment = `
kind: Deployment
metadata: {name: llz-reconciler, namespace: llz-reconciler}
spec:
  template:
    spec:
      containers:
        - name: reconcile
          env:
            - {name: OPENBAO_ADDR, value: "https://openbao:8200"}
            - {name: OPENBAO_CA_FILE, value: /etc/openbao-ca/ca.crt}
          volumeMounts:
            - {name: ca, mountPath: /etc/openbao-ca}
            - {name: id, mountPath: /etc/openbao-client-tls}
      volumes:
        - {name: ca, secret: {secretName: openbao-ca-bundle}}
        - {name: id, secret: {secretName: llz-reconciler-client-tls}}
`

func certsFor(names ...string) map[string]bool {
	m := map[string]bool{}
	for _, n := range names {
		m[n] = true
	}
	return m
}

func TestMTLSWiringAcceptsAWiredPod(t *testing.T) {
	certs := certsFor("llz-reconciler/openbao-ca-bundle", "llz-reconciler/llz-reconciler-client-tls")
	if got := checkMTLSWiring("d.yaml", mustPod(t, wiredDeployment), certs); len(got) != 0 {
		t.Errorf("a correctly wired pod must pass, got %+v", got)
	}
}

// TestMTLSWiringCatchesMissingClientCert is the regression test for the defect
// that motivated this guard: removing the client-certificate mount left the pod
// unable to reach OpenBao while every other gate in the repo reported color.Green.
func TestMTLSWiringCatchesMissingClientCert(t *testing.T) {
	y := strings.Replace(wiredDeployment, "            - {name: id, mountPath: /etc/openbao-client-tls}\n", "", 1)
	certs := certsFor("llz-reconciler/openbao-ca-bundle", "llz-reconciler/llz-reconciler-client-tls")
	got := checkMTLSWiring("d.yaml", mustPod(t, y), certs)
	if len(got) == 0 {
		t.Fatal("removing the client-cert mount must be a finding")
	}
	if !strings.Contains(got[0].problem, "/etc/openbao-client-tls") {
		t.Errorf("finding should name the missing path, got %q", got[0].problem)
	}
}

func TestMTLSWiringCatchesMissingCA(t *testing.T) {
	y := strings.Replace(wiredDeployment, "            - {name: ca, mountPath: /etc/openbao-ca}\n", "", 1)
	certs := certsFor("llz-reconciler/openbao-ca-bundle", "llz-reconciler/llz-reconciler-client-tls")
	if got := checkMTLSWiring("d.yaml", mustPod(t, y), certs); len(got) == 0 {
		t.Error("removing the CA mount must be a finding")
	}
}

// TestMTLSWiringRejectsSkipVerify: ADR 0010 deleted the flag because its failure
// mode was a silent downgrade. A pod with the mounts AND the flag would
// otherwise pass a mounts-only check.
func TestMTLSWiringRejectsSkipVerify(t *testing.T) {
	y := strings.Replace(wiredDeployment,
		"            - {name: OPENBAO_ADDR, value: \"https://openbao:8200\"}",
		"            - {name: OPENBAO_ADDR, value: \"https://openbao:8200\"}\n            - {name: OPENBAO_SKIP_VERIFY, value: \"true\"}", 1)
	certs := certsFor("llz-reconciler/openbao-ca-bundle", "llz-reconciler/llz-reconciler-client-tls")
	got := checkMTLSWiring("d.yaml", mustPod(t, y), certs)
	if len(got) == 0 || !strings.Contains(got[0].problem, "OPENBAO_SKIP_VERIFY") {
		t.Errorf("re-adding OPENBAO_SKIP_VERIFY must be a finding, got %+v", got)
	}
}

// TestMTLSWiringCatchesSecretWithNoCertificate: the mount exists but nothing
// creates the Secret — invisible until the pod sits in ContainerCreating.
func TestMTLSWiringCatchesSecretWithNoCertificate(t *testing.T) {
	certs := certsFor("llz-reconciler/openbao-ca-bundle") // client-tls Certificate absent
	got := checkMTLSWiring("d.yaml", mustPod(t, wiredDeployment), certs)
	if len(got) == 0 || !strings.Contains(got[0].problem, "no Certificate") {
		t.Errorf("a mounted Secret with no Certificate must be a finding, got %+v", got)
	}
}

// TestMTLSWiringIgnoresNonConsumers: a pod that never talks to OpenBao must not
// be asked for TLS material. Inferring the requirement from OPENBAO_ADDR is what
// keeps the guard allowlist-free, so this is the property that keeps it usable.
func TestMTLSWiringIgnoresNonConsumers(t *testing.T) {
	y := strings.Replace(wiredDeployment, "            - {name: OPENBAO_ADDR, value: \"https://openbao:8200\"}\n", "", 1)
	if got := checkMTLSWiring("d.yaml", mustPod(t, y), map[string]bool{}); len(got) != 0 {
		t.Errorf("a non-consumer must not be required to mount anything, got %+v", got)
	}
}

// TestMTLSWiringHandlesCronJobNesting: CronJobs nest the pod template one level
// deeper, and two of the three OpenBao consumers are CronJobs — a guard that
// only understood Deployments would silently skip them.
func TestMTLSWiringHandlesCronJobNesting(t *testing.T) {
	y := `
kind: CronJob
metadata: {name: broad-pat-rotator, namespace: llz-pat-rotator}
spec:
  jobTemplate:
    spec:
      template:
        spec:
          containers:
            - name: rotate
              env:
                - {name: OPENBAO_ADDR, value: "https://openbao:8200"}
              volumeMounts: []
`
	got := checkMTLSWiring("c.yaml", mustPod(t, y), map[string]bool{})
	if len(got) == 0 {
		t.Fatal("a CronJob consumer with no mounts must be a finding — the nesting must not hide it")
	}
}

// TestMTLSWiringRealTree runs the guard over the actual manifests. This is the
// test that fails when someone drops a mount from a real workload.
func TestMTLSWiringRealTree(t *testing.T) {
	root := repoRootForMTLSTest(t)
	if err := Run(root); err != nil {
		t.Errorf("mtls-wiring-guard failed on the real tree: %v", err)
	}
}

// TestMTLSWiringDefaultsMatchClientCode pins the guard's expected paths to the
// ones openbao.InClusterHTTPClient() actually reads. If the client code moves a
// default and this is not updated, the guard would assert the wrong invariant
// and pass a genuinely broken pod.
func TestMTLSWiringDefaultsMatchClientCode(t *testing.T) {
	if openbao.DefaultCAFile != "/etc/openbao-ca/ca.crt" {
		t.Errorf("CA default drifted: %s", openbao.DefaultCAFile)
	}
	if openbao.DefaultClientCertFile != "/etc/openbao-client-tls/tls.crt" {
		t.Errorf("client cert default drifted: %s", openbao.DefaultClientCertFile)
	}
	if openbao.DefaultClientKeyFile != "/etc/openbao-client-tls/tls.key" {
		t.Errorf("client key default drifted: %s", openbao.DefaultClientKeyFile)
	}
}

func repoRootForMTLSTest(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "platform-apl")); err != nil {
		t.Skipf("repo root not found at %s: %v", root, err)
	}
	return root
}
