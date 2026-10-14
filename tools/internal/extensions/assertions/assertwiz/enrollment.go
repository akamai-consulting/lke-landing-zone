package assertwiz

// enrollment.go — `llz ci assert-wiz-enrollment`.
//
// UNVERIFIED DELIVERY, asserted at the consumer. The chain is
//
//	OpenBao (team subtree) → ESO → Secret wiz-api-token → registration Job →
//	Wiz API accepts the cluster → Secret wiz-connector → broker tunnel
//
// and every link can be broken while the Application reads Synced/Healthy: an
// unseeded path leaves the ExternalSecret SecretSyncedError (the Application is
// still Synced), a Job that 401s is a hook Argo retries quietly, and a broker
// whose tunnel never came up is a Running pod. So the gate reads the four objects
// the chain PRODUCES and judges each one:
//
//  1. ExternalSecret wiz-api-token is Ready (ESO could read the path).
//  2. Secret wiz-api-token carries non-empty clientId AND clientToken.
//  3. Secret wiz-connector's connectorData is no longer the `{}` the chart seeds
//     it with — the registration Job only rewrites it after the Wiz API accepted
//     the credential and created the connector. GROUND TRUTH from the tenant's
//     answer, not a proxy inferred from our own config.
//  4. Deployment wiz-agent (the broker) has a Ready replica — its readiness probe
//     is the broker's own /ready, i.e. the tunnel to Wiz is up.
//
// FAIL CLOSED. An object that could not be read (unreachable, unparseable) is a
// failure distinct from one that is absent, and a run that examined nothing never
// passes. The verdict names what IS present, so the reader is not sent to a live
// cluster to learn which link broke.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghaout"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

// readState is how one object read came back.
type readState int

const (
	readOK      readState = iota // the object was returned
	readAbsent                   // kubectl answered: it is not there
	readUnknown                  // no answer (unreachable, timeout, refused)
)

// observed is one object read: its state and raw JSON (readOK only), plus the
// error text for a readUnknown so the verdict can print it.
type observed struct {
	state  readState
	raw    []byte
	detail string
}

// observation is everything the gate reads, keyed by what it is.
type observation struct {
	ExternalSecret observed
	Credential     observed
	Registration   observed
	Broker         observed
}

// check is one link's verdict.
type check struct {
	Name string
	OK   bool
	// Final marks a failure no amount of waiting fixes on its own within the
	// settle budget (a credential that is not seeded, a wrong path) as distinct
	// from a link that may still be converging. Used only to stop polling early.
	Final  bool
	Detail string
}

// readObject is the transport seam: one `kubectl -n wiz get <kind> <name> -o json`
// through the declared cluster-read handle. Tests replace it.
var readObject = func(kind, name string) observed {
	b, err := capability.MustCluster(Extension().MustBinding("wiz-enrollment")).Run(
		"-n", clusterspec.WizNamespace, "get", kind, name, "-o", "json")
	if err == nil {
		return observed{state: readOK, raw: b}
	}
	if kubectlprobe.ClassifyErr(err) == kubectlprobe.Absent {
		return observed{state: readAbsent}
	}
	return observed{state: readUnknown, detail: strings.TrimSpace(kubectlprobe.ErrText(err))}
}

func observe() observation {
	return observation{
		ExternalSecret: readObject("externalsecrets.external-secrets.io", clusterspec.WizCredentialSecret),
		Credential:     readObject("secret", clusterspec.WizCredentialSecret),
		Registration:   readObject("secret", clusterspec.WizRegistrationSecret()),
		Broker:         readObject("deployment", clusterspec.WizBrokerDeployment()),
	}
}

// evaluate judges an observation. Pure: every arm is testable without a cluster.
func evaluate(o observation) []check {
	return []check{
		evalExternalSecret(o.ExternalSecret),
		evalCredential(o.Credential),
		evalRegistration(o.Registration),
		evalBroker(o.Broker),
	}
}

func unreadable(name, what string, ob observed) check {
	return check{Name: name, Detail: fmt.Sprintf("could not read %s (%s) — this is NOT evidence it is absent", what, dashIfEmpty(ob.detail))}
}

