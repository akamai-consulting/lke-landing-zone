package main

// ci_mtls_wiring_guard.go implements `llz ci mtls-wiring-guard` — the gate on
// the correspondence between "this workload talks to OpenBao" and "this workload
// mounts the TLS material its code reads".
//
// WHY IT EXISTS. Deleting the reconciler's client-certificate volumeMount used
// to pass EVERY gate in this repo: kustomize still rendered, kubeconform was
// satisfied, `make lint-k8s` reported zero errors — and the pod could no longer
// reach OpenBao at all, because the listener requires a client certificate
// (ADR 0010). Verified by mutation, not assumed. The other guards check
// sync-wave health, schema validity and plaintext drift; nothing checked that a
// pod spec provides what its code path demands, which is exactly the kind of
// thing a refactor breaks silently and CI blesses.
//
// THE INVARIANT, inferred rather than registered: a pod that declares
// OPENBAO_ADDR is a pod that will call inClusterBaoHTTPClient(), and that
// function reads three files. So the pod must mount all three. There is no
// allowlist to maintain — adding a new OpenBao consumer automatically inherits
// the requirement, which is the property a registry-based version would lose.
//
// It also enforces two things that fall out of the same idea:
//
//   - Every TLS Secret a pod mounts must be CREATED by a Certificate in the same
//     namespace. A rename on one side of that pair is otherwise invisible until
//     the pod sits in ContainerCreating on a real cluster.
//   - OPENBAO_SKIP_VERIFY must not come back. ADR 0010 deleted it because its
//     failure mode was a silent downgrade to unverified TLS; a guard that only
//     checked for the presence of mounts would happily pass a pod that had the
//     mounts AND the escape hatch.

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/guardkit"
)

// The file paths inClusterBaoHTTPClient() reads when the corresponding env var
// is unset. Kept in sync with openbao_k8s_login.go by
// TestMTLSWiringDefaultsMatchClientCode, so a change to one side cannot drift
// from the other.
const (
	envOpenBaoAddr = "OPENBAO_ADDR"
	envCAFile      = "OPENBAO_CA_FILE"
	envClientCert  = "OPENBAO_CLIENT_CERT_FILE"
	envClientKey   = "OPENBAO_CLIENT_KEY_FILE"
	envSkipVerify  = "OPENBAO_SKIP_VERIFY"
)

// mtlsPodDoc is the minimal shape the guard needs from a workload manifest.
// Deployment/StatefulSet and CronJob nest the pod template differently, so both
// shapes are decoded and normalised by podTemplate().
type mtlsPodDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	Spec struct {
		// Deployment / StatefulSet
		Template mtlsPodTemplate `yaml:"template"`
		// CronJob
		JobTemplate struct {
			Spec struct {
				Template mtlsPodTemplate `yaml:"template"`
			} `yaml:"spec"`
		} `yaml:"jobTemplate"`
	} `yaml:"spec"`
}

type mtlsPodTemplate struct {
	Spec struct {
		Containers []struct {
			Name string `yaml:"name"`
			Env  []struct {
				Name  string `yaml:"name"`
				Value string `yaml:"value"`
			} `yaml:"env"`
			VolumeMounts []struct {
				Name      string `yaml:"name"`
				MountPath string `yaml:"mountPath"`
			} `yaml:"volumeMounts"`
		} `yaml:"containers"`
		Volumes []struct {
			Name   string `yaml:"name"`
			Secret struct {
				SecretName string `yaml:"secretName"`
			} `yaml:"secret"`
		} `yaml:"volumes"`
	} `yaml:"spec"`
}

// certDoc is a cert-manager Certificate, for the Secret-exists half.
type certDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	Spec struct {
		SecretName string `yaml:"secretName"`
	} `yaml:"spec"`
}

func (d mtlsPodDoc) podTemplate() (mtlsPodTemplate, bool) {
	switch d.Kind {
	case "Deployment", "StatefulSet", "DaemonSet":
		return d.Spec.Template, true
	case "CronJob":
		return d.Spec.JobTemplate.Spec.Template, true
	}
	return mtlsPodTemplate{}, false
}

func ciMTLSWiringGuardCmd() *cobra.Command {
	var root string
	cmd := &cobra.Command{
		Use:   "mtls-wiring-guard",
		Short: "fail when an OpenBao-consuming workload does not mount the mTLS material its code reads",
		Long: "Gate on the correspondence between a workload's code path and its pod spec\n" +
			"(docs/adr/0010-in-cluster-mtls.md). Any pod declaring OPENBAO_ADDR calls\n" +
			"inClusterBaoHTTPClient(), which reads a CA bundle and a client keypair — so\n" +
			"the pod must mount paths covering all three, every TLS Secret it mounts must\n" +
			"be created by a Certificate in the same namespace, and OPENBAO_SKIP_VERIFY\n" +
			"must not reappear.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIMTLSWiringGuard(root) },
	}
	cmd.Flags().StringVar(&root, "root", ".", "repo root (template or instance layout)")
	return cmd
}

type mtlsFinding struct {
	file, workload, problem string
}

