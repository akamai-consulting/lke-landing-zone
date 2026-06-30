package main

// import_cluster.go holds the deeper cluster-introspection parsers for `llz
// import scan`: node-pool layout, storage classes, routing/domains (Istio +
// cert-manager, since APL does not use Ingress), load balancers, installed
// operators (from CRDs), app versions (from image tags), the cert-manager
// ClusterIssuer, and per-team resource quotas. Every function here is pure (raw
// kubectl JSON in, struct out) so the mapping is unit-tested; the kubectl calls
// live in runImportScan.

import (
	"encoding/json"
	"sort"
	"strings"
)

// ── node pools (pure) ────────────────────────────────────────────────────────

type nodePool struct {
	PoolID   string `json:"poolID,omitempty"`
	NodeType string `json:"nodeType,omitempty"`
	Count    int    `json:"count"`
}

// parseNodePools groups nodes by their LKE pool id (lke.linode.com/pool-id),
// reporting the instance type + node count per pool — the real provisioning
// layout that the single majority nodeType collapses. Returns nil for a plain
// single-pool cluster with no pool labels (the cluster.NodeType already covers it).
func parseNodePools(js string) []nodePool {
	var d struct {
		Items []struct {
			Metadata struct {
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if json.Unmarshal([]byte(js), &d) != nil {
		return nil
	}
	type agg struct {
		nodeType string
		count    int
	}
	byPool := map[string]*agg{}
	var sawPoolID bool
	for _, it := range d.Items {
		l := it.Metadata.Labels
		pool := l["lke.linode.com/pool-id"]
		if pool != "" {
			sawPoolID = true
		}
		itype := firstLabel(l, "node.kubernetes.io/instance-type", "beta.kubernetes.io/instance-type")
		key := pool
		if key == "" {
			key = "type:" + itype // no pool label → at least separate by instance type
		}
		a := byPool[key]
		if a == nil {
			a = &agg{nodeType: itype}
			byPool[key] = a
		}
		a.count++
	}
	if !sawPoolID && len(byPool) <= 1 {
		return nil // single homogeneous pool, nothing the majority didn't say
	}
	var out []nodePool
	for key, a := range byPool {
		id := key
		if strings.HasPrefix(key, "type:") {
			id = ""
		}
		out = append(out, nodePool{PoolID: id, NodeType: a.nodeType, Count: a.count})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].PoolID != out[j].PoolID {
			return out[i].PoolID < out[j].PoolID
		}
		return out[i].NodeType < out[j].NodeType
	})
	return out
}

// ── storage classes (pure) ───────────────────────────────────────────────────

type storageClass struct {
	Name        string `json:"name"`
	Provisioner string `json:"provisioner,omitempty"`
	Default     bool   `json:"default,omitempty"`
}

func parseStorageClasses(js string) []storageClass {
	var d struct {
		Items []struct {
			Metadata struct {
				Name        string            `json:"name"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
			Provisioner string `json:"provisioner"`
		} `json:"items"`
	}
	if json.Unmarshal([]byte(js), &d) != nil {
		return nil
	}
	var out []storageClass
	for _, it := range d.Items {
		out = append(out, storageClass{
			Name:        it.Metadata.Name,
			Provisioner: it.Provisioner,
			Default:     it.Metadata.Annotations["storageclass.kubernetes.io/is-default-class"] == "true",
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ── routing / domains (pure) ─────────────────────────────────────────────────

// parseIstioHosts maps namespace → hosts from Istio Gateways (spec.servers[].hosts,
// which may be "ns/host" — the ns/ prefix is stripped) and VirtualServices
// (spec.hosts[]). Wildcard-only entries ("*") are dropped. APL routes through
// Istio, so this is where the real hostnames live (not Ingress).
func parseIstioHosts(gatewayJSON, virtualServiceJSON string) map[string][]string {
	out := map[string][]string{}

	var gw struct {
		Items []struct {
			Metadata struct {
				Namespace string `json:"namespace"`
			} `json:"metadata"`
			Spec struct {
				Servers []struct {
					Hosts []string `json:"hosts"`
				} `json:"servers"`
			} `json:"spec"`
		} `json:"items"`
	}
	if json.Unmarshal([]byte(gatewayJSON), &gw) == nil {
		for _, it := range gw.Items {
			for _, s := range it.Spec.Servers {
				for _, h := range s.Hosts {
					if hn := normalizeHost(h); hn != "" {
						out[it.Metadata.Namespace] = append(out[it.Metadata.Namespace], hn)
					}
				}
			}
		}
	}

	var vs struct {
		Items []struct {
			Metadata struct {
				Namespace string `json:"namespace"`
			} `json:"metadata"`
			Spec struct {
				Hosts []string `json:"hosts"`
			} `json:"spec"`
		} `json:"items"`
	}
	if json.Unmarshal([]byte(virtualServiceJSON), &vs) == nil {
		for _, it := range vs.Items {
			for _, h := range it.Spec.Hosts {
				if hn := normalizeHost(h); hn != "" {
					out[it.Metadata.Namespace] = append(out[it.Metadata.Namespace], hn)
				}
			}
		}
	}
	return out
}

// parseCertDNSNames maps namespace → cert-manager Certificate spec.dnsNames.
func parseCertDNSNames(js string) map[string][]string {
	var d struct {
		Items []struct {
			Metadata struct {
				Namespace string `json:"namespace"`
			} `json:"metadata"`
			Spec struct {
				DNSNames []string `json:"dnsNames"`
			} `json:"spec"`
		} `json:"items"`
	}
	out := map[string][]string{}
	if json.Unmarshal([]byte(js), &d) != nil {
		return out
	}
	for _, it := range d.Items {
		for _, h := range it.Spec.DNSNames {
			if hn := normalizeHost(h); hn != "" {
				out[it.Metadata.Namespace] = append(out[it.Metadata.Namespace], hn)
			}
		}
	}
	return out
}

// normalizeHost strips an Istio "ns/" selector prefix and a leading "*." wildcard,
// then drops anything that isn't an external DNS name: a bare "*", a single-label
// name (e.g. "barman-cloud", a webhook service), or a cluster-internal address
// (".svc" / ".svc.cluster.local"). Returns "" for those — they're not domains an
// operator migrates.
func normalizeHost(h string) string {
	if i := strings.Index(h, "/"); i >= 0 {
		h = h[i+1:] // "ns/host" or "*/host"
	}
	h = strings.TrimPrefix(h, "*.")
	if h == "" || h == "*" {
		return ""
	}
	if !strings.Contains(h, ".") { // single label — not a routable domain
		return ""
	}
	if strings.HasSuffix(h, ".svc") || strings.HasSuffix(h, ".svc.cluster.local") || strings.Contains(h, ".svc.") {
		return "" // cluster-internal Service DNS
	}
	return h
}

// mergeHostSources unions several namespace→hosts maps.
func mergeHostSources(sources ...map[string][]string) map[string][]string {
	out := map[string][]string{}
	for _, src := range sources {
		for ns, hosts := range src {
			out[ns] = append(out[ns], hosts...)
		}
	}
	return out
}

// allHostValues flattens every namespace's hosts into one deduped, sorted list.
func allHostValues(byNS map[string][]string) []string {
	var all []string
	for _, hosts := range byNS {
		all = append(all, hosts...)
	}
	return dedupeSorted(all)
}

// commonDomainSuffix returns the longest dot-delimited suffix shared by every
// host (e.g. app.demo.example.com + api.example.com → example.com). Requires at
// least two labels to be meaningful; a bare TLD ("com") returns "".
func commonDomainSuffix(hosts []string) string {
	if len(hosts) == 0 {
		return ""
	}
	// Reversed label lists.
	rev := make([][]string, 0, len(hosts))
	for _, h := range hosts {
		labels := strings.Split(h, ".")
		for i, j := 0, len(labels)-1; i < j; i, j = i+1, j-1 {
			labels[i], labels[j] = labels[j], labels[i]
		}
		rev = append(rev, labels)
	}
	var common []string
	for i := 0; ; i++ {
		var label string
		for j, labels := range rev {
			if i >= len(labels) {
				goto done
			}
			if j == 0 {
				label = labels[i]
			} else if labels[i] != label {
				goto done
			}
		}
		common = append(common, label)
	}
done:
	if len(common) < 2 {
		return ""
	}
	for i, j := 0, len(common)-1; i < j; i, j = i+1, j-1 {
		common[i], common[j] = common[j], common[i]
	}
	return strings.Join(common, ".")
}

// ── load balancers (pure) ────────────────────────────────────────────────────

func parseLoadBalancers(js string) []lbService {
	var d struct {
		Items []struct {
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
			Spec struct {
				Type string `json:"type"`
			} `json:"spec"`
			Status struct {
				LoadBalancer struct {
					Ingress []struct {
						IP       string `json:"ip"`
						Hostname string `json:"hostname"`
					} `json:"ingress"`
				} `json:"loadBalancer"`
			} `json:"status"`
		} `json:"items"`
	}
	if json.Unmarshal([]byte(js), &d) != nil {
		return nil
	}
	var out []lbService
	for _, it := range d.Items {
		if it.Spec.Type != "LoadBalancer" {
			continue
		}
		var addrs []string
		for _, ing := range it.Status.LoadBalancer.Ingress {
			if ing.IP != "" {
				addrs = append(addrs, ing.IP)
			} else if ing.Hostname != "" {
				addrs = append(addrs, ing.Hostname)
			}
		}
		out = append(out, lbService{Namespace: it.Metadata.Namespace, Name: it.Metadata.Name, Addresses: addrs})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// ── operators from CRDs (pure) ───────────────────────────────────────────────

// crdComponentByName maps a CRD's full name to the LLZ component it implies — the
// reliable signal (the object exists) versus a namespace-name guess.
var crdComponentByName = map[string]string{
	"applications.argoproj.io":                    "argocd",
	"workflows.argoproj.io":                       "argoWorkflows",
	"sensors.argoproj.io":                         "argoEvents",
	"eventsources.argoproj.io":                    "argoEvents",
	"certificates.cert-manager.io":                "certManager",
	"clusterpolicies.kyverno.io":                  "policyEngine",
	"policies.kyverno.io":                         "policyEngine",
	"externalsecrets.external-secrets.io":         "externalSecrets",
	"vulnerabilityreports.aquasecurity.github.io": "imageScanning",
	"prometheuses.monitoring.coreos.com":          "observability",
}

// crdGroupOperator maps a CRD API group to a friendly operator name.
func crdGroupOperator(group string) string {
	switch {
	case group == "cert-manager.io":
		return "cert-manager"
	case strings.HasSuffix(group, "istio.io"):
		return "istio"
	case group == "kyverno.io":
		return "kyverno"
	case group == "argoproj.io":
		return "argo"
	case group == "tekton.dev":
		return "tekton"
	case strings.HasSuffix(group, "keycloak.org"):
		return "keycloak"
	case group == "external-secrets.io":
		return "external-secrets"
	case group == "goharbor.io":
		return "harbor"
	case group == "aquasecurity.github.io":
		return "trivy"
	case group == "monitoring.coreos.com":
		return "prometheus-operator"
	case strings.HasSuffix(group, "cilium.io"):
		return "cilium"
	}
	return ""
}

// parseCRDOperators returns the friendly operator names and the LLZ components
// implied by the installed CustomResourceDefinitions.
func parseCRDOperators(js string) (operators []string, components map[string]bool) {
	var d struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Group string `json:"group"`
			} `json:"spec"`
		} `json:"items"`
	}
	components = map[string]bool{}
	if json.Unmarshal([]byte(js), &d) != nil {
		return nil, nil
	}
	opSet := map[string]bool{}
	for _, it := range d.Items {
		if op := crdGroupOperator(it.Spec.Group); op != "" {
			opSet[op] = true
		}
		if c, ok := crdComponentByName[it.Metadata.Name]; ok {
			components[c] = true
		}
	}
	if len(components) == 0 {
		components = nil
	}
	return sortedSetKeys(opSet), components
}

// ── app versions from images (pure) ──────────────────────────────────────────

// versionAppByImageSubstring maps a substring of a container image reference to
// the app whose version it carries.
var versionAppByImageSubstring = []struct{ sub, app string }{
	{"harbor", "harbor"},
	{"loki", "loki"},
	{"grafana", "grafana"},
	{"prometheus", "prometheus"},
	{"keycloak", "keycloak"},
	{"kyverno", "kyverno"},
	{"trivy", "trivy"},
	{"tekton", "tekton"},
	{"gitea", "gitea"},
	{"pilot", "istio"},
	{"istio", "istio"},
	{"cert-manager", "cert-manager"},
}

// parseImageVersions derives the APL/Otomi version and a best-effort app→version
// map from running container image tags.
func parseImageVersions(workloads []workload) (aplVersion string, versions map[string]string) {
	versions = map[string]string{}
	for _, w := range workloads {
		for _, img := range w.Images {
			tag := imageTag(img)
			if tag == "" {
				continue
			}
			lower := strings.ToLower(img)
			if aplVersion == "" && (strings.Contains(lower, "otomi") || strings.Contains(lower, "apl-core") || strings.Contains(lower, "/apl-")) {
				aplVersion = tag
			}
			// Match on the image NAME (repo basename), not the full ref — else a
			// registry org like "grafana/loki" would mis-attribute loki's tag to
			// grafana.
			name := strings.ToLower(imageName(img))
			for _, m := range versionAppByImageSubstring {
				if _, seen := versions[m.app]; !seen && strings.Contains(name, m.sub) {
					versions[m.app] = tag
				}
			}
		}
	}
	if len(versions) == 0 {
		versions = nil
	}
	return aplVersion, versions
}

// imageName returns the repo basename of a container image reference (no
// registry, org, tag, or digest): "grafana/loki:2.9.2" → "loki".
func imageName(image string) string {
	if i := strings.Index(image, "@"); i >= 0 {
		image = image[:i]
	}
	slash := strings.LastIndex(image, "/")
	if colon := strings.LastIndex(image, ":"); colon > slash {
		image = image[:colon]
	}
	if i := strings.LastIndex(image, "/"); i >= 0 {
		return image[i+1:]
	}
	return image
}

// imageTag returns the tag of a container image reference, or "" if untagged.
// A digest (@sha256:…) is stripped; a registry port colon is not mistaken for a
// tag (the tag colon must come after the last "/").
func imageTag(image string) string {
	if i := strings.Index(image, "@"); i >= 0 {
		image = image[:i]
	}
	slash := strings.LastIndex(image, "/")
	colon := strings.LastIndex(image, ":")
	if colon > slash {
		return image[colon+1:]
	}
	return ""
}

// ── cert-manager ClusterIssuer (pure) ────────────────────────────────────────

// parseClusterIssuers returns the first ACME registration email and the set of
// solver types (dns01/http01) across the cluster's ClusterIssuers.
func parseClusterIssuers(js string) (acmeEmail string, solvers []string) {
	var d struct {
		Items []struct {
			Spec struct {
				ACME struct {
					Email   string `json:"email"`
					Solvers []struct {
						DNS01  json.RawMessage `json:"dns01"`
						HTTP01 json.RawMessage `json:"http01"`
					} `json:"solvers"`
				} `json:"acme"`
			} `json:"spec"`
		} `json:"items"`
	}
	if json.Unmarshal([]byte(js), &d) != nil {
		return "", nil
	}
	solverSet := map[string]bool{}
	for _, it := range d.Items {
		if acmeEmail == "" && it.Spec.ACME.Email != "" {
			acmeEmail = it.Spec.ACME.Email
		}
		for _, s := range it.Spec.ACME.Solvers {
			if len(s.DNS01) > 0 {
				solverSet["dns01"] = true
			}
			if len(s.HTTP01) > 0 {
				solverSet["http01"] = true
			}
		}
	}
	return acmeEmail, sortedSetKeys(solverSet)
}

// ── resource quotas (pure) ───────────────────────────────────────────────────

// parseResourceQuotas maps namespace → its CPU/memory hard limits (other quota
// keys are dropped to keep the team rollup readable). Merges multiple quotas in a
// namespace.
func parseResourceQuotas(js string) map[string]map[string]string {
	var d struct {
		Items []struct {
			Metadata struct {
				Namespace string `json:"namespace"`
			} `json:"metadata"`
			Spec struct {
				Hard map[string]string `json:"hard"`
			} `json:"spec"`
		} `json:"items"`
	}
	if json.Unmarshal([]byte(js), &d) != nil {
		return nil
	}
	out := map[string]map[string]string{}
	for _, it := range d.Items {
		for k, v := range it.Spec.Hard {
			if !strings.Contains(k, "cpu") && !strings.Contains(k, "memory") {
				continue
			}
			if out[it.Metadata.Namespace] == nil {
				out[it.Metadata.Namespace] = map[string]string{}
			}
			out[it.Metadata.Namespace][k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
