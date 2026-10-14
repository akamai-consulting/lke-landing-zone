package kube

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// SecretField pulls a base64 Secret value out of `kubectl get -o json`.
//
// IT MOVED HERE BECAUSE IT HAD TWO OWNERS AND BELONGED TO NEITHER. It was defined
// in the obj-roundtrip assertion and imported by cmd/llz purely to satisfy
// internal/objenc's SecretField dependency — so extracting the assertion would
// have made one extension's private helper part of another's wiring. Decoding a
// Secret's data field is not either extension's business; it is what this package
// is for.
//
// The error strings stay ESO-flavoured ("ESO has not materialized this consumer's
// credential") because that is the failure every caller actually hits, and a
// generic "key not found" would cost the reader the diagnosis. a base64 Secret value out of `kubectl get -o json`.
func SecretField(raw []byte, field string) (string, error) {
	var s struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("decoding the Secret: %w", err)
	}
	v, ok := s.Data[field]
	if !ok {
		return "", fmt.Errorf("the Secret has no %q key — ESO has not materialized this consumer's credential", field)
	}
	b, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return "", fmt.Errorf("the Secret's %q is not valid base64: %w", field, err)
	}
	if strings.TrimSpace(string(b)) == "" {
		return "", fmt.Errorf("the Secret's %q is empty", field)
	}
	return string(b), nil
}
