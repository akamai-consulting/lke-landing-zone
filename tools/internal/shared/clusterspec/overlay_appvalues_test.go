package clusterspec

// The property under test throughout is the one that decides whether this channel
// is usable at all: LLZ and apl-operator co-write env/apps/<name>.yaml, so every
// write LLZ makes must be one apl-operator's next rewrite cannot turn into a
// permanent push-fight. "changed" is that contract, and it is semantic.

import (
	"strings"
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// aplOperatorLoki is the shape apl-operator actually leaves behind: 4-space
// indent, its own keys, its own ordering, and the values LLZ asserts already
// present. Written as a literal rather than round-tripped through the renderer
// because the point is that it does NOT look like LLZ's output.
//
// It carries `persistence.claims` as a SEQUENCE deliberately. That is the chart's
// real shape (size and storageClass live on each claim, not one level up), and a
// list is the value type most likely to be compared wrongly — an element-wise
// merge, or a comparison that never matches, turns into a push on every
// reconcile pass forever. The push-fight test below is the one that catches it.
const aplOperatorLoki = `apiVersion: apl.akamai.com/v1
kind: AplApp
metadata:
    name: loki
spec:
    enabled: true
    _rawValues:
        ingester:
            persistence:
                claims:
                    - accessModes:
                        - ReadWriteOnce
                      name: data
                      size: 5Gi
                      storageClass: block-storage-retain
                enabled: true
            resources:
                limits:
                    cpu: "1"
                    memory: 3Gi
                requests:
                    cpu: 100m
                    memory: 512Mi
`

func lokiWant(t *testing.T) AppOverlay {
	t.Helper()
	rv, err := AppRawValues([]byte(RenderAppValuesOverlayShared()))
	if err != nil {
		t.Fatalf("parse rendered appvalues: %v", err)
	}
	on := true
	return AppOverlay{Enabled: &on, RawValues: rv["loki"]}
}

// THE PUSH-FIGHT TEST. apl-operator's copy already says everything LLZ wants to
// say, in apl-operator's formatting. A byte comparison calls that a difference;
// this must not. If it ever does, the reconciler commits to the machine branch on
// every pass, forever, against a writer that reformats it back every time.
func TestSetAppSpecDoesNotPushWhenAplOperatorAlreadyAgrees(t *testing.T) {
	updated, changed, err := SetAppSpec([]byte(aplOperatorLoki), lokiWant(t))
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Errorf("changed=true against a file that already carries every asserted value — "+
			"the reconciler would push on every pass. Got:\n%s", updated)
	}
	if string(updated) != aplOperatorLoki {
		t.Errorf("an unchanged result must be the INPUT bytes, not a re-marshal:\n%s", updated)
	}
}

// LLZ ASSERTS A SUBSET. apl-operator owns keys LLZ has never heard of; they are
// not differences, and they must survive the merge. A guard that treated them as
// drift would either churn or (worse) delete them.
func TestSetAppSpecPreservesKeysLLZDoesNotAssert(t *testing.T) {
	current := strings.Replace(aplOperatorLoki,
		"    _rawValues:\n",
		"    autoscaling:\n        enabled: false\n    _rawValues:\n        replicas: 3\n", 1)
	updated, changed, err := SetAppSpec([]byte(current), lokiWant(t))
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Errorf("apl-operator's own keys were read as drift:\n%s", updated)
	}
	for _, keep := range []string{"autoscaling", "replicas"} {
		if !strings.Contains(string(updated), keep) {
			t.Errorf("%q was dropped — LLZ must never remove a key it does not assert:\n%s", keep, updated)
		}
	}
}

