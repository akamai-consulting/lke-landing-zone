package clusterspec

// wiz.go — the per-env surface of the `wiz` component (Wiz CSPM cluster
// enrollment): where the credential lives, what name the cluster enrolls under,
// and the two patches `llz render` writes into the carved llz-wiz App's source root.

import (
	"fmt"
	"regexp"
	"strings"
)

// Fixed names the wiz component's manifests (platform-apl/components/wiz/) and
// the enrollment gate both rely on. They are the CONTRACT between the bundle and
// `llz ci assert-wiz-enrollment`, so they live here once rather than being
// restated on each side.
const (
	// WizNamespace is the namespace the bundle creates and the chart releases into.
	WizNamespace = "wiz"
	// WizCredentialSecret is the ExternalSecret AND its target Secret: the name the
	// upstream chart is pointed at via global.wizApiToken.secret.name.
	WizCredentialSecret = "wiz-api-token"
	// WizReleaseName is the Helm release name on the llz-wiz-connector Application.
	// The upstream chart derives the broker Deployment (<release>-agent) and the
	// registration Secret (<release>-connector) from it.
	WizReleaseName = "wiz"
	// WizConnectorApplication is the Application that installs the upstream chart.
	WizConnectorApplication = "llz-wiz-connector"
)

// WizBrokerDeployment is the upstream broker Deployment the release produces.
func WizBrokerDeployment() string { return WizReleaseName + "-agent" }

// WizRegistrationSecret is the Secret the upstream registration Job writes the
// connector record into once Wiz has accepted the cluster.
func WizRegistrationSecret() string { return WizReleaseName + "-connector" }

// WizCredentialFields are the properties the credential must carry at secretPath,
// in the upstream chart's key names. A `clientSecret` (the Wiz console's word) is
// ignored by the chart, so the seed instructions and the ExternalSecret both say
// clientToken.
var WizCredentialFields = []string{"clientId", "clientToken"}

// wizSecretPathRe is a KV path in the bao CLI's absolute form: secret/ followed
// by lowercase kebab segments. The same alphabet the team subtree validator uses,
// so a valid path under a valid subtree is always expressible.
var wizSecretPathRe = regexp.MustCompile(`^secret(/[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

// validateWizSecretPath enforces the wiz component's one required setting. It
// runs from the top-level Validate because the check needs spec.teams, which a
// per-env validator does not see.
//
// UNDER A TEAM SUBTREE, OR NOTHING READS IT. The `openbao` ClusterSecretStore's
// identity holds platform-ci (an explicit list of platform paths) plus one
// `<team>-reader` per declared team. Any other path validates as a string, renders
// cleanly, syncs to an ExternalSecret that is denied on every refresh, and leaves
// the connector unregistered — a failure that looks, from the Wiz console, like a
// cluster that was never installed.
func validateWizSecretPath(env string, components map[string]ComponentToggle, teams []Team) []error {
	if !ComponentEnabled(components, "wiz") {
		return nil
	}
	p := components["wiz"].SecretPath
	if p == "" {
		return []error{fmt.Errorf("environments.%s.components.wiz requires secretPath when enabled — the OpenBao KV path holding the Wiz service-account clientId + clientToken, under a declared spec.teams[].openbaoSubtree (e.g. secret/<team>/wiz/connector)", env)}
	}
	if !wizSecretPathRe.MatchString(p) {
		return []error{fmt.Errorf("environments.%s.components.wiz.secretPath %q must be secret/<segment>[/<segment>…] with lowercase kebab segments (the bao CLI's absolute form, e.g. secret/<team>/wiz/connector)", env, p)}
	}
	var subtrees []string
	for _, t := range teams {
		if t.OpenbaoSubtree == "" {
			continue
		}
		if strings.HasPrefix(p, t.OpenbaoSubtree+"/") {
			return nil
		}
		subtrees = append(subtrees, t.OpenbaoSubtree)
	}
	if len(subtrees) == 0 {
		return []error{fmt.Errorf("environments.%s.components.wiz.secretPath %q: no spec.teams entry declares an openbaoSubtree, and only a team subtree is readable by the ESO identity without a platform policy change — declare a team (spec.teams: [{name, openbaoSubtree}]) and put the credential under it", env, p)}
	}
	return []error{fmt.Errorf("environments.%s.components.wiz.secretPath %q is not under any declared team subtree (%s) — the ESO identity cannot read it, so the ExternalSecret would be denied on every refresh", env, p, strings.Join(subtrees, ", "))}
}

// WizRemoteKey is secretPath relative to the ClusterSecretStore, which is already
// mounted at the KV engine: `secret/team-a/wiz/connector` → `team-a/wiz/connector`.
// Spelling it with the prefix would read secret/data/secret/… and be denied.
func WizRemoteKey(secretPath string) string {
	return strings.TrimPrefix(secretPath, "secret/")
}

// RenderWizExternalSecretPatch returns the per-env patch pointing the
// wiz-api-token ExternalSecret at the env's secretPath. spec.data is a list, and
// lists in a CR merge atomically, so the patch carries the COMPLETE data list.
func RenderWizExternalSecretPatch(secretPath string) string {
	key := WizRemoteKey(secretPath)
	var b strings.Builder
	b.WriteString("# GENERATED by `llz render` — the OpenBao path the Wiz credential is read from\n" +
		"# (spec.components.wiz.secretPath). spec.data is replaced wholesale (CR lists are atomic).\n" +
		"apiVersion: external-secrets.io/v1\n" +
		"kind: ExternalSecret\n" +
		"metadata:\n" +
		"  name: " + WizCredentialSecret + "\n" +
		"  namespace: " + WizNamespace + "\n" +
		"spec:\n" +
		"  data:\n")
	for _, f := range WizCredentialFields {
		b.WriteString("    - secretKey: " + f + "\n" +
			"      remoteRef:\n" +
			"        key: " + key + "\n" +
			"        property: " + f + "\n")
	}
	return b.String()
}

// RenderWizApplicationPatch returns the per-env patch naming the connector and the
// cluster after the environment, so each cluster enrolls under a name an auditor
// can match to the deployment inventory (and two envs never register as one).
func RenderWizApplicationPatch(env string) string {
	return "# GENERATED by `llz render` — the name this cluster enrolls in Wiz under (the env name).\n" +
		`apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: ` + WizConnectorApplication + `
  namespace: argocd
spec:
  source:
    helm:
      valuesObject:
        global:
          clusterDisplayName: "` + env + `"
        wiz-kubernetes-connector:
          autoCreateConnector:
            connectorName: "` + env + "\"\n"
}
