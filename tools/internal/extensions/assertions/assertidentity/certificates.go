package assertidentity

// ci_assert_certificates.go implements `llz ci assert-certificates` — the gate on
// cert-manager Certificate readiness and expiry.
//
// The signal already existed and nothing consumed it. The reconciler publishes
// llz_certificates_not_ready and LLZCertificatesNotReady alerts on it — but
// alert-eval is report-only and --strict ignores FIRING by design, so a
// Certificate stuck not-Ready reds nothing. Exactly the gap that left per-lane
// reconciler staleness unasserted: a correct gauge, a correct alert, and no gate.
//
// It reads the Certificates rather than the gauge, on purpose. The gauge is a
// COUNT: it tells you three are unhealthy, never which, and a gate whose failure
// message is "3" sends the operator to go and find them on a cluster that e2e
// tears down minutes later. The objects carry the name, the issuer's message and
// the expiry.
//
// NOT-READY AND EXPIRING-SOON ARE DIFFERENT FAILURES and are reported as such. A
// Certificate that never became Ready is a broken issuance — a wrong issuerRef, a
// failed ACME challenge, an unreachable CA. One that is Ready but expiring inside
// the renewal window is a broken RENEWAL, where issuance worked once and the
// controller has since stopped. The remedies share nothing.
//
// FAIL-CLOSED, including on finding zero Certificates: this platform issues its
// own CA chain (llz-client-ca, llz-serving-ca, the OpenBao bootstrap CA), so a
// cluster with none is one where cert-manager never reconciled anything, not a
// cluster that happens not to use TLS.

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// certVerdict is one Certificate's outcome.
type certVerdict struct {
	Name    string // namespace/name
	Expiry  time.Time
	Left    time.Duration
	FailWhy string
}

// certCondition / certStatus / certItem are the subset of a cert-manager
// Certificate this gate reads.
type certCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

type certStatus struct {
	NotAfter   string          `json:"notAfter"`
	Conditions []certCondition `json:"conditions"`
}

type certItem struct {
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Status certStatus `json:"status"`
}

// evalCertificates judges a cert-manager Certificate list. Pure.
func evalCertificates(raw []byte, now time.Time, minLeft time.Duration) ([]certVerdict, error) {
	var list struct {
		Items []certItem `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("decoding the Certificate list: %w", err)
	}

	out := make([]certVerdict, 0, len(list.Items))
	for _, it := range list.Items {
		v := certVerdict{Name: it.Metadata.Namespace + "/" + it.Metadata.Name}

		ready, why := false, ""
		for _, c := range it.Status.Conditions {
			if c.Type == "Ready" {
				ready = c.Status == "True"
				why = strings.TrimSpace(c.Reason + " " + c.Message)
			}
		}
		switch {
		case !ready:
			// Broken ISSUANCE. Carry cert-manager's own reason: it names the wrong
			// issuerRef or the failing challenge, which is the whole diagnosis.
			v.FailWhy = "not Ready — " + firstNonEmpty(why,
				"no Ready condition at all, so cert-manager has never acted on it (check the issuerRef exists and the controller is running)")
		case strings.TrimSpace(it.Status.NotAfter) == "":
			v.FailWhy = "Ready but carries no status.notAfter — its remaining life cannot be established, which is as blind as an expired one"
		default:
			t, err := time.Parse(time.RFC3339, it.Status.NotAfter)
			if err != nil {
				v.FailWhy = fmt.Sprintf("status.notAfter %q is not RFC3339: %v", it.Status.NotAfter, err)
				break
			}
			v.Expiry, v.Left = t, t.Sub(now)
			if v.Left < minLeft {
				// Broken RENEWAL: issuance worked once and has since stopped.
				v.FailWhy = fmt.Sprintf("Ready but expires in %s, inside the %s floor — issuance worked once and "+
					"renewal has stopped. Check the cert-manager controller and the issuer, not the Certificate spec",
					v.Left.Round(time.Hour), minLeft)
			}
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// readCertificates lists cert-manager Certificates cluster-wide. Seamed.
var readCertificates = func() ([]byte, error) {
	return caps.Exec("kubectl", "get", "certificates.cert-manager.io", "--all-namespaces", "-o", "json")
}

func failedCerts(vs []certVerdict) []string {
	var out []string
	for _, v := range vs {
		if v.FailWhy != "" {
			out = append(out, v.Name)
		}
	}
	return out
}

func runCIAssertCertificates(minDays int, settle, interval time.Duration) error {
	fmt.Println("## Certificate readiness assertion")
	minLeft := time.Duration(minDays) * 24 * time.Hour

	var last []certVerdict
	var lastErr error
	deadline := time.Now().Add(settle)
	for attempt := 1; ; attempt++ {
		raw, err := readCertificates()
		if err != nil {
			lastErr = fmt.Errorf("listing Certificates: %w", err)
		} else {
			last, lastErr = evalCertificates(raw, time.Now(), minLeft)
		}
		if lastErr == nil && len(failedCerts(last)) == 0 && len(last) > 0 {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		fmt.Printf("attempt %d: certificates not all healthy yet — retrying in %s\n", attempt, interval)
		time.Sleep(interval)
	}

	if lastErr != nil {
		fmt.Fprintf(os.Stderr, "::error::%v\n", lastErr)
		return lastErr
	}
	// Zero is a failure: this platform issues its own CA chain.
	if len(last) == 0 {
		fmt.Fprintln(os.Stderr, "::error::no cert-manager Certificates found — refusing to pass having examined nothing")
		return fmt.Errorf("no cert-manager Certificates found — this platform issues its own CA chain, so none means cert-manager never reconciled")
	}

	for _, v := range last {
		if v.FailWhy != "" {
			fmt.Printf("FAIL: %s — %s\n", v.Name, v.FailWhy)
		} else {
			fmt.Printf("OK: %s — %s left\n", v.Name, v.Left.Round(time.Hour))
		}
	}
	if bad := failedCerts(last); len(bad) > 0 {
		fmt.Fprintf(os.Stderr, "::error::unhealthy certificate(s): %s\n", strings.Join(bad, ", "))
		return fmt.Errorf("unhealthy certificate(s): %s", strings.Join(bad, ", "))
	}
	fmt.Printf("All %d Certificate(s) are Ready with more than %d days left.\n", len(last), minDays)
	return nil
}
