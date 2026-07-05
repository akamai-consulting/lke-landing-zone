package main

// ci_wave_dependency_guard.go implements `llz ci wave-dependency-guard` — the
// static guard extracted from the #163 converge wedge.
//
// Sibling to wave-health-guard, but the inverse failure: not "a resource at a
// NEGATIVE wave is health-inert?" but "a workload never gates a wave BEFORE the
// ExternalSecret that feeds it." Argo CD gates sync waves on per-resource
// health, so a Deployment/StatefulSet/DaemonSet at wave W whose pod hard-depends
// on a Secret (a non-optional secretKeyRef / envFrom.secretRef / secret volume)
// produced by an ExternalSecret at wave E > W can NEVER go Healthy at wave W —
// its Secret is not created until wave E, which the sync never reaches because
// the unhealthy workload blocks every wave past W. The whole platform-bootstrap
// manifest sync stalls, and EVERY other wave-E ExternalSecret in that sync
// starves too (in #163 the reconciler Deployment sat at the default wave 0 with
// a wave-5 secretKeyRef and took harbor-registry-s3 + loki-object-store down
// with it — a ~40-minute e2e run to discover on a real cluster).
//
// The guard makes that class a PR-time failure: for every workload in the
// platform-bootstrap tree (apl-values/_shared/manifest/ + apl-values/components/)
// that hard-references a Secret which an ExternalSecret in the same tree+namespace
// produces, the workload's sync-wave MUST be strictly greater than that
// ExternalSecret's. Optional references (optional: true) don't block pod start,
// so they're ignored.

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// wdSecretRef is one Secret reference by a container/volume, with its optionality.
type wdSecretRef struct {
	name     string
	optional bool
}

// wdContainer captures the Secret-consuming fields of a (init)container.
type wdContainer struct {
	Env []struct {
		ValueFrom *struct {
			SecretKeyRef *struct {
				Name     string `yaml:"name"`
				Optional *bool  `yaml:"optional"`
			} `yaml:"secretKeyRef"`
		} `yaml:"valueFrom"`
	} `yaml:"env"`
	EnvFrom []struct {
		SecretRef *struct {
			Name     string `yaml:"name"`
			Optional *bool  `yaml:"optional"`
		} `yaml:"secretRef"`
	} `yaml:"envFrom"`
}

// wdVolume captures a secret-backed volume.
type wdVolume struct {
	Secret *struct {
		SecretName string `yaml:"secretName"`
		Optional   *bool  `yaml:"optional"`
	} `yaml:"secret"`
}

