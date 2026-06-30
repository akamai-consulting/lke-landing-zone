package main

// import_linode.go adds the Linode-API source to `llz import scan`. The live
// cluster (kubectl) and the repos can't see how the cluster was PROVISIONED —
// node-pool autoscaling, the VPC subnet CIDR, the Cloud Firewall rules, the
// NodeBalancers, and the Object Storage backends all live in the Linode API.
// These map directly onto the LLZ cluster / object-storage / firewall tfvars.
//
// The HTTP calls (enrichFromLinode) reuse internal/linode.Client and are the only
// I/O; every extractor below is pure (decoded Linode JSON in, struct out) and
// unit-tested. Linode list responses are decoded with json.Number (the client
// sets UseNumber), so the map helpers accept json.Number as well as float64.

import (
	"context"
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/linode"
)

func parseUint(s string) uint64  { n, _ := strconv.ParseUint(s, 10, 64); return n }
func formatUint(n uint64) string { return strconv.FormatUint(n, 10) }

type importLinode struct {
	ClusterID      uint64        `json:"clusterID,omitempty"`
	Label          string        `json:"label,omitempty"`
	Region         string        `json:"region,omitempty"`
	K8sVersion     string        `json:"k8sVersion,omitempty"`
	ControlPlaneHA bool          `json:"controlPlaneHA,omitempty"`
	Tags           []string      `json:"tags,omitempty"`
	NodePools      []lkePool     `json:"nodePools,omitempty"`
	VPC            *lkeVPC       `json:"vpc,omitempty"`
	Firewalls      []lkeFirewall `json:"firewalls,omitempty"`
	NodeBalancers  int           `json:"nodeBalancers,omitempty"`
	ObjectStorage  []lkeBucket   `json:"objectStorage,omitempty"`
}

type lkePool struct {
	ID                uint64 `json:"id,omitempty"`
	Type              string `json:"type,omitempty"`
	Count             int    `json:"count"`
	AutoscalerEnabled bool   `json:"autoscalerEnabled,omitempty"`
	Min               int    `json:"min,omitempty"`
	Max               int    `json:"max,omitempty"`
}

type lkeVPC struct {
	ID      uint64   `json:"id,omitempty"`
	Label   string   `json:"label,omitempty"`
	Region  string   `json:"region,omitempty"`
	Subnets []string `json:"subnetCIDRs,omitempty"`
}

type lkeFirewall struct {
	ID            uint64   `json:"id,omitempty"`
	Label         string   `json:"label,omitempty"`
	InboundPolicy string   `json:"inboundPolicy,omitempty"`
	InboundCIDRs  []string `json:"inboundCIDRs,omitempty"`
}

type lkeBucket struct {
	Label   string `json:"label"`
	Region  string `json:"region,omitempty"`
	Objects int    `json:"objects,omitempty"`
}

