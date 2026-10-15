package evidencepack

import (
	"encoding/json"
	"fmt"
	"sort"
)

// This file holds the pure parsers for the supplemental kubectl evidence the
// `llz ci cis-evidence` command harvests. Keeping them here (not in the command)
// lets the list-counting / label-filtering logic be unit-tested against canned
// `kubectl get -o json` payloads.

// CountListItems returns the number of items in a `kubectl get … -o json` list.
func CountListItems(raw []byte) (int, error) {
	var doc struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return 0, fmt.Errorf("parse list: %w", err)
	}
	return len(doc.Items), nil
}

// RestrictedNamespaces returns the names of namespaces whose
// pod-security.kubernetes.io/enforce label is "restricted", sorted. The result
// is non-nil even when empty so callers can distinguish "collected, none" from
// "not collected" (nil).
func RestrictedNamespaces(raw []byte) ([]string, error) {
	var doc struct {
		Items []struct {
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse namespaces: %w", err)
	}
	out := []string{}
	for _, it := range doc.Items {
		if it.Metadata.Labels["pod-security.kubernetes.io/enforce"] == "restricted" {
			out = append(out, it.Metadata.Name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// DefaultStorageClassEncrypted reports whether the cluster's default
// StorageClass requests Linode block-storage encryption. It returns nil when no
// default StorageClass is present (so the pack records "not collected" rather
// than a misleading "no").
func DefaultStorageClassEncrypted(raw []byte) (*bool, error) {
	var doc struct {
		Items []struct {
			Metadata struct {
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
			Parameters map[string]string `json:"parameters"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse storageclasses: %w", err)
	}
	for _, it := range doc.Items {
		if it.Metadata.Annotations["storageclass.kubernetes.io/is-default-class"] == "true" {
			enc := it.Parameters["linodebs.csi.linode.com/encrypted"] == "true"
			return &enc, nil
		}
	}
	return nil, nil
}

// ComplianceScannerState reads trivy-operator's `trivy-operator-config`
// ConfigMap (`kubectl get configmap -o json`) and reports whether its
// cluster-compliance scanner is on. Only the literal "true" is enabled: apl-core
// renders the value from operator.clusterComplianceEnabled, and an absent or
// unparseable key is not evidence that anything is being scored.
func ComplianceScannerState(raw []byte) (string, error) {
	var doc struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return ScannerNotCollected, fmt.Errorf("parse trivy-operator config: %w", err)
	}
	if doc.Data[complianceEnabledKey] == "true" {
		return ScannerEnabled, nil
	}
	return ScannerDisabled, nil
}

// trivy-operator's config ConfigMap and the key apl-core sets to "false"
// (values/trivy-operator/trivy-operator.gotmpl: operator.clusterComplianceEnabled).
const (
	trivyOperatorNamespace = "trivy-operator"
	trivyOperatorConfigMap = "trivy-operator-config"
	complianceEnabledKey   = "OPERATOR_CLUSTER_COMPLIANCE_ENABLED"
)
