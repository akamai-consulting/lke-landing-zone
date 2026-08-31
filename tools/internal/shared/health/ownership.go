package health

// ownership.go — THE PLATFORM/INSTANCE BOUNDARY, resolved per RESOURCE.
//
// The convergence contract gates the PLATFORM. Instance-owned content — the
// operator escape hatch, and the apps an instance deploys through it — is
// reported but must not pin the platform verdict, because a platform release
// being blocked by an app team's missing credential is the wrong coupling.
//
// The Application-level classifier has drawn that line since the hatch
// shipped. What it could not do is follow the line DOWN: an instance-owned
// Application's ExternalSecrets, Deployments, Workflows and Pods are checked by
// their own sections, which knew nothing about who owns them and so gated on all
// of it. Measured on akamai/gsap-apl: nine instance-owned Applications correctly
// reported non-gating, while thirteen of their ExternalSecrets, four Workflows,
// two Deployments and eighteen Pods failed the platform gate — every one of them
// downstream of eight per-app PATs an operator had never seeded.
//
// WHERE THE ANSWER COMES FROM. Not a namespace list, and not a name convention:
// instance-custom-istio-system deploys INTO istio-system, so namespace cannot
// separate an operator's oauth2-proxy from the platform's own istio workloads.
// The authority is Argo's own `.status.resources` — the exact (group, kind,
// namespace, name) set each Application manages, which the app fetch already
// parses for the Drifted field. Ownership is therefore DERIVED from the cluster,
// never guessed.
//
// FAILS CLOSED, IN THREE PLACES. A resource no instance-owned Application claims
// is platform, and gates. A resource a PLATFORM Application also claims is
// platform, and gates, however many instance apps declare it — see the contested
// pass in NewOwnershipIndex. And a pod resolves to its workload only through a
// name shape Kubernetes actually generates; anything else is unowned.

import (
	"sort"
	"strconv"
	"strings"
)

// InstanceCustomProject is the AppProject every instance-owned Application is
// scoped to — the operator escape hatch's project (platform-apl/manifest/
// instance-custom-project.yaml), and the project the instance's own generators
// stamp onto the Applications they produce.
//
// THIS, NOT THE NAME, IS THE BOUNDARY. IsInstanceCustomApp keyed on the
// `instance-custom-<ns>` name prefix the escape-hatch ApplicationSet happens to
// produce. An instance that generates Applications any other way — the
// managed-apps ApplicationSet names them after the app (`dispatch`,
// `account-health`) — declares `project: instance-custom` and got classified as
// platform anyway, because only the name was consulted. The project is what the
// AppProject actually scopes, so it is what ownership reads.
//
// TestInstanceCustomProjectMatchesTheShippedAppProject reads the delivered
// manifest and asserts this string against it, so renaming the AppProject cannot
// silently return every instance-owned app to the platform gate.
const InstanceCustomProject = "instance-custom"

// instanceCustomAppPrefix is the escape-hatch ApplicationSet's naming convention,
// derived from the project rather than spelled a second time.
const instanceCustomAppPrefix = InstanceCustomProject + "-"

// ResourceRef identifies one resource an Application manages, as Argo reports it
// in `.status.resources`.
//
// GROUP IS PART OF THE IDENTITY. Kind alone is not unique — `Certificate` is
// cert-manager's and also several other CRDs' — and the checks that consult this
// index pass exactly those collision-prone kinds. Argo already reports the group;
// dropping it would let an operator CRD named Certificate demote the platform's
// TLS certificate of the same name in the same namespace.
type ResourceRef struct {
	Group     string
	Kind      string
	Namespace string
	Name      string
}