func runCIMTLSWiringGuard(root string) error {
	dirs := platformTreeDirs(root)
	findings, examined, err := collectMTLSWiringFindings(dirs)
	if err != nil {
		return err
	}
	// A guard that walked nothing reports the same green as one that walked
	// everything — the sibling guards' shared contract.
	if err := guardkit.RequireCorpus("mtls-wiring-guard", examined, dirs); err != nil {
		return err
	}
	for _, f := range findings {
		fmt.Printf("::error file=%s::%s: %s\n", f.file, f.workload, f.problem)
	}
	if len(findings) > 0 {
		return fmt.Errorf("mtls-wiring-guard: %d problem(s) in the mTLS wiring of OpenBao-consuming workloads", len(findings))
	}
	fmt.Println("mtls-wiring-guard: every OpenBao consumer mounts its CA + client identity, and every mounted TLS Secret has a Certificate.")
	return nil
}

func collectMTLSWiringFindings(dirs []string) ([]mtlsFinding, int, error) {
	// Pass 1: every Secret a Certificate creates, keyed namespace/secretName.
	certSecrets := map[string]bool{}
	if _, err := walkManifests(dirs, func(_ string, raw []byte) error {
		for _, c := range decodeDocs(string(raw), func(c certDoc) bool { return c.Kind == "Certificate" }) {
			certSecrets[c.Metadata.Namespace+"/"+c.Spec.SecretName] = true
		}
		return nil
	}); err != nil {
		return nil, 0, err
	}

	var findings []mtlsFinding
	examined, err := walkManifests(dirs, func(path string, raw []byte) error {
		for _, d := range decodeDocs(string(raw), func(d mtlsPodDoc) bool { return d.Kind != "" }) {
			findings = append(findings, checkMTLSWiring(path, d, certSecrets)...)
		}
		return nil
	})
	if err != nil {
		return nil, examined, err
	}
	sortGuardFindings(findings, func(f mtlsFinding) (string, string) { return f.file, f.workload })
	return findings, examined, nil
}

// checkMTLSWiring is the pure check — one manifest doc in, findings out — so the
// invariant is unit-tested without a tree on disk.
func checkMTLSWiring(path string, d mtlsPodDoc, certSecrets map[string]bool) []mtlsFinding {
	tmpl, ok := d.podTemplate()
	if !ok {
		return nil
	}
	ns := d.Metadata.Namespace
	label := d.Kind + " " + ns + "/" + d.Metadata.Name
	var out []mtlsFinding

	// Volume name → Secret name, for the Certificate-exists check below.
	volSecret := map[string]string{}
	for _, v := range tmpl.Spec.Volumes {
		if v.Secret.SecretName != "" {
			volSecret[v.Name] = v.Secret.SecretName
		}
	}

	for _, c := range tmpl.Spec.Containers {
		env := map[string]string{}
		for _, e := range c.Env {
			env[e.Name] = e.Value
		}
		if _, talksToOpenBao := env[envOpenBaoAddr]; !talksToOpenBao {
			continue
		}
		if _, skip := env[envSkipVerify]; skip {
			out = append(out, mtlsFinding{path, label, fmt.Sprintf(
				"container %q sets %s. ADR 0010 deleted that flag: its failure mode was a SILENT downgrade to unverified TLS. The listener requires a client certificate, so the flag cannot even work — remove it",
				c.Name, envSkipVerify)})
		}
		mounts := make([]string, 0, len(c.VolumeMounts))
		mountVol := map[string]string{}
		for _, m := range c.VolumeMounts {
			mounts = append(mounts, m.MountPath)
			mountVol[m.MountPath] = m.Name
		}
		for _, want := range []struct{ envKey, def, why string }{
			{envCAFile, defaultOpenBaoCAFile, "verify OpenBao's serving certificate"},
			{envClientCert, defaultOpenBaoClientCert, "present this workload's own identity, which the listener REQUIRES"},
			{envClientKey, defaultOpenBaoClientKey, "present this workload's own identity, which the listener REQUIRES"},
		} {
			p := env[want.envKey]
			if p == "" {
				p = want.def
			}
			mp, covered := coveringMount(mounts, p)
			if !covered {
				out = append(out, mtlsFinding{path, label, fmt.Sprintf(
					"container %q declares %s but nothing mounts %s (needed to %s). inClusterBaoHTTPClient() reads that path; without it every OpenBao call fails the TLS handshake, and no other gate in this repo notices",
					c.Name, envOpenBaoAddr, p, want.why)})
				continue
			}
			// The mount exists — does the Secret behind it actually get created?
			if sec := volSecret[mountVol[mp]]; sec != "" && !certSecrets[ns+"/"+sec] {
				out = append(out, mtlsFinding{path, label, fmt.Sprintf(
					"container %q mounts Secret %q at %s, but no Certificate in namespace %q creates it. A rename on either side is invisible until the pod sits in ContainerCreating on a real cluster",
					c.Name, sec, mp, ns)})
			}
		}
	}
	return dedupeMTLSFindings(out)
}

// coveringMount reports the mountPath that would make file p present.
func coveringMount(mounts []string, p string) (string, bool) {
	best := ""
	for _, m := range mounts {
		mm := strings.TrimRight(m, "/")
		if p == mm || strings.HasPrefix(p, mm+"/") {
			if len(mm) > len(best) {
				best = mm
			}
		}
	}
	return best, best != ""
}

// dedupeMTLSFindings collapses the identical Secret-missing finding that the
// cert and key checks both produce (they share one mount).
func dedupeMTLSFindings(in []mtlsFinding) []mtlsFinding {
	seen := map[string]bool{}
	out := in[:0]
	for _, f := range in {
		k := f.file + "|" + f.workload + "|" + f.problem
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, f)
	}
	return out
}