// enrichFromLinode resolves the source LKE cluster and queries the Linode API for
// the provisioning detail kubectl can't see. clusterID 0 means "resolve it": from
// the kube context name (lke<ID>-ctx), else the only cluster on the account. It
// returns nil + a reason when the token can't be used or the cluster can't be
// resolved; the caller surfaces the reason as a warning.
func enrichFromLinode(token string, clusterID uint64, contextName string) (*importLinode, string) {
	if token == "" {
		return nil, "no Linode token (set --linode-token or LINODE_API_TOKEN) — skipping Linode enrichment"
	}
	client := linode.NewClient(token, 60*time.Second)
	ctx := context.Background()

	clusters, err := client.ListClusters(ctx)
	if err != nil {
		return nil, "Linode API: list clusters failed: " + err.Error()
	}
	if clusterID == 0 {
		clusterID = lkeClusterIDFromContext(contextName)
	}
	if clusterID == 0 && len(clusters) == 1 {
		clusterID = mapUint(clusters[0], "id")
	}
	if clusterID == 0 {
		return nil, "could not resolve the LKE cluster id (pass --linode-cluster-id) — skipping Linode enrichment"
	}
	cluster, ok := findCluster(clusters, clusterID)
	if !ok {
		return nil, "LKE cluster id not found on this account — skipping Linode enrichment"
	}

	out := lkeClusterInfo(cluster)
	out.ClusterID = clusterID

	if pools, err := client.ListNodePools(ctx, clusterID); err == nil {
		out.NodePools = lkeNodePools(pools)
	}
	if vpcs, err := client.ListVPCs(ctx); err == nil {
		if vpc, ok := matchClusterVPC(vpcs, out.Label); ok {
			id := mapUint(vpc, "id")
			info := lkeVPCInfo(vpc)
			if subnets, err := client.ListVPCSubnets(ctx, id); err == nil {
				info.Subnets = vpcSubnetCIDRs(subnets)
			}
			out.VPC = &info
		}
	}
	if fws, err := client.ListFirewalls(ctx); err == nil {
		out.Firewalls = clusterFirewalls(fws, out.Label, clusterID)
	}
	if nbs, err := client.ListNodeBalancers(ctx); err == nil {
		out.NodeBalancers = countClusterNodeBalancers(nbs, clusterID)
	}
	if buckets, _, err := client.ListRaw(ctx, "v4", "object-storage/buckets"); err == nil {
		out.ObjectStorage = objectStorageBuckets(buckets)
	}
	return &out, ""
}

// ── pure extractors ──────────────────────────────────────────────────────────

var lkeCtxRe = regexp.MustCompile(`lke(\d+)`)

// lkeClusterIDFromContext pulls the numeric id out of a Linode-generated kube
// context name like "lke579582-ctx".
func lkeClusterIDFromContext(name string) uint64 {
	m := lkeCtxRe.FindStringSubmatch(name)
	if m == nil {
		return 0
	}
	return parseUint(m[1])
}

func findCluster(clusters []map[string]any, id uint64) (map[string]any, bool) {
	for _, c := range clusters {
		if mapUint(c, "id") == id {
			return c, true
		}
	}
	return nil, false
}

// lkeClusterInfo reads identity + control-plane HA + tags from a /lke/clusters item.
func lkeClusterInfo(m map[string]any) importLinode {
	out := importLinode{
		Label:      mapString(m, "label"),
		Region:     mapString(m, "region"),
		K8sVersion: mapString(m, "k8s_version"),
		Tags:       mapStrings(m, "tags"),
	}
	if cp := mapChild(m, "control_plane"); cp != nil {
		out.ControlPlaneHA = mapBool(cp, "high_availability")
	}
	return out
}

