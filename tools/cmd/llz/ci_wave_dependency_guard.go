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
// platform-bootstrap tree (platform-apl/manifest/ + platform-apl/components/)
// that hard-references a Secret which an ExternalSecret in the same tree+namespace
// produces, the workload's sync-wave MUST be strictly greater than that
// ExternalSecret's. Optional references (optional: true) don't block pod start,
// so they're ignored.
//
// CROSS-APPLICATION AWARENESS (blast-radius decomposition). A component carved into
// its own Argo CD Application (clusterspec CarvedApp) no longer shares
// platform-bootstrap's single wave sequence: Argo cannot sync a carved App's content
// before the app-of-apps CREATES that App at its App-level wave, and sibling Apps
// sync independently (no cross-App health gate). So a workload in one App whose Secret
// is produced by an ExternalSecret in a LATER-created App is the same wedge in a new
// dress — the ES may not exist yet when the workload's App starts.
//
// The guard therefore judges ordering by TOPOLOGY, keying off the SAME registry
// `llz render` emits the Apps from (so the two can't drift):
//   - workload and ExternalSecret in the SAME Application → compare RESOURCE
//     sync-waves (Argo orders one App's resources by them); the existing rule.
//   - in DIFFERENT Applications → compare APP-LEVEL waves; the workload's App must be
//     created strictly after the ExternalSecret's App.
// Note it is NOT a single max()-of-the-two "effective wave": flooring both a
// workload and its ExternalSecret at a common App wave would MASK a genuine
// intra-App inversion (a wave-0 workload + wave-5 ES in the same App is still a
// wedge). The platform-bootstrap root tree ranks earliest (wdRootAppWave), since
// Terraform creates it before any carved child App.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
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

// wdInversion is one workload → ExternalSecret ordering violation. workloadWave/esWave
// are the waves that were actually COMPARED: resource sync-waves when both live in the
// same Application, App-level waves when they live in different ones. workloadApp/esApp
// name the owning Applications for the diagnostic.
type wdInversion struct {
	file, workload, secret, esFile string
	workloadWave, esWave           int
	workloadApp, esApp             string
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
	dirs := platformTreeDirs(root)
	inversions, examined, err := collectWaveDependencyInversions(dirs)
	if err != nil {
		return err
	}
	if err := requireCorpus("wave-dependency-guard", examined, dirs); err != nil {
		return err
	}
	if len(inversions) == 0 {
		fmt.Println("wave-dependency-guard: every workload syncs after the ExternalSecret(s) it hard-depends on.")
		return nil
	}
	for _, v := range inversions {
		cross := ""
		if v.workloadApp != v.esApp {
			cross = fmt.Sprintf(" The workload syncs under Application %q and the ExternalSecret under %q — a carved App cannot sync before the app-of-apps creates it, so give %q a strictly higher App-level sync-wave than %q (clusterspec CarvedApp.AppWave), or move them into the same App.", v.workloadApp, v.esApp, v.workloadApp, v.esApp)
		}
		fmt.Printf("::error file=%s::workload %q at effective sync-wave %d hard-references Secret %q, but the ExternalSecret that produces it (%s) is at effective sync-wave %d. A workload that gates a wave before its Secret's ExternalSecret can never go Healthy — it wedges the sync and starves every later-wave ExternalSecret (the #163 failure class). Give the workload a sync-wave > %d, or mark the reference optional: true.%s\n",
			v.file, v.workload, v.workloadWave, v.secret, v.esFile, v.esWave, v.esWave, cross)
	}
	return fmt.Errorf("wave-dependency-guard: %d workload/ExternalSecret wave inversion(s)", len(inversions))
}