func evalExternalSecret(ob observed) check {
	const name = "credential delivery (ExternalSecret)"
	what := "externalsecret " + clusterspec.WizNamespace + "/" + clusterspec.WizCredentialSecret
	switch ob.state {
	case readUnknown:
		return unreadable(name, what, ob)
	case readAbsent:
		return check{Name: name, Final: true, Detail: what + " does not exist — the component's bundle did not deploy (check the llz-wiz Application and that `llz render` was committed for this env)"}
	}
	var es struct {
		Spec struct {
			Data []struct {
				RemoteRef struct {
					Key string `json:"key"`
				} `json:"remoteRef"`
			} `json:"data"`
		} `json:"spec"`
		Status struct {
			Conditions []struct {
				Type    string `json:"type"`
				Status  string `json:"status"`
				Reason  string `json:"reason"`
				Message string `json:"message"`
			} `json:"conditions"`
		} `json:"status"`
	}
	if err := json.Unmarshal(ob.raw, &es); err != nil {
		return check{Name: name, Detail: fmt.Sprintf("could not parse %s: %v", what, err)}
	}
	key := ""
	if len(es.Spec.Data) > 0 {
		key = es.Spec.Data[0].RemoteRef.Key
	}
	for _, c := range es.Status.Conditions {
		if c.Type != "Ready" {
			continue
		}
		if c.Status == "True" {
			return check{Name: name, OK: true, Detail: fmt.Sprintf("Ready, reading OpenBao key %q", key)}
		}
		d := fmt.Sprintf("NOT Ready reading OpenBao key %q: %s %s", key, c.Reason, strings.TrimSpace(c.Message))
		if strings.Contains(key, "TEAM_SUBTREE") {
			return check{Name: name, Final: true, Detail: d + " — the key is still the base placeholder, so the per-env patch was not applied (re-run `llz render` and commit)"}
		}
		return check{Name: name, Final: true, Detail: d + " — seed clientId + clientToken at secret/" + key + " (llz openbao set … --yes) and confirm the path is under a declared team subtree"}
	}
	return check{Name: name, Detail: fmt.Sprintf("%s has no Ready condition yet (reading %q) — ESO has not reconciled it", what, key)}
}

func evalCredential(ob observed) check {
	const name = "credential material (Secret)"
	what := "secret " + clusterspec.WizNamespace + "/" + clusterspec.WizCredentialSecret
	switch ob.state {
	case readUnknown:
		return unreadable(name, what, ob)
	case readAbsent:
		return check{Name: name, Detail: what + " does not exist — ESO has not materialised it (see the ExternalSecret verdict)"}
	}
	data, err := secretData(ob.raw)
	if err != nil {
		return check{Name: name, Detail: fmt.Sprintf("could not parse %s: %v", what, err)}
	}
	var missing, present []string
	for _, f := range clusterspec.WizCredentialFields {
		if len(data[f]) == 0 {
			missing = append(missing, f)
		} else {
			present = append(present, f)
		}
	}
	if len(missing) > 0 {
		return check{Name: name, Final: true, Detail: fmt.Sprintf("%s is missing or empty: %s (present: %s; keys in the Secret: %s) — the chart reads exactly %s",
			what, strings.Join(missing, ", "), dashIfEmpty(strings.Join(present, ", ")), dashIfEmpty(strings.Join(sortedKeys(data), ", ")),
			strings.Join(clusterspec.WizCredentialFields, " + "))}
	}
	return check{Name: name, OK: true, Detail: "clientId and clientToken present (values not printed)"}
}

func evalRegistration(ob observed) check {
	const name = "connector registration (Wiz API accepted the cluster)"
	what := "secret " + clusterspec.WizNamespace + "/" + clusterspec.WizRegistrationSecret()
	switch ob.state {
	case readUnknown:
		return unreadable(name, what, ob)
	case readAbsent:
		return check{Name: name, Detail: what + " does not exist — the chart's registration hook has not run (check the llz-wiz-connector Application's last sync)"}
	}
	data, err := secretData(ob.raw)
	if err != nil {
		return check{Name: name, Detail: fmt.Sprintf("could not parse %s: %v", what, err)}
	}
	cd := strings.TrimSpace(string(data["connectorData"]))
	if cd == "" || cd == "{}" || cd == "null" {
		return check{Name: name, Detail: what + " still holds the chart's empty placeholder — the registration Job has not completed against the Wiz API (`kubectl -n " + clusterspec.WizNamespace + " logs -l wiz.io/component=create-kubernetes-connector`: a 401 means a wrong clientId/clientToken, a timeout means egress to the Wiz SaaS is blocked)"}
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(cd), &rec); err != nil || len(rec) == 0 {
		return check{Name: name, Detail: what + " connectorData is not a non-empty JSON object — cannot tell whether registration succeeded"}
	}
	return check{Name: name, OK: true, Detail: fmt.Sprintf("connector record written by the registration Job (%d fields)", len(rec))}
}

func evalBroker(ob observed) check {
	const name = "broker tunnel (Deployment Ready)"
	what := "deployment " + clusterspec.WizNamespace + "/" + clusterspec.WizBrokerDeployment()
	switch ob.state {
	case readUnknown:
		return unreadable(name, what, ob)
	case readAbsent:
		return check{Name: name, Detail: what + " does not exist — the chart's connector/broker is not deployed (is wiz-kubernetes-connector.enabled still true on llz-wiz-connector?)"}
	}
	var d struct {
		Status struct {
			Replicas      int `json:"replicas"`
			ReadyReplicas int `json:"readyReplicas"`
		} `json:"status"`
	}
	if err := json.Unmarshal(ob.raw, &d); err != nil {
		return check{Name: name, Detail: fmt.Sprintf("could not parse %s: %v", what, err)}
	}
	if d.Status.ReadyReplicas < 1 {
		return check{Name: name, Detail: fmt.Sprintf("%s has %d/%d Ready replicas — the broker's /ready probe fails until its tunnel to Wiz is up (check egress 443 and `kubectl -n %s logs deploy/%s`)",
			what, d.Status.ReadyReplicas, d.Status.Replicas, clusterspec.WizNamespace, clusterspec.WizBrokerDeployment())}
	}
	return check{Name: name, OK: true, Detail: fmt.Sprintf("%d/%d Ready", d.Status.ReadyReplicas, d.Status.Replicas)}
}