// OwnershipIndex is the set of resources instance-owned Applications manage.
// The zero value is usable and owns nothing — which is the fail-closed default:
// an index that could not be built classifies everything as platform.
type OwnershipIndex struct {
	owned map[ResourceRef]bool
	// generated maps a controller-generated child to the resource an Application
	// actually declares, for chains the ownerReferences alone cannot close: a
	// CronWorkflow's Workflow carries a generated name, and its PODS resolve only
	// as far as that generated Workflow. One hop was not enough.
	generated map[ResourceRef]ResourceRef
	// statefulSets is the owned StatefulSet names per namespace. A
	// volumeClaimTemplate PVC carries NO ownerReferences under the default Retain
	// retention policy, so the only link back to the declared StatefulSet is the
	// name Kubernetes generates from it.
	statefulSets map[string][]string
	// platformDeclared is every resource a PLATFORM Application declares. The
	// contested pass uses it to refuse a claim; Owns keeps it afterwards because
	// the namespace inference below needs the same veto per resource.
	platformDeclared map[ResourceRef]bool
	// platformNamespaces are the namespaces the caller declares the platform
	// occupies. They are not off-limits — instance-custom-istio-system deploys into
	// istio-system by design — but they are where a wrong answer costs the most, so
	// they are the blast radius the guards below are scoped to.
	platformNamespaces map[string]bool
	// platformOccupied is every namespace a PLATFORM Application actually has a
	// resource in, read from the cluster rather than from a list.
	//
	// THE CALLER'S LIST IS NOT THE PLATFORM. converge's healthNamespaces is nine
	// entries — the namespaces it scans per-namespace — while apl-core's platform
	// runs in monitoring, keycloak, kyverno, cnpg-system and more, all of which the
	// -A sections judge. Deriving instanceNamespaces from the caller's list alone
	// meant ONE instance-owned resource in monitoring re-labelled the whole
	// namespace as app estate, and loki's volumeClaimTemplate PVCs — generated, so
	// declared by no Application and unprotected by platformDeclared — would have
	// been demoted out of the platform gate. Fail-open, in the guard that exists to
	// stop the widening failing open.
	platformOccupied map[string]bool
	// instanceNamespaces are the namespaces instance-owned Applications declare
	// into that the platform does NOT occupy — team-gsap and its like.
	//
	// THEY EXIST BECAUSE THE SCAN NOW REACHES THEM. converge widens its
	// per-namespace sections (Deployments, StatefulSets, DaemonSets, Services) to
	// cover the app estate so `--scope=apps` can see it. Those namespaces were
	// previously examined by nobody, so without this every resource in them that
	// no Application happens to declare — an operator's operand, a hand-applied
	// manifest, anything a controller creates — would arrive in the report as
	// PLATFORM and hard-fail the platform gate. That is a new way for app content
	// to block a platform release, created by the same change that exists to stop
	// app content blocking a platform release.
	//
	// The veto is per resource, not per namespace: a platform Application that
	// declares something here keeps it (see platformDeclared), and the inference
	// is switched off entirely while any platform Application is unresolved, since
	// then there is no evidence to veto WITH.
	instanceNamespaces map[string]bool
	// platformUnresolved names PLATFORM Applications Argo has NOT COMPARED and
	// that therefore declare no resources. Such an app's resources are missing
	// from the contested pass and an instance claim on one would win. See
	// platformMayClaim for the blast radius that opens.
	//
	// ZERO RESOURCES IS NOT ENOUGH ON ITS OWN — it conflates two opposite states,
	// and reading it alone made this veto permanent on akamai/gsap-apl. An app
	// Argo has not compared publishes nothing because there is no answer yet;
	// a COMPARED app that publishes nothing is telling you the answer, and it is
	// "I own nothing". Four apl-core gitops shells (gitops-global,
	// istio-system-istio-artifacts, team-admin-values-gitops,
	// team-platform-values-gitops) are Synced/Healthy with an empty
	// .status.resources on every poll forever — global/team values with no
	// rendered manifests. Vetoing on them is not "wait for evidence", it is
	// waiting for evidence that has already arrived, so the boundary could never
	// demote anything in a platform namespace on that instance. argoCompared is
	// the discriminator; it fails closed on any value it does not recognise.
	platformUnresolved []string
	// platformUnresolvedNS are the destination namespaces of those Applications —
	// the only evidence available about where the resources they did not publish
	// would have landed.
	platformUnresolvedNS map[string]bool
	// platformUnresolvedAnywhere is set when one of them names no destination at
	// all, so nothing bounds it and the veto goes back to covering every platform
	// namespace.
	platformUnresolvedAnywhere bool
	// instanceDeclaredNS is every namespace an instance-owned Application declares
	// into, BEFORE the platform exclusions instanceNamespaces applies. Kept
	// separately because misprojectedApps has to reason about the namespaces the
	// app estate occupies, and a misprojected Application is precisely one that
	// removes its own namespace from instanceNamespaces.
	instanceDeclaredNS map[string]bool
	// platformAppNS maps each PLATFORM Application to the namespaces it declares
	// into, for the same reason.
	platformAppNS map[string][]string
	// misprojected names Applications that look like the instance's own — every
	// resource in a namespace the app estate occupies and the platform does not —
	// but are NOT scoped to the instance-custom AppProject, so the boundary reads
	// them as platform and they gate.
	//
	// THIS IS THE RESIDUAL COUPLING, MADE VISIBLE. Ownership keys on
	// `.spec.project`, so an instance that generates Applications without setting
	// it — a new ApplicationSet left on `default`, a copy-pasted manifest — puts
	// its apps straight back on the platform gate, silently, and the report reads
	// exactly as it did before this boundary existed. Worse, such an Application
	// marks its own namespace platformOccupied, which switches OFF the namespace
	// inference for the whole app estate around it. Naming them is not a gate; it
	// is the one line that turns "why is my app failing the platform" into a
	// one-word fix.
	misprojected []string
	// contested counts resources an instance-owned Application declared that a
	// PLATFORM Application declares too. They stay platform; the count exists so
	// the report can say the boundary was asked to move something it refused to.
	contested int
	// unresolved names instance-owned Applications Argo has NOT COMPARED — such an
	// Application publishes an empty .status.resources, so its children cannot be
	// attributed to it THIS poll and gate the platform as if they were platform's.
	// Reported so a converge that hard-fails on one poll and passes on the next is
	// explicable.
	//
	// SAME DISCRIMINATOR AS platformUnresolved, and for the same reason. A
	// COMPARED instance app with an empty .status.resources owns nothing, so
	// nothing of its is gating and there is nothing to explain — naming it would
	// tell a reader to go looking for content that does not exist. Only the
	// genuinely uncompared case is transient, which is what the report's "on this
	// poll" claims about it.
	unresolved []string
}