// THE CASE THE WHOLE CHANNEL EXISTS FOR: apl-core's default ingester limit, which
// OOMKills mid-WAL-replay. LLZ must see it as a difference and correct it.
func TestSetAppSpecCorrectsTheAplCoreIngesterDefault(t *testing.T) {
	current := strings.Replace(aplOperatorLoki, "memory: 3Gi", "memory: 1Gi", 1)
	updated, changed, err := SetAppSpec([]byte(current), lokiWant(t))
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("a 1Gi ingester limit was accepted as already correct — this is the 16-day OOM crashloop")
	}
	var doc struct {
		Spec struct {
			RawValues struct {
				Ingester struct {
					Resources struct {
						Limits map[string]string `yaml:"limits"`
					} `yaml:"resources"`
				} `yaml:"ingester"`
			} `yaml:"_rawValues"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(updated, &doc); err != nil {
		t.Fatal(err)
	}
	if got := doc.Spec.RawValues.Ingester.Resources.Limits["memory"]; got != LokiWALReplayMemoryLimit {
		t.Errorf("ingester memory limit = %q, want %q", got, LokiWALReplayMemoryLimit)
	}
}

// ONE FILE, ONE WRITE. Both overlay sources target env/apps/<name>.yaml, so they
// are composed before the write. This asserts the composition rather than the
// write, because the failure it prevents — the second writer discarding the
// first — is invisible in the single-source case that every other test covers.
func TestAppOverlaysComposesBothSourcesForOneApp(t *testing.T) {
	apps := []byte("apps:\n  loki:\n    enabled: true\n")
	got, err := AppOverlays(apps, []byte(RenderAppValuesOverlayShared()))
	if err != nil {
		t.Fatal(err)
	}
	loki := got["loki"]
	if loki.Enabled == nil || !*loki.Enabled {
		t.Error("loki lost its enabled toggle when values were composed in")
	}
	if len(loki.RawValues) == 0 {
		t.Error("loki lost its _rawValues when the toggle was composed in")
	}
}

// ABSENT IS NOT A DEFAULT. An app named by only one source must carry only that
// half — writing `enabled: false` onto an app LLZ merely has values for, or
// blanking the values of an app it merely toggles, would override apl-core.
func TestAppOverlaysLeavesTheUnnamedHalfNil(t *testing.T) {
	// An app the values side says nothing about, DERIVED rather than named: this
	// test used to pick harbor, and then harbor gained an appvalues entry and the
	// test failed for a reason that had nothing to do with what it asserts.
	values, err := AppRawValues([]byte(RenderAppValuesOverlayShared()))
	if err != nil {
		t.Fatal(err)
	}
	toggleOnly := ""
	for _, candidate := range []string{"grafana", "alertmanager", "prometheus", "otel", "gitea"} {
		if _, named := values[candidate]; !named {
			toggleOnly = candidate
			break
		}
	}
	if toggleOnly == "" {
		t.Skip("every candidate app now has an appvalues entry — nothing to test the absent half with")
	}

	got, err := AppOverlays([]byte("apps:\n  "+toggleOnly+":\n    enabled: true\n"),
		[]byte(RenderAppValuesOverlayShared()))
	if err != nil {
		t.Fatal(err)
	}
	if got[toggleOnly].RawValues != nil {
		t.Errorf("%s has no appvalues entry but got a non-nil RawValues", toggleOnly)
	}
	// argocd is a CORE app: it has no `enabled` key in apl-core's schema at all,
	// so LLZ asserting one would be writing a field the CR does not have.
	if got["argocd"].Enabled != nil {
		t.Error("argocd is values-only, but an Enabled was composed in — argocd is a core app with no enabled key")
	}
	if len(got["argocd"].RawValues) == 0 {
		t.Error("argocd lost its health customizations")
	}
}

// FAIL LOUD, NOT EMPTY. A malformed source must be an error the reconciler
// returns, never an empty desired-state map that silently asserts nothing — that
// reads, from the cluster, exactly like the pre-fix world.
func TestAppRawValuesRejectsMalformedSource(t *testing.T) {
	if _, err := AppRawValues([]byte("apps: [this is a list\n")); err == nil {
		t.Error("malformed appvalues parsed clean — an unparseable source must not degrade to 'no opinion'")
	}
}

// A rendered overlay must survive its own round trip, or the committed file and
// the reconciler's reading of it are two different documents.
func TestRenderedAppValuesRoundTripsToWhatItAsserts(t *testing.T) {
	rv, err := AppRawValues([]byte(RenderAppValuesOverlayShared()))
	if err != nil {
		t.Fatal(err)
	}
	want := aplAppRawValues()
	if len(rv) != len(want) {
		t.Fatalf("round trip carried %d apps, renderer declared %d", len(rv), len(want))
	}
	for app := range want {
		if len(rv[app]) == 0 {
			t.Errorf("app %q lost its _rawValues in the round trip", app)
		}
	}
}

// ── the shapes a hand-edited or apl-operator-rewritten overlay can arrive in ──
//
// The overlay is rendered, but it is READ from a git branch two writers share, so
// the parser meets documents this repo did not produce. Each shape below must
// degrade to "LLZ asserts nothing about that app" rather than to a panic or, far
// worse, an empty assertion that reads as a deliberate blanking.

func TestAppRawValuesSkipsAppsWithNothingToAssert(t *testing.T) {
	for name, doc := range map[string]string{
		"app is a scalar, not a map": "apps:\n  loki: true\n",
		"app has no _rawValues":      "apps:\n  loki:\n    enabled: true\n",
		"_rawValues is empty":        "apps:\n  loki:\n    _rawValues: {}\n",
		"_rawValues is not a map":    "apps:\n  loki:\n    _rawValues: \"nope\"\n",
		"no apps key at all":         "other: 1\n",
		"apps is present but empty":  "apps: {}\n",
		"document is entirely empty": "",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := AppRawValues([]byte(doc))
			if err != nil {
				t.Fatalf("a well-formed document was rejected: %v", err)
			}
			if len(got) != 0 {
				t.Errorf("got %v — an app with nothing to assert must be ABSENT, because writing "+
					"an empty _rawValues onto apl-operator's CR is a change with no meaning", got)
			}
		})
	}
}

// A MALFORMED SOURCE ON EITHER SIDE MUST ERROR, never degrade to an empty desired
// state: an empty map writes no files, which from the cluster is indistinguishable
// from the pre-fix world where nothing was asserted at all.
func TestAppOverlaysPropagatesEitherParseError(t *testing.T) {
	const bad = "apps: [this is a list\n"
	if _, err := AppOverlays([]byte(bad), []byte(RenderAppValuesOverlayShared())); err == nil {
		t.Error("a malformed apps.yaml parsed clean")
	}
	if _, err := AppOverlays([]byte("apps:\n  loki:\n    enabled: true\n"), []byte(bad)); err == nil {
		t.Error("a malformed appvalues.yaml parsed clean")
	}
}

// SetAppSpec on a CR with no `spec` at all — the shape apl-operator leaves before
// it has populated an app. LLZ must create the block rather than fail.
func TestSetAppSpecCreatesAMissingSpecBlock(t *testing.T) {
	updated, changed, err := SetAppSpec([]byte("apiVersion: apl.akamai.com/v1\nkind: AplApp\n"),
		lokiWant(t))
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("a CR with no spec was reported as already correct")
	}
	if !strings.Contains(string(updated), "_rawValues") || !strings.Contains(string(updated), "enabled") {
		t.Errorf("the created spec block is missing what LLZ asserts:\n%s", updated)
	}
}

// A malformed CR on the machine branch must be an error the reconciler returns,
// not a silent overwrite of whatever is there.
func TestSetAppSpecRejectsAMalformedCR(t *testing.T) {
	if _, _, err := SetAppSpec([]byte("spec: [not a map\n"), lokiWant(t)); err == nil {
		t.Error("a malformed AplApp CR was accepted — the next write would clobber it")
	}
}

// A row asserting a SEQUENCE replaces it wholesale and reports the change.
// Merging two lists element-wise has no defensible meaning here, and pinning that
// stops a future "smart" merge from silently half-applying a list.
func TestAssertedSequencesAreReplacedWholesale(t *testing.T) {
	current := "spec:\n  _rawValues:\n    args: [a, b, c]\n"
	want := AppOverlay{RawValues: map[string]any{"args": []any{"x"}}}
	updated, changed, err := SetAppSpec([]byte(current), want)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("a differing sequence was read as unchanged")
	}
	if strings.Contains(string(updated), "- b") {
		t.Errorf("the sequence was merged rather than replaced:\n%s", updated)
	}
	// …and asserting the SAME sequence is not a change, or the reconciler pushes
	// on every pass for every list-valued key.
	if _, changed, err = SetAppSpec(updated, want); err != nil || changed {
		t.Errorf("re-asserting an identical sequence reported changed=%v (err=%v)", changed, err)
	}
}

// THE CHURN LOOP THE CLAIMS LIST REINTRODUCED. apl-operator re-emits
// `persistence.claims` with the chart's own defaults filled in — keys LLZ never
// mentions. Compared as a whole value, LLZ's claim never equals apl-operator's,
// and the reconciler pushes a commit on every pass forever against a writer that
// normalises it straight back. That is the exact failure SetAppSpec's `changed`
// contract exists to prevent, and the value TYPE was enough to bring it back.
func TestAplOperatorsNormalisedClaimIsNotDrift(t *testing.T) {
	current := strings.Replace(aplOperatorLoki,
		`                      storageClass: block-storage-retain`,
		`                      storageClass: block-storage-retain
                      volumeAttributesClassName: null
                      enableStatefulSetAutoDeletePVC: false`, 1)
	updated, changed, err := SetAppSpec([]byte(current), lokiWant(t))
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Errorf("apl-operator's own claim keys were read as drift — the reconciler would push "+
			"every pass, forever:\n%s", updated)
	}
	if !strings.Contains(string(updated), "volumeAttributesClassName") {
		t.Errorf("a key LLZ does not assert was dropped from the claim:\n%s", updated)
	}
}

// …and a claim whose ASSERTED value is wrong is still corrected. Without this the
// test above is satisfied by a merge that never changes anything.
func TestAWrongClaimStorageClassIsCorrected(t *testing.T) {
	current := strings.Replace(aplOperatorLoki,
		"storageClass: block-storage-retain", "storageClass: linode-block-storage", 1)
	updated, changed, err := SetAppSpec([]byte(current), lokiWant(t))
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("an unencrypted storage class on the WAL claim was accepted as correct")
	}
	if !strings.Contains(string(updated), "block-storage-retain") {
		t.Errorf("the asserted class was not written:\n%s", updated)
	}
}

// A claim apl-operator does not have yet is APPENDED, not dropped — otherwise
// enabling persistence on an app whose CR predates it would assert nothing.
func TestAnAbsentClaimIsAdded(t *testing.T) {
	current := "spec:\n  _rawValues:\n    ingester:\n      persistence:\n        claims:\n          - name: other\n            size: 1Gi\n"
	updated, changed, err := SetAppSpec([]byte(current), lokiWant(t))
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("a missing claim was not added")
	}
	for _, want := range []string{"name: other", "name: data", "block-storage-retain"} {
		if !strings.Contains(string(updated), want) {
			t.Errorf("%q missing after the merge:\n%s", want, updated)
		}
	}
}

// AN ANONYMOUS LIST STILL COMPARES WHOLE. The keyed merge is narrow on purpose —
// merging two argument lists element-wise has no defensible meaning, and a
// general "merge lists" rule would half-apply one.
func TestAnAnonymousListIsStillReplacedWholesale(t *testing.T) {
	current := "spec:\n  _rawValues:\n    args:\n      - a\n      - b\n"
	want := AppOverlay{RawValues: map[string]any{"args": []any{"x"}}}
	updated, changed, err := SetAppSpec([]byte(current), want)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || strings.Contains(string(updated), "- b") {
		t.Errorf("an anonymous list was merged rather than replaced (changed=%v):\n%s", changed, updated)
	}
}