// secretData decodes a Secret's .data into raw bytes per key.
func secretData(raw []byte) (map[string][]byte, error) {
	var s struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	out := make(map[string][]byte, len(s.Data))
	for k, v := range s.Data {
		b, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			return nil, fmt.Errorf("key %s is not valid base64", k)
		}
		out[k] = b
	}
	return out, nil
}

func sortedKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func dashIfEmpty(s string) string {
	if strings.TrimSpace(s) == "" {
		return "none"
	}
	return s
}

// allOK reports whether every check passed. An EMPTY set is not a pass.
func allOK(cs []check) bool {
	if len(cs) == 0 {
		return false
	}
	for _, c := range cs {
		if !c.OK {
			return false
		}
	}
	return true
}

// anyFinal reports whether a failure is one waiting will not fix.
func anyFinal(cs []check) bool {
	for _, c := range cs {
		if !c.OK && c.Final {
			return true
		}
	}
	return false
}

// wizExpected answers, from the SPEC, whether this env should be enrolled. The
// cluster is the thing under test, so it cannot also decide whether to test: an
// enabled component whose bundle never deployed must FAIL, not skip.
var wizExpected = func(region string) (bool, string) {
	if region == "" {
		return false, "no --region was given, so the spec could not say whether Wiz enrollment is expected"
	}
	lz, err := clusterspec.LoadInstance(".")
	if err != nil {
		return false, fmt.Sprintf("the instance spec could not be read (%v)", err)
	}
	e, ok := lz.Env(region)
	if !ok {
		return false, fmt.Sprintf("%q is not a deployment in the instance spec", region)
	}
	if clusterspec.ComponentEnabled(e.Components, "wiz") {
		return true, fmt.Sprintf("spec.components.wiz IS enabled for %s", region)
	}
	return false, fmt.Sprintf("spec.components.wiz is disabled for %s", region)
}

var sleep = time.Sleep

// setResult publishes the verdict as the step output `result` (pass | fail |
// skipped). A skip exits 0 like a pass, so a workflow recording this gate's
// outcome (the evidence pack's --gate) must read THIS, not the step's outcome —
// or every instance without Wiz would file a skip as a passing enrollment.
var setResult = func(r string) { _ = ghaout.Append("GITHUB_OUTPUT", "result="+r) }

// runAssertWizEnrollment polls until every link passes, a final failure is seen,
// or the settle budget runs out.
func runAssertWizEnrollment(region string, settle, interval time.Duration) error {
	expected, why := wizExpected(region)
	if !expected {
		if region != "" && !strings.Contains(why, "could not") && !strings.Contains(why, "not a deployment") {
			fmt.Printf("::notice::assert-wiz-enrollment: SKIPPED — %s. This gate checked NOTHING; it is not a pass.\n", why)
			setResult("skipped")
			return nil
		}
		// No spec answer: fall back to the cluster, but say so. A bundle that is
		// there gets judged; a namespace with nothing in it is reported as a skip,
		// loudly, for ad-hoc runs outside an instance checkout.
		if o := observe(); o.ExternalSecret.state == readAbsent && o.Broker.state == readAbsent {
			fmt.Printf("::notice::assert-wiz-enrollment: SKIPPED — %s, and no Wiz bundle is deployed in namespace %s. This gate checked NOTHING; it is not a pass.\n", why, clusterspec.WizNamespace)
			setResult("skipped")
			return nil
		}
	}

	deadline := time.Now().Add(settle)
	var cs []check
	for {
		cs = evaluate(observe())
		if allOK(cs) || anyFinal(cs) || !time.Now().Before(deadline) {
			break
		}
		sleep(interval)
	}
	for _, c := range cs {
		mark := "ok  "
		if !c.OK {
			mark = "FAIL"
		}
		fmt.Printf("  [%s] %s: %s\n", mark, c.Name, c.Detail)
	}
	if allOK(cs) {
		fmt.Printf("assert-wiz-enrollment: cluster is enrolled in Wiz (%s).\n", why)
		setResult("pass")
		return nil
	}
	var failed []string
	for _, c := range cs {
		if !c.OK {
			failed = append(failed, c.Name)
		}
	}
	setResult("fail")
	fmt.Fprintf(os.Stderr, "::error::assert-wiz-enrollment: Wiz enrollment is broken — failing link(s): %s. Checked in order: OpenBao path → ExternalSecret → Secret keys → registration Job (Wiz API) → broker tunnel.\n", strings.Join(failed, "; "))
	return fmt.Errorf("assert-wiz-enrollment: %d of %d enrollment links failing", len(failed), len(cs))
}
