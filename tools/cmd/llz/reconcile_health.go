// Observe-reconciler health gauges (see docs/designs/kube-native-reconciler.md).
//
// These surface, as continuously-scraped metrics, the readiness signals the daily
// hosted-runner `scheduled-checks` jobs port-forward for today — so the cluster
// self-reports and (Phase 3) those CIDR-fragile external checks can be demoted to
// belt-and-suspenders. All three read the Kubernetes API only (no OpenBao network
// or auth), and the ESO/Certificate checks reuse the SAME tested predicate
// `llz ci health` uses (internal/health.ClassifyReady over a resource's Ready
// condition). OpenBao seal is approximated by pod readiness — a sealed OpenBao pod
// fails its readiness probe — which needs no OpenBao wiring; the precise
// /v1/sys/seal-status probe and the OpenBao-authenticated credential-age gauges are
// a follow-up that adds the OpenBao egress/auth once.
package main

import (
	"context"
	"fmt"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/health"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/metrics"
)

const (
	esoStorePath = "/apis/external-secrets.io/v1/clustersecretstores/openbao"
	certsPath    = "/apis/cert-manager.io/v1/certificates"
	openbaoPods  = "/api/v1/namespaces/llz-openbao/pods?labelSelector=app.kubernetes.io/name%3Dopenbao"
)

// sampleHealth publishes the ESO-store / Certificate / OpenBao-pod readiness
// gauges. It returns an error only on an unexpected API failure (so observe
// records up=0); a not-ready resource is a valid observation. A 404 (CRD/resource
// absent, pre-bootstrap) is treated as "not present", never an error.
func sampleHealth(ctx context.Context, client nodeGetter, reg *metrics.Registry) error {
	if err := sampleESOStore(ctx, client, reg); err != nil {
		return err
	}
	if err := sampleCertificates(ctx, client, reg); err != nil {
		return err
	}
	return sampleOpenBaoPods(ctx, client, reg)
}

// sampleESOStore reports whether the openbao ClusterSecretStore is Ready.
func sampleESOStore(ctx context.Context, client nodeGetter, reg *metrics.Registry) error {
	obj, status, err := client.GetJSON(ctx, esoStorePath)
	if err != nil {
		return err
	}
	ready := 0.0
	if status == 200 && obj != nil {
		s, r, m := readyCondition(obj)
		if cat, _ := health.ClassifyReady("ClusterSecretStore", "openbao", s, r, m, false, nil); cat == health.CatOK {
			ready = 1
		}
	} else if status != 404 && (status < 200 || status >= 300) {
		return fmt.Errorf("GET clustersecretstore openbao: status %d", status)
	}
	reg.SetGauge("llz_eso_store_ready", "1 if the openbao ClusterSecretStore is Ready",
		map[string]string{"store": "openbao"}, ready)
	return nil
}

// sampleCertificates counts cert-manager Certificates not Ready (across all
// namespaces) alongside the total.
func sampleCertificates(ctx context.Context, client nodeGetter, reg *metrics.Registry) error {
	obj, status, err := client.GetJSON(ctx, certsPath)
	if err != nil {
		return err
	}
	if status == 404 {
		reg.SetGauge("llz_certificates_total", "count of cert-manager Certificates", nil, 0)
		reg.SetGauge("llz_certificates_not_ready", "count of cert-manager Certificates not Ready", nil, 0)
		return nil
	}
	if status < 200 || status >= 300 || obj == nil {
		return fmt.Errorf("GET certificates: status %d", status)
	}
	items, _ := obj["items"].([]any)
	total, notReady := 0, 0
	for _, it := range items {
		c, ok := it.(map[string]any)
		if !ok {
			continue
		}
		total++
		s, r, m := readyCondition(c)
		if cat, _ := health.ClassifyReady("Certificate", certName(c), s, r, m, false, nil); cat != health.CatOK {
			notReady++
		}
	}
	reg.SetGauge("llz_certificates_total", "count of cert-manager Certificates", nil, float64(total))
	reg.SetGauge("llz_certificates_not_ready", "count of cert-manager Certificates not Ready", nil, float64(notReady))
	return nil
}

// sampleOpenBaoPods reports the ready/total OpenBao pods — a sealed pod reads
// NotReady, so this is the OpenBao availability signal without OpenBao wiring.
func sampleOpenBaoPods(ctx context.Context, client nodeGetter, reg *metrics.Registry) error {
	obj, status, err := client.GetJSON(ctx, openbaoPods)
	if err != nil {
		return err
	}
	if status == 404 {
		return nil // namespace/pods not present yet
	}
	if status < 200 || status >= 300 || obj == nil {
		return fmt.Errorf("GET openbao pods: status %d", status)
	}
	items, _ := obj["items"].([]any)
	total, ready := 0, 0
	for _, it := range items {
		p, ok := it.(map[string]any)
		if !ok {
			continue
		}
		total++
		if s, _, _ := readyCondition(p); s == "True" {
			ready++
		}
	}
	reg.SetGauge("llz_openbao_pods_total", "count of OpenBao pods", nil, float64(total))
	reg.SetGauge("llz_openbao_pods_ready", "count of OpenBao pods passing their readiness probe (a sealed pod is NotReady)", nil, float64(ready))
	return nil
}

// readyCondition returns the status/reason/message of a resource's Ready
// condition (or empty strings if absent).
func readyCondition(obj map[string]any) (status, reason, msg string) {
	st, _ := obj["status"].(map[string]any)
	conds, _ := st["conditions"].([]any)
	for _, c := range conds {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if cm["type"] == "Ready" {
			s, _ := cm["status"].(string)
			r, _ := cm["reason"].(string)
			m, _ := cm["message"].(string)
			return s, r, m
		}
	}
	return "", "", ""
}

// certName returns a namespace/name key for a Certificate object.
func certName(c map[string]any) string {
	meta, _ := c["metadata"].(map[string]any)
	ns, _ := meta["namespace"].(string)
	name, _ := meta["name"].(string)
	if ns == "" {
		return name
	}
	return ns + "/" + name
}