// argoCompared reports whether Argo has finished a comparison for this
// Application — i.e. whether its .status.resources is an ANSWER rather than an
// absence of one.
//
// THIS IS THE WHOLE FIX. Reading `len(a.Resources) == 0` alone conflates "Argo
// has not looked yet" with "Argo looked and there is nothing here", and only the
// first justifies a veto. Argo's SyncStatusCode has exactly three values:
// "Synced" and "OutOfSync" are comparison VERDICTS; "Unknown" is what Argo
// publishes while it has not compared — the state argo.go's own worked example
// documents (`gitops-global (Unknown/Healthy) — ComparisonError: failed to list
// refs`), and the state a ComparisonError leaves an app in.
//
// EVERYTHING ELSE IS "NOT COMPARED", deliberately. An empty string (no
// .status.sync at all, a brand-new Application, a fetch that could not parse the
// field) and any value a future Argo adds both land in the default arm and keep
// the veto. Enumerating the verdicts rather than excluding "Unknown" is what
// makes the unknown-unknowns fail closed.
func argoCompared(a ArgoApp) bool {
	switch a.Sync {
	case "Synced", "OutOfSync":
		return true
	default:
		return false
	}
}

// NewOwnershipIndex builds the index from every instance-owned Application's
// declared resources, then removes anything a PLATFORM Application also declares.
//
// THE CONTESTED PASS IS LOAD-BEARING. The instance-custom AppProject is
// deliberately permissive — `namespace: '*'` with `group: '*', kind: '*'` on both
// whitelists — so an operator manifest CAN name a platform resource, by patch, by
// copy-paste, or by a chart that renders the same name into a shared namespace.
// Without this pass, declaring `Deployment/istio-system/istiod` in
// kubernetes-custom/ would silently remove the platform's own istiod from the
// convergence gate. Ownership can only ever REMOVE a resource from the platform
// gate that nothing platform-owned claims.
func NewOwnershipIndex(apps []ArgoApp) OwnershipIndex {
	idx := OwnershipIndex{
		owned:                make(map[ResourceRef]bool),
		statefulSets:         map[string][]string{},
		generated:            map[ResourceRef]ResourceRef{},
		platformDeclared:     make(map[ResourceRef]bool),
		platformOccupied:     map[string]bool{},
		platformUnresolvedNS: map[string]bool{},
		instanceDeclaredNS:   map[string]bool{},
		platformAppNS:        map[string][]string{},
	}
	platform := idx.platformDeclared
	for _, a := range apps {
		if !IsInstanceOwnedApp(a) {
			// A platform Application Argo has not COMPARED cannot defend anything.
			// The instance side already refuses to claim in this state; the platform
			// side has to record it, because the resources it would have protected
			// are exactly the ones an instance claim could now take. Its DESTINATION
			// bounds where those resources could be — an app with no destination
			// bounds nothing, which is the fail-closed reading.
			//
			// BOTH HALVES ARE REQUIRED. Zero resources on its own is not evidence of
			// anything: a compared app that declares nothing has ANSWERED, and
			// vetoing on its answer is what made this guard permanent rather than
			// transient on akamai/gsap-apl. See argoCompared and platformUnresolved.
			if len(a.Resources) == 0 && !argoCompared(a) {
				idx.platformUnresolved = append(idx.platformUnresolved, a.Name)
				if a.DestNamespace == "" {
					idx.platformUnresolvedAnywhere = true
				} else {
					idx.platformUnresolvedNS[a.DestNamespace] = true
				}
			}
			if a.DestNamespace != "" {
				idx.platformOccupied[a.DestNamespace] = true
			}
			for _, res := range a.Resources {
				platform[res] = true
				if res.Namespace != "" {
					idx.platformOccupied[res.Namespace] = true
					idx.platformAppNS[a.Name] = append(idx.platformAppNS[a.Name], res.Namespace)
				}
			}
			continue
		}
		if len(a.Resources) == 0 {
			// Same discriminator as the platform arm. A compared instance app that
			// declares nothing owns nothing — reporting it as "still gating the
			// platform" points a reader at content that does not exist.
			if !argoCompared(a) {
				idx.unresolved = append(idx.unresolved, a.Name)
			}
			continue
		}
		for _, res := range a.Resources {
			idx.owned[res] = true
			if res.Namespace != "" {
				idx.instanceDeclaredNS[res.Namespace] = true
			}
		}
	}
	for ref := range platform {
		if idx.owned[ref] {
			delete(idx.owned, ref)
			idx.contested++
		}
	}
	for ref := range idx.owned {
		if ref.Kind == "StatefulSet" {
			idx.statefulSets[ref.Namespace] = append(idx.statefulSets[ref.Namespace], ref.Name)
		}
	}
	return idx
}