// collectWaveDependencyInversions walks the dirs, indexes ExternalSecrets by the
// Secret they produce (namespace/name → waves), then flags each workload whose
// non-optional Secret reference resolves to an ExternalSecret that syncs equal-or-later.
// "Later" is judged per the app-of-apps topology: within one Application by RESOURCE
// sync-wave (Argo orders a single App's resources by it); across Applications by the
// App-LEVEL wave (a carved App's content cannot sync before the app-of-apps creates
// the App, and sibling Apps sync with no cross-App health gate). See the file header.
func collectWaveDependencyInversions(dirs []string) (_ []wdInversion, examined int, err error) {
	type res struct {
		file, app        string
		resWave, appWave int
	}
	type esInfo struct {
		res
	}
	esBySecret := map[string]esInfo{} // "namespace/secretName" → info
	type workload struct {
		res
		name, namespace string
		refs            []wdSecretRef
	}
	var workloads []workload

	examined, err = walkManifests(dirs, func(path string, raw []byte) error {
		app, appWave := wdOwningApp(path) // same for every doc in a file
		for _, doc := range splitWaveDependencyDocs(string(raw)) {
			r := res{file: path, app: app, resWave: wdSyncWave(doc.Metadata.Annotations), appWave: appWave}
			ns := doc.Metadata.Namespace
			switch {
			case doc.Kind == "ExternalSecret":
				name := doc.Metadata.Name
				if doc.Spec.Target != nil && doc.Spec.Target.Name != "" {
					name = doc.Spec.Target.Name
				}
				esBySecret[ns+"/"+name] = esInfo{res: r}
			case wdWorkloadKinds[doc.Kind] && doc.Spec.Template != nil:
				refs := wdWorkloadSecretRefs(doc.Spec.Template.Spec.Containers,
					doc.Spec.Template.Spec.InitContainers, doc.Spec.Template.Spec.Volumes)
				r.file = path
				workloads = append(workloads, workload{
					res: r, name: doc.Kind + "/" + doc.Metadata.Name, namespace: ns, refs: refs,
				})
			}
		}
		return nil
	})
	if err != nil {
		return nil, examined, err
	}

	var inversions []wdInversion
	for _, w := range workloads {
		for _, ref := range w.refs {
			if ref.optional {
				continue
			}
			es, ok := esBySecret[w.namespace+"/"+ref.name]
			if !ok {
				continue
			}
			// Same Application: Argo orders its resources by RESOURCE wave, so the
			// workload must sync strictly after (equal is fine — same wave applies
			// together). Different Applications: compare APP-level waves — the
			// workload's App must be created strictly after the ES's App so the ES
			// App gets a head start (equal App waves race, which is a wedge risk).
			var inverted bool
			var wWave, eWave int
			if w.app == es.app {
				inverted, wWave, eWave = es.resWave > w.resWave, w.resWave, es.resWave
			} else {
				inverted, wWave, eWave = es.appWave >= w.appWave, w.appWave, es.appWave
			}
			if !inverted {
				continue
			}
			inversions = append(inversions, wdInversion{
				file: w.file, workload: w.name, secret: ref.name,
				esFile: es.file, workloadWave: wWave, esWave: eWave,
				workloadApp: w.app, esApp: es.app,
			})
		}
	}
	sort.Slice(inversions, func(i, j int) bool {
		if inversions[i].file != inversions[j].file {
			return inversions[i].file < inversions[j].file
		}
		return inversions[i].secret < inversions[j].secret
	})
	return inversions, examined, nil
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

// wdRootAppWave is the App-level wave assigned to the platform-bootstrap root tree
// (the shared base + every non-carved component). platform-bootstrap and its
// llz-secret-store sibling are created by Terraform BEFORE the app-of-apps spawns
// any carved child App, so the root ranks earliest — a sentinel below any real
// sync-wave — for cross-App comparisons: a root workload depending on a carved App's
// ExternalSecret is correctly flagged (root came up first), while a carved workload
// depending on a root ExternalSecret is fine.
const wdRootAppWave = -1 << 30

// wdOwningApp returns the Argo CD Application a manifest path syncs under and that
// App's App-level wave: a carved component's App (clusterspec CarvedApp) when the
// path lives under that component's dir, else the platform-bootstrap root tree.
func wdOwningApp(path string) (string, int) {
	if c, ok := wdComponentOf(path); ok && c.CarvedApp != nil {
		return c.CarvedApp.AppName, c.CarvedApp.AppWave
	}
	return "platform-bootstrap", wdRootAppWave
}

// wdComponentOf resolves the clusterspec component a manifest path belongs to by its
// platform-apl/components/<name>/ segment. ok=false for the shared base / any path not
// under a component dir.
func wdComponentOf(path string) (clusterspec.Component, bool) {
	const marker = "/components/"
	i := strings.LastIndex(path, marker)
	if i < 0 {
		return clusterspec.Component{}, false
	}
	rest := path[i+len(marker):]
	name := rest
	if j := strings.IndexByte(rest, '/'); j >= 0 {
		name = rest[:j]
	}
	return clusterspec.LookupComponent(name)
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
