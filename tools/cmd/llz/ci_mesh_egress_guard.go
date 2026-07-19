package main

// ci_mesh_egress_guard.go implements `llz ci mesh-egress-guard` — the static
// guard extracted from the harbor-reconciler mesh-isolation failure.
//
// apl-core runs the platform app namespaces (harbor, …) under an Istio sidecar
// with STRICT mTLS: a plaintext request from a pod OUTSIDE that mesh to a
// Service inside it is rejected at the sidecar, regardless of NetworkPolicy.
// The batch-5 harbor reconciler learned this the expensive way — a pod in
// llz-reconciler (no sidecar) egressing to harbor-core.harbor.svc could never
// provision Harbor, so harbor stayed a CronJob IN the harbor namespace (in the
// mesh). Two ~50-minute e2e cycles to discover; the netpol :8080 fix was
// necessary-but-not-sufficient noise on the way.
//
// The guard makes that class a PR-time failure: any NetworkPolicy in the
// platform-bootstrap tree (platform-apl/manifest/ + platform-apl/components/)
// whose egress targets a KNOWN STRICT-mesh namespace — from a DIFFERENT namespace
// (i.e. a source pod that is not itself in that mesh) — is flagged. A workload
// that must talk to a mesh-STRICT Service belongs IN that namespace (a
// CronJob/Job/Deployment there, so it gets a sidecar), not in an outside one.
//
// meshStrictNamespaces starts with the one namespace proven STRICT (harbor) and
// is the single place to extend as others are confirmed. Same-namespace egress
// (harbor's own robot-provisioner CronJob → harbor-core) is fine and never
// flagged.

import (
	"fmt"

	"github.com/spf13/cobra"
)

// meshStrictNamespaces maps a namespace known to enforce Istio STRICT mTLS to the
// reason a non-mesh client can't reach it. Extend ONLY with namespaces actually
// confirmed STRICT — a false entry would flag a reachable target.
var meshStrictNamespaces = map[string]string{
	"harbor": "harbor-core runs behind an Istio sidecar with STRICT mTLS (apl-core). A plaintext request from a pod outside the harbor namespace is rejected at the sidecar — provisioning must run IN the harbor namespace (the harbor-robot-provisioner CronJob), not from llz-reconciler. See the batch-5 harbor-reconciler revert.",
}

// meDoc is the minimal NetworkPolicy shape the guard inspects.
type meDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	Spec struct {
		Egress []struct {
			To []struct {
				NamespaceSelector *struct {
					MatchLabels map[string]string `yaml:"matchLabels"`
				} `yaml:"namespaceSelector"`
			} `yaml:"to"`
		} `yaml:"egress"`
	} `yaml:"spec"`
}

// meFinding is one NetworkPolicy → mesh-strict-namespace egress violation.
type meFinding struct {
	file, policy, sourceNS, targetNS, reason string
}

func ciMeshEgressGuardCmd() *cobra.Command {
	var root string
	cmd := &cobra.Command{
		Use:   "mesh-egress-guard",
		Short: "fail when a NetworkPolicy egresses to a STRICT-mesh namespace from outside that mesh",
		Long: "Static guard for the harbor-reconciler mesh-isolation class: a pod outside an\n" +
			"Istio STRICT-mTLS namespace (e.g. harbor) cannot reach a Service inside it, so a\n" +
			"NetworkPolicy that egresses there from a different namespace describes traffic that\n" +
			"will be silently dropped at the sidecar. Run the client IN that namespace instead.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIMeshEgressGuard(root) },
	}
	cmd.Flags().StringVar(&root, "root", ".", "repo root (template or instance layout)")
	return cmd
}

func runCIMeshEgressGuard(root string) error {
	dirs := platformTreeDirs(root)
	findings, examined, err := collectMeshEgressFindings(dirs)
	if err != nil {
		return err
	}
	if err := requireCorpus("mesh-egress-guard", examined, dirs); err != nil {
		return err
	}
	if len(findings) == 0 {
		fmt.Println("mesh-egress-guard: no NetworkPolicy egresses to a STRICT-mesh namespace from outside it.")
		return nil
	}
	for _, f := range findings {
		fmt.Printf("::error file=%s::NetworkPolicy %q in namespace %q egresses to the %q namespace, which enforces Istio STRICT mTLS — a pod outside that mesh cannot reach it (traffic is dropped at the sidecar, not by NetworkPolicy). %s\n",
			f.file, f.policy, f.sourceNS, f.targetNS, f.reason)
	}
	return fmt.Errorf("mesh-egress-guard: %d cross-mesh egress rule(s) to a STRICT-mesh namespace", len(findings))
}

// collectMeshEgressFindings walks the dirs and flags every NetworkPolicy egress
// whose namespaceSelector targets a meshStrictNamespaces entry from a different
// source namespace.
func collectMeshEgressFindings(dirs []string) (findings []meFinding, examined int, err error) {
	examined, err = walkManifests(dirs, func(path string, raw []byte) error {
		for _, doc := range decodeDocs(string(raw), func(d meDoc) bool { return d.Kind == "NetworkPolicy" }) {
			for _, e := range doc.Spec.Egress {
				for _, to := range e.To {
					if to.NamespaceSelector == nil {
						continue
					}
					target := to.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"]
					reason, strict := meshStrictNamespaces[target]
					// Same-namespace egress (the mesh workload's own netpol) is
					// in-mesh and fine; only a DIFFERENT source namespace is a
					// cross-mesh reach.
					if strict && target != doc.Metadata.Namespace {
						findings = append(findings, meFinding{
							file: path, policy: doc.Metadata.Name,
							sourceNS: doc.Metadata.Namespace, targetNS: target, reason: reason,
						})
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, examined, err
	}
	sortGuardFindings(findings, func(f meFinding) (string, string) { return f.file, f.targetNS })
	return findings, examined, nil
}