// platformReservedNamespaces is where the contested pass is structurally blind.
//
// THE PASS ONLY PROTECTS WHAT ARGO DECLARES. LKE ships cilium, coredns,
// csi-linode-node and konnectivity-agent into kube-system with no Application at
// all, and checkWorkloads judges that namespace — so an operator manifest naming
// `DaemonSet/kube-system/csi-linode-node` would claim it uncontested and take a
// dead CNI out of the platform gate. Nothing an instance owns belongs in
// kube-system, so the whole namespace is reserved rather than guessing a list of
// resource names that grows with every LKE release.
var platformReservedNamespaces = map[string]bool{"kube-system": true}

// WithPlatformNamespaces scopes the fail-closed guards to the namespaces the
// platform occupies. The caller owns that list (converge's healthNamespaces).
func (i OwnershipIndex) WithPlatformNamespaces(ns []string) OwnershipIndex {
	i.platformNamespaces = make(map[string]bool, len(ns))
	for _, n := range ns {
		i.platformNamespaces[n] = true
	}
	// Everything else an instance-owned Application declares into is an app
	// namespace: the platform does not occupy it, and only the widened scan brings
	// it into the report at all. See the instanceNamespaces field.
	i.instanceNamespaces = map[string]bool{}
	for _, n := range i.Namespaces() {
		if !i.platformNamespaces[n] && !i.platformOccupied[n] && !platformReservedNamespaces[n] {
			i.instanceNamespaces[n] = true
		}
	}
	i.misprojected = i.misprojectedApps()
	return i
}

