package promwire

// Package promwire decodes Prometheus instant-query responses.
//
// It exists because TWO assertion extensions need the same parse and a third
// will: `assert-reconciler` reads the reconciler's lane gauges,
// `assert-rotation-health` reads credential ages, and `assert-observability`
// (still in package main) is built entirely on this shape.
//
// THE PROPERTY WORTH PROTECTING is the one both decoders below spell out: a query
// FAILURE and an EMPTY RESULT are different answers. "We could not ask" must never
// read as "the lane is dead" — that sends an operator after a healthy reconciler,
// which is the same class of bug internal/kubectlprobe exists to prevent for
// kubectl. One copy, so the distinction cannot be lost in one caller and kept in
// another.

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// VectorByLabel parses an instant-query response into label-value → sample
// value, keyed by the named label. Like Scalar it separates a query failure
// from an empty result — "we could not ask" must never read as "the lane is
// dead", which would send an operator after a healthy reconciler.
func VectorByLabel(raw []byte, label string) (map[string]float64, error) {
	var resp struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Value  []any             `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("unparseable Prometheus response: %w", err)
	}
	if resp.Status != "success" {
		detail := resp.Error
		if detail == "" {
			detail = "status=" + resp.Status
		}
		return nil, fmt.Errorf("prometheus returned an error: %s", detail)
	}
	out := map[string]float64{}
	for _, r := range resp.Data.Result {
		key := r.Metric[label]
		if key == "" || len(r.Value) != 2 {
			continue
		}
		str, ok := r.Value[1].(string)
		if !ok {
			continue
		}
		if f, err := strconv.ParseFloat(str, 64); err == nil {
			out[key] = f
		}
	}
	return out, nil
}

// Scalar parses an instant-query response, returning the first sample's value
// and whether ANY series was returned. A non-success status or unparseable body
// is treated as no series (the caller decides how to classify an absent gauge).
// Scalar returns the first sample's value, whether a series came back, and
// whether the QUERY ITSELF failed.
//
// The third return is the point. This used to fold four different conditions
// into hasSeries=false — unparseable JSON, status!="success" (Prometheus
// returned an error), a genuinely absent series, and a malformed value tuple —
// and evalReconcilerGauge then reported all four with its absentWhy string,
// e.g. "the reconciler isn't reporting (pod down / not scraped)".
//
// So a Prometheus hiccup failed a GATING e2e assert while blaming the
// reconciler: the operator goes and inspects a pod that is perfectly healthy.
// Same shape as the OpenBao false-SEALED report and alert-eval's vacuous
// --strict pass — "could not ask" is not an answer about the subject.
func Scalar(raw []byte) (value float64, hasSeries bool, queryErr error) {
	var resp struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			Result []struct {
				Value []any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return 0, false, fmt.Errorf("unparseable Prometheus response: %w", err)
	}
	if resp.Status != "success" {
		detail := resp.Error
		if detail == "" {
			detail = "status=" + resp.Status
		}
		return 0, false, fmt.Errorf("prometheus returned an error: %s", detail)
	}
	if len(resp.Data.Result) == 0 {
		return 0, false, nil // a real answer: the series genuinely is not there
	}
	v := resp.Data.Result[0].Value
	if len(v) != 2 {
		return 0, false, fmt.Errorf("malformed sample: expected [ts, value], got %d element(s)", len(v))
	}
	str, ok := v[1].(string)
	if !ok {
		return 0, false, fmt.Errorf("malformed sample: value is %T, not a string", v[1])
	}
	f, perr := strconv.ParseFloat(str, 64)
	if perr != nil {
		return 0, false, fmt.Errorf("malformed sample: value %q is not numeric", str)
	}
	return f, true, nil
}