// wdDoc is the minimal YAML shape the guard inspects — a union over the fields
// of a workload (spec.template) and an ExternalSecret (spec.target).
type wdDoc struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name        string            `yaml:"name"`
		Namespace   string            `yaml:"namespace"`
		Annotations map[string]string `yaml:"annotations"`
	} `yaml:"metadata"`
	Spec struct {
		Target *struct {
			Name string `yaml:"name"`
		} `yaml:"target"`
		Template *struct {
			Spec struct {
				Containers     []wdContainer `yaml:"containers"`
				InitContainers []wdContainer `yaml:"initContainers"`
				Volumes        []wdVolume    `yaml:"volumes"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

// wdWorkloadKinds are the health-gated workload kinds Argo waits on per wave. A
// CronJob is excluded (Argo assesses no CronJob health; see wave-health-guard).
var wdWorkloadKinds = map[string]bool{"Deployment": true, "StatefulSet": true, "DaemonSet": true}

// wdInversion is one workload → ExternalSecret ordering violation.
type wdInversion struct {
	file, workload, secret, esFile string
	workloadWave, esWave           int
}

func ciWaveDependencyGuardCmd() *cobra.Command {
	var root string
	cmd := &cobra.Command{
		Use:   "wave-dependency-guard",
		Short: "fail when a workload syncs at or before the ExternalSecret that provides a Secret it hard-depends on",
		Long: "Static guard for the #163 wedge class: Argo sync waves gate on per-resource\n" +
			"health, so a Deployment/StatefulSet/DaemonSet that hard-references (non-optional)\n" +
			"a Secret produced by an ExternalSecret at a LATER sync-wave can never go Healthy\n" +
			"— it blocks its wave forever and starves every later-wave ExternalSecret in the\n" +
			"platform-bootstrap sync. The workload's wave must be strictly greater than the\n" +
			"ExternalSecret's. Mark the reference optional: true to opt out (the pod then\n" +
			"starts without the Secret).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIWaveDependencyGuard(root) },
	}
	cmd.Flags().StringVar(&root, "root", ".", "repo root (template or instance layout)")
	return cmd
}

func runCIWaveDependencyGuard(root string) error {
	aplDir := esRepoPath(root, "apl-values")
	dirs := []string{filepath.Join(aplDir, "_shared", "manifest"), filepath.Join(aplDir, "components")}
	inversions, err := collectWaveDependencyInversions(dirs)
	if err != nil {
		return err
	}
	if len(inversions) == 0 {
		fmt.Println("wave-dependency-guard: every workload syncs after the ExternalSecret(s) it hard-depends on.")
		return nil
	}
	for _, v := range inversions {
		fmt.Printf("::error file=%s::workload %q at sync-wave %d hard-references Secret %q, but the ExternalSecret that produces it (%s) is at sync-wave %d. A workload that gates a wave before its Secret's ExternalSecret can never go Healthy — it wedges the platform-bootstrap sync and starves every later-wave ExternalSecret (the #163 failure class). Give the workload a sync-wave > %d, or mark the reference optional: true.\n",
			v.file, v.workload, v.workloadWave, v.secret, v.esFile, v.esWave, v.esWave)
	}
	return fmt.Errorf("wave-dependency-guard: %d workload/ExternalSecret wave inversion(s)", len(inversions))
}

// collectWaveDependencyInversions walks the dirs, indexes ExternalSecrets by the
// Secret they produce (namespace/name → wave), then flags each workload whose
// non-optional Secret reference resolves to an ExternalSecret at an equal-or-later
// wave.
func collectWaveDependencyInversions(dirs []string) ([]wdInversion, error) {
	type esInfo struct {
		wave int
		file string
	}
	esBySecret := map[string]esInfo{} // "namespace/secretName" → info
	type workload struct {
		file, name, namespace string
		wave                  int
		refs                  []wdSecretRef
	}
	var workloads []workload

	for _, dir := range dirs {
		if _, statErr := os.Stat(dir); os.IsNotExist(statErr) {
			continue // a layout without this dir (e.g. no _shared overlay) — nothing to scan
		}
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".yaml") {
				return err
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, doc := range splitWaveDependencyDocs(string(raw)) {
				wave := wdSyncWave(doc.Metadata.Annotations)
				ns := doc.Metadata.Namespace
				switch {
				case doc.Kind == "ExternalSecret":
					name := doc.Metadata.Name
					if doc.Spec.Target != nil && doc.Spec.Target.Name != "" {
						name = doc.Spec.Target.Name
					}
					esBySecret[ns+"/"+name] = esInfo{wave: wave, file: path}
				case wdWorkloadKinds[doc.Kind] && doc.Spec.Template != nil:
					refs := wdWorkloadSecretRefs(doc.Spec.Template.Spec.Containers,
						doc.Spec.Template.Spec.InitContainers, doc.Spec.Template.Spec.Volumes)
					workloads = append(workloads, workload{
						file: path, name: doc.Kind + "/" + doc.Metadata.Name, namespace: ns, wave: wave, refs: refs,
					})
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	var inversions []wdInversion
	for _, w := range workloads {
		for _, ref := range w.refs {
			if ref.optional {
				continue
			}
			es, ok := esBySecret[w.namespace+"/"+ref.name]
			if !ok || es.wave <= w.wave {
				continue
			}
			inversions = append(inversions, wdInversion{
				file: w.file, workload: w.name, secret: ref.name,
				esFile: es.file, workloadWave: w.wave, esWave: es.wave,
			})
		}
	}
	sort.Slice(inversions, func(i, j int) bool {
		if inversions[i].file != inversions[j].file {
			return inversions[i].file < inversions[j].file
		}
		return inversions[i].secret < inversions[j].secret
	})
	return inversions, nil
}

// wdWorkloadSecretRefs gathers every Secret reference (env secretKeyRef, envFrom
// secretRef, secret volume) across a pod template's (init)containers + volumes.
func wdWorkloadSecretRefs(containers, initContainers []wdContainer, volumes []wdVolume) []wdSecretRef {
	var refs []wdSecretRef
	for _, c := range append(append([]wdContainer{}, containers...), initContainers...) {
		for _, e := range c.Env {
			if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil && e.ValueFrom.SecretKeyRef.Name != "" {
				refs = append(refs, wdSecretRef{e.ValueFrom.SecretKeyRef.Name, boolVal(e.ValueFrom.SecretKeyRef.Optional)})
			}
		}
		for _, ef := range c.EnvFrom {
			if ef.SecretRef != nil && ef.SecretRef.Name != "" {
				refs = append(refs, wdSecretRef{ef.SecretRef.Name, boolVal(ef.SecretRef.Optional)})
			}
		}
	}
	for _, v := range volumes {
		if v.Secret != nil && v.Secret.SecretName != "" {
			refs = append(refs, wdSecretRef{v.Secret.SecretName, boolVal(v.Secret.Optional)})
		}
	}
	return refs
}

// wdSyncWave reads the argocd sync-wave annotation, defaulting to 0 (Argo's
// default) when absent or unparseable.
func wdSyncWave(ann map[string]string) int {
	w, err := strconv.Atoi(strings.TrimSpace(ann["argocd.argoproj.io/sync-wave"]))
	if err != nil {
		return 0
	}
	return w
}

// splitWaveDependencyDocs parses a multi-doc YAML file, skipping docs that fail
// to parse (kustomize patches, comments-only files).
func splitWaveDependencyDocs(raw string) []wdDoc {
	var docs []wdDoc
	dec := yaml.NewDecoder(strings.NewReader(raw))
	for {
		var d wdDoc
		if err := dec.Decode(&d); err != nil {
			break
		}
		if d.Kind != "" {
			docs = append(docs, d)
		}
	}
	return docs
}

func boolVal(p *bool) bool { return p != nil && *p }