// misprojectedApps finds the Applications that look like the instance's own but
// are not scoped to the instance-custom AppProject.
//
// THE TEST IS "LIVES ONLY WHERE THE APP ESTATE LIVES", and every word of it is
// load-bearing for the noise floor. apl-core's platform Applications share
// istio-system and argocd with the escape hatch by design, so sharing A namespace
// proves nothing; requiring that EVERY declared namespace is one an instance-owned
// Application also occupies, and that none of them is a namespace the caller named
// as the platform's, leaves the platform's own ~40 Applications silent and names
// the ApplicationSet somebody forgot to project.
//
// It is deliberately blind to the first app in a brand-new namespace: with no
// correctly-projected sibling there, nothing distinguishes it from a platform
// Application. That case gates, loudly, in the report — this line is for the more
// common one, where the namespace already carries instance-custom content.
func (i OwnershipIndex) misprojectedApps() []string {
	var out []string
	for name, namespaces := range i.platformAppNS {
		looksInstance := len(namespaces) > 0
		for _, ns := range namespaces {
			if !i.instanceDeclaredNS[ns] || i.platformNamespaces[ns] || platformReservedNamespaces[ns] {
				looksInstance = false
				break
			}
		}
		if looksInstance {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Misprojected names the Applications that declare only into the app estate but
// are not in the instance-custom AppProject, so the boundary reads them as
// platform and their content gates.
func (i OwnershipIndex) Misprojected() []string { return i.misprojected }

// InstanceNamespaces lists the namespaces the platform does not occupy that
// instance-owned Applications declare into — the app estate the widened scan
// reaches. Empty until WithPlatformNamespaces has been called, because "not a
// platform namespace" has no meaning before the caller says which those are.
func (i OwnershipIndex) InstanceNamespaces() []string {
	out := make([]string, 0, len(i.instanceNamespaces))
	for ns := range i.instanceNamespaces {
		out = append(out, ns)
	}
	sort.Strings(out)
	return out
}

// RecordGenerated links a controller-generated resource to the parent an
// Application declares, closing a chain ownerReferences alone cannot: a
// CronWorkflow's Workflow has a generated name, so its PODS resolve only as far as
// that Workflow and would gate the platform while the Workflow itself is demoted.
//
// The map is shared by every copy of the index (a map header copies by
// reference), so a section that learns a link — checkWorkflows, listing the
// Workflows and their owners — makes it available to the sections that run after
// it. Order is the contract: record before the consumer runs, or the link is
// simply absent and the resource gates, which is the fail-closed direction.
func (i OwnershipIndex) RecordGenerated(child, parent ResourceRef) {
	if i.generated == nil || child.Kind == "" || child.Name == "" {
		return
	}
	i.generated[child] = parent
}

// Namespaces lists the namespaces instance-owned Applications declare resources
// in. The per-namespace sections iterate the PLATFORM's namespaces, so without
// this an app in a team namespace is examined by neither scope.
func (i OwnershipIndex) Namespaces() []string {
	seen := map[string]bool{}
	var out []string
	for ref := range i.owned {
		if ref.Namespace != "" && !seen[ref.Namespace] {
			seen[ref.Namespace] = true
			out = append(out, ref.Namespace)
		}
	}
	sort.Strings(out)
	return out
}

// PlatformUnresolved names platform Applications Argo has not compared, so the
// contested pass could not see what they own. A COMPARED platform Application
// that declares nothing is not here: it owns nothing, and there is nothing for
// the contested pass to have missed.
func (i OwnershipIndex) PlatformUnresolved() []string { return i.platformUnresolved }

// Contested is how many resources an instance-owned Application claimed that a
// platform Application claims too — refused, and still gating.
func (i OwnershipIndex) Contested() int { return i.contested }

// Unresolved names the instance-owned Applications Argo has not compared, so
// their content could not be attributed to them on this scan and gates the
// platform meanwhile. A COMPARED instance Application that declares nothing is
// not here — it has no content to attribute.
func (i OwnershipIndex) Unresolved() []string { return i.unresolved }

// Owns reports whether an instance-owned Application — and no platform one —
// declares this resource, subject to the two guards the contested pass cannot
// enforce on its own.
func (i OwnershipIndex) Owns(ref ResourceRef) bool {
	// A ref with no identity matches nothing. checkPods passes a namespace-only
	// ref for a pod with no controller, and one malformed .status.resources entry
	// would otherwise make every such pod in that namespace instance-owned.
	if ref.Kind == "" || ref.Name == "" {
		return false
	}
	if platformReservedNamespaces[ref.Namespace] {
		return false
	}
	if i.platformMayClaim(ref.Namespace) {
		return false
	}
	if i.owned[ref] {
		return true
	}
	// Second hop: the resource's declared parent, for a chain ownerReferences
	// close only halfway.
	if parent, ok := i.generated[ref]; ok && i.owned[parent] {
		return true
	}
	if i.ownsStatefulSetPVC(ref) {
		return true
	}
	// LAST, AND ONLY ON EVIDENCE. A resource in a namespace the platform does not
	// occupy, that no platform Application declares, is the app estate's. This is
	// the one rule here that is not a direct claim, so it is fenced four ways:
	// instanceNamespaces already excludes every namespace the caller named AND
	// every namespace a platform Application actually has a resource in; a
	// reserved namespace returned above; a platform Application's own claim on
	// this exact resource beats it; and it is off entirely while any platform
	// Application is UNCOMPARED — an app Argo has not diffed yet could have
	// declared this one, and a namespace-shaped guess is not the place to
	// overrule that. (A compared platform app that declares nothing is not in
	// that list: it has answered, and its answer is "not mine".)
	if len(i.platformUnresolved) > 0 {
		return false
	}
	return i.instanceNamespaces[ref.Namespace] && !i.platformDeclared[ref]
}

// platformMayClaim reports whether an UNCOMPARED PLATFORM Application could have
// declared a resource in this namespace, in which case nothing there is
// demotable this poll.
//
// "THIS POLL" IS LOAD-BEARING and only true because of argoCompared. While zero
// resources alone qualified, four permanently-empty apl-core gitops shells kept
// this returning true on every poll forever — a guard that reads as transient
// and is in fact structural. An uncompared app really does resolve on a later
// poll, so the veto really is transient now.
//
// SCOPED TO THE DESTINATION, not to every platform namespace. The blanket form
// meant one Application mid-comparison — routine during a bootstrap, when Argo is
// still working through the tree — switched the whole boundary off across all
// twelve platform namespaces, including istio-system where the instance's own
// oauth2-proxy Deployment and ExternalSecrets live. The demotion then flipped
// poll to poll for reasons no reader could see in the report. An Application that
// names no destination still bounds nothing and still vetoes broadly.
func (i OwnershipIndex) platformMayClaim(ns string) bool {
	if len(i.platformUnresolved) == 0 {
		return false
	}
	if i.platformUnresolvedAnywhere && i.platformNamespaces[ns] {
		return true
	}
	return i.platformUnresolvedNS[ns]
}

// ownsStatefulSetPVC resolves a volumeClaimTemplate PVC to its StatefulSet by the
// name Kubernetes generates — `<template>-<statefulset>-<ordinal>` — because such
// a PVC carries no ownerReferences under the default Retain retention policy and
// appears in no Application's declared set under that generated name.
func (i OwnershipIndex) ownsStatefulSetPVC(ref ResourceRef) bool {
	if ref.Kind != "PersistentVolumeClaim" {
		return false
	}
	cut := strings.LastIndex(ref.Name, "-")
	if cut <= 0 {
		return false
	}
	if _, err := strconv.Atoi(ref.Name[cut+1:]); err != nil { // the ordinal
		return false
	}
	for _, sts := range i.statefulSets[ref.Namespace] {
		if strings.HasSuffix(ref.Name[:cut], "-"+sts) {
			return true
		}
	}
	return false
}

// demote answers the boundary for one finding: the category to record, the text
// to record it under, and whether the demotion actually happened.
//
// The bool is the answer, not `cat == CatInstance`. A caller that already holds a
// CatInstance finding (the Application-level path) would otherwise take the
// demoted branch without the index ever being consulted — fail-open by input
// category alone, in a file whose whole posture is fail-closed.
//
// CatOK, CatWarn, CatDeferred and CatDrift are already non-gating and pass
// through untouched — a drift line on an instance resource still reads as drift.
func (i OwnershipIndex) demote(cat Category, ref ResourceRef, msg string) (Category, string, bool) {
	return instanceDemotion(cat, i.Owns(ref), msg)
}

// InstanceOwnedSuffix is the explanation appended to every instance-owned
// finding. One constant so the Application-level and resource-level paths cannot
// drift into saying different things about the same boundary.
const InstanceOwnedSuffix = "  ⇒ instance-owned (operator escape hatch); reported, does NOT gate platform convergence"

// PodWorkloadOwner maps a resource's ownerReferences to the workload an Argo
// Application would declare — collapsing the ReplicaSet indirection to its
// Deployment. Returns the zero ResourceRef and false when there is no usable
// controller owner; a bare pod belongs to whoever created it, is claimed by no
// Application, and therefore gates.
//
// THE CONTROLLER REF IS THE ANSWER, not the first one. A resource may carry
// several ownerReferences with at most one `controller: true`, and the API server
// preserves whatever order the writer supplied — reading owners[0] resolves a
// GC-adoption or CRD back-reference as if it were the controller.
//
// THE REPLICASET STEP IS A NAMING CONVENTION, and the only one here. A Deployment
// owns a ReplicaSet named `<deployment>-<podtemplatehash>` and the ReplicaSet owns
// the pods; Argo declares the DEPLOYMENT. The hash is validated against the
// alphabet Kubernetes actually generates it from, so a bare ReplicaSet that merely
// contains a hyphen — `web-canary`, `dispatch-worker` — is NOT folded into a
// Deployment that may exist and may be instance-owned.
func PodWorkloadOwner(owners []OwnerRef, namespace string) (ResourceRef, bool) {
	o, ok := controllerOwner(owners)
	if !ok {
		return ResourceRef{}, false
	}
	group := ownerGroup(o.APIVersion)
	if o.Kind == "ReplicaSet" {
		if deploy, ok := deploymentFromReplicaSet(o.Name); ok {
			return ResourceRef{Group: group, Kind: "Deployment", Namespace: namespace, Name: deploy}, true
		}
		// A ReplicaSet with no pod-template-hash suffix was not made by a
		// Deployment; it is itself the declared resource.
	}
	return ResourceRef{Group: group, Kind: o.Kind, Namespace: namespace, Name: o.Name}, true
}

// controllerOwner picks the owning controller: the ref marked `controller: true`
// if one is present, else the first usable ref (older writers omit the field).
func controllerOwner(owners []OwnerRef) (OwnerRef, bool) {
	var first OwnerRef
	haveFirst := false
	for _, o := range owners {
		if o.Kind == "" || o.Name == "" {
			continue
		}
		if o.Controller != nil && *o.Controller {
			return o, true
		}
		if !haveFirst {
			first, haveFirst = o, true
		}
	}
	return first, haveFirst
}

// ownerGroup extracts the API group from an ownerReference's apiVersion
// ("apps/v1" → "apps", "v1" → "").
func ownerGroup(apiVersion string) string {
	if i := strings.Index(apiVersion, "/"); i > 0 {
		return apiVersion[:i]
	}
	return ""
}

// podTemplateHashAlphabet is rand.SafeEncodeString's alphabet — the character set
// Kubernetes builds a pod-template-hash from. It deliberately omits vowels and
// the digits 0/1/3, which is what makes a hash distinguishable from a word.
const podTemplateHashAlphabet = "bcdfghjklmnpqrstvwxz2456789"

// deploymentFromReplicaSet strips the pod-template-hash segment a Deployment
// appends when it names a ReplicaSet. ok=false when the final segment is not
// hash-SHAPED, which means no Deployment generated it — the difference between
// `dispatch-76b5bb9749` (a Deployment's ReplicaSet) and `dispatch-worker` (a bare
// ReplicaSet that would otherwise resolve to the unrelated Deployment `dispatch`).
func deploymentFromReplicaSet(rs string) (string, bool) {
	i := strings.LastIndex(rs, "-")
	if i <= 0 || i == len(rs)-1 {
		return "", false
	}
	if !isPodTemplateHash(rs[i+1:]) {
		return "", false
	}
	return rs[:i], true
}

func isPodTemplateHash(s string) bool {
	if len(s) < 5 || len(s) > 10 {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune(podTemplateHashAlphabet, c) {
			return false
		}
	}
	return true
}

// ── Workflows: the one chain neither ownerReferences nor .status.resources closes ──

// WorkflowTemplateLabel / ClusterWorkflowTemplateLabel are the labels the Argo
// Workflows controller stamps on a Workflow it ran from a (Cluster)WorkflowTemplate.
// They are the fallback for a Workflow whose spec carries the templates inline —
// `argo submit --from` records a workflowTemplateRef, but a Workflow rendered from
// a template by other means carries only the label.
const (
	WorkflowTemplateLabel        = "workflows.argoproj.io/workflow-template"
	ClusterWorkflowTemplateLabel = "workflows.argoproj.io/cluster-workflow-template"
)

// WorkflowDeclaredOwner resolves a Workflow to the resource an Argo Application
// actually declares.
//
// THIS IS WHERE THE BOUNDARY LEAKED. A Workflow reaches a cluster three ways, and
// only one of them was covered:
//
//   - A CronWorkflow spawns it, setting an ownerReference — handled, and the only
//     case the first cut of this boundary knew about.
//   - `argo submit --from workflowtemplate/<name>` creates it with NO
//     ownerReferences and a generated name. The Application declares the
//     WorkflowTemplate; nothing anywhere declares the Workflow. So the Workflow
//     resolved to itself, missed the index, and hard-failed the PLATFORM gate —
//     as did every pod it owns, which resolve no further than that same Workflow.
//     This is the documented way to run the managed-apps build (`argo submit --from
//     workflowtemplate/docker-build-<app>-<rev>`), so on the run that motivated the
//     boundary four failed builds and their pods still gated the platform after the
//     Applications, ExternalSecrets, Deployments and app pods had all been demoted.
//   - An Argo Events sensor submits it: the same ownerless shape.
//
// The template ref IS declared, so it is the answer. ok=false when there is
// neither an owner nor a template — a Workflow applied straight from a manifest is
// declared under its own name, and the caller routes on that.
func WorkflowDeclaredOwner(owners []OwnerRef, templateRef string, clusterScope bool, labels map[string]string, namespace string) (ResourceRef, bool) {
	// The controller ref wins: a CronWorkflow's Workflow also carries a
	// workflowTemplateRef when the CronWorkflow uses one, and the CronWorkflow is
	// the resource the Application declares.
	if owner, ok := PodWorkloadOwner(owners, namespace); ok {
		return owner, true
	}
	if templateRef == "" {
		templateRef, clusterScope = labels[WorkflowTemplateLabel], false
		if templateRef == "" {
			templateRef, clusterScope = labels[ClusterWorkflowTemplateLabel], true
		}
	}
	if templateRef == "" {
		return ResourceRef{}, false
	}
	if clusterScope {
		// Cluster-scoped: Argo reports a cluster-scoped resource in
		// .status.resources with an empty namespace, so the ref must carry one too.
		return ResourceRef{Group: WorkflowsGroup, Kind: "ClusterWorkflowTemplate", Name: templateRef}, true
	}
	return ResourceRef{Group: WorkflowsGroup, Kind: "WorkflowTemplate", Namespace: namespace, Name: templateRef}, true
}

// WorkflowsGroup is the API group Argo reports for Workflows, CronWorkflows and
// WorkflowTemplates in .status.resources. Named once because the ownership index
// keys on the group and a mismatch fails silently — as a miss, which gates.
const WorkflowsGroup = "argoproj.io"