func lkeNodePools(pools []map[string]any) []lkePool {
	var out []lkePool
	for _, p := range pools {
		pool := lkePool{
			ID:    mapUint(p, "id"),
			Type:  mapString(p, "type"),
			Count: mapInt(p, "count"),
		}
		if as := mapChild(p, "autoscaler"); as != nil {
			pool.AutoscalerEnabled = mapBool(as, "enabled")
			pool.Min = mapInt(as, "min")
			pool.Max = mapInt(as, "max")
		}
		out = append(out, pool)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// matchClusterVPC finds the VPC for a cluster by label: LKE-E names it
// "<clusterLabel>" or "<clusterLabel>-vpc"; falls back to a label that contains
// the cluster label.
func matchClusterVPC(vpcs []map[string]any, clusterLabel string) (map[string]any, bool) {
	if clusterLabel == "" {
		return nil, false
	}
	var contains map[string]any
	for _, v := range vpcs {
		l := mapString(v, "label")
		if l == clusterLabel || l == clusterLabel+"-vpc" {
			return v, true
		}
		if contains == nil && strings.Contains(l, clusterLabel) {
			contains = v
		}
	}
	if contains != nil {
		return contains, true
	}
	return nil, false
}

func lkeVPCInfo(m map[string]any) lkeVPC {
	return lkeVPC{ID: mapUint(m, "id"), Label: mapString(m, "label"), Region: mapString(m, "region")}
}

func vpcSubnetCIDRs(subnets []map[string]any) []string {
	var out []string
	for _, s := range subnets {
		if cidr := mapString(s, "ipv4"); cidr != "" {
			out = append(out, cidr)
		}
	}
	sort.Strings(out)
	return out
}

// clusterFirewalls returns the firewalls that protect the cluster's nodes. The
// LKE-managed node firewall is named "lke-<id>" (and older/LKE-E variants tag
// "lke<id>" or embed the cluster label), so match on any of those — plus the
// platform default "platform-nodes-fw".
func clusterFirewalls(fws []map[string]any, clusterLabel string, clusterID uint64) []lkeFirewall {
	id := formatUint(clusterID)
	var out []lkeFirewall
	for _, fw := range fws {
		label := mapString(fw, "label")
		match := label == "platform-nodes-fw" ||
			strings.Contains(label, "lke-"+id) || // LKE node firewall: lke-<id>
			strings.Contains(label, "lke"+id) ||
			(clusterLabel != "" && strings.Contains(label, clusterLabel)) ||
			containsString(mapStrings(fw, "tags"), "lke"+id)
		if !match {
			continue
		}
		out = append(out, lkeFirewallInfo(fw))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}

// lkeFirewallInfo reads a firewall's inbound policy + the union of its inbound
// rule CIDRs (IPv4 + IPv6).
func lkeFirewallInfo(fw map[string]any) lkeFirewall {
	out := lkeFirewall{ID: mapUint(fw, "id"), Label: mapString(fw, "label")}
	rules := mapChild(fw, "rules")
	if rules == nil {
		return out
	}
	out.InboundPolicy = mapString(rules, "inbound_policy")
	set := map[string]bool{}
	for _, r := range mapSlice(rules, "inbound") {
		rm, ok := r.(map[string]any)
		if !ok {
			continue
		}
		addrs := mapChild(rm, "addresses")
		if addrs == nil {
			continue
		}
		for _, cidr := range append(mapStrings(addrs, "ipv4"), mapStrings(addrs, "ipv6")...) {
			set[cidr] = true
		}
	}
	out.InboundCIDRs = sortedSetKeys(set)
	return out
}

func countClusterNodeBalancers(nbs []map[string]any, clusterID uint64) int {
	want := formatUint(clusterID)
	n := 0
	for _, nb := range nbs {
		if linode.LKEClusterIDFromNB(nb) == want {
			n++
		}
	}
	return n
}

func objectStorageBuckets(buckets []map[string]any) []lkeBucket {
	var out []lkeBucket
	for _, b := range buckets {
		region := mapString(b, "region")
		if region == "" {
			region = mapString(b, "cluster") // older API field
		}
		out = append(out, lkeBucket{
			Label:   mapString(b, "label"),
			Region:  region,
			Objects: mapInt(b, "objects"),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}

// ── map helpers (json.Number-aware) ──────────────────────────────────────────

func mapString(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func mapBool(m map[string]any, k string) bool {
	b, _ := m[k].(bool)
	return b
}

func mapInt(m map[string]any, k string) int {
	switch v := m[k].(type) {
	case json.Number:
		i, _ := v.Int64()
		return int(i)
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}

func mapUint(m map[string]any, k string) uint64 {
	if n := mapInt(m, k); n > 0 {
		return uint64(n)
	}
	return 0
}

func mapChild(m map[string]any, k string) map[string]any {
	c, _ := m[k].(map[string]any)
	return c
}

func mapSlice(m map[string]any, k string) []any {
	s, _ := m[k].([]any)
	return s
}

// mapStrings reads a []string from a generic []any value, skipping non-strings.
func mapStrings(m map[string]any, k string) []string {
	raw, ok := m[k].([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, e := range raw {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
