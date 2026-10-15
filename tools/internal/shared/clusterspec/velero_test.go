package clusterspec

import (
	"os"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

const veleroDir = "../../../../platform-apl/components/velero/velero/"

func readVelero(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(veleroDir + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// veleroApp is the slice of the chart Application (base manifest or rendered
// patch) the schedule checks read.
type veleroApp struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Source struct {
			Helm struct {
				ReleaseName  string `json:"releaseName"`
				ValuesObject struct {
					Schedules map[string]struct {
						Disabled bool `json:"disabled"`
						Template struct {
							IncludedNamespaces      []string `json:"includedNamespaces"`
							ExcludedNamespaces      []string `json:"excludedNamespaces"`
							IncludeClusterResources *bool    `json:"includeClusterResources"`
							SnapshotVolumes         *bool    `json:"snapshotVolumes"`
						} `json:"template"`
					} `json:"schedules"`
					Configuration struct {
						BackupStorageLocation []struct {
							Bucket string            `json:"bucket"`
							Config map[string]string `json:"config"`
						} `json:"backupStorageLocation"`
					} `json:"configuration"`
					Credentials struct {
						ExistingSecret string `json:"existingSecret"`
					} `json:"credentials"`
				} `json:"valuesObject"`
			} `json:"helm"`
		} `json:"source"`
	} `json:"spec"`
}

func parseVeleroApp(t *testing.T, doc string) veleroApp {
	t.Helper()
	var a veleroApp
	if err := yaml.Unmarshal([]byte(doc), &a); err != nil {
		t.Fatal(err)
	}
	return a
}

// assertSchedulesProtectOpenBao fails closed: no schedules is a failure, so a
// renamed values path cannot turn this into a check of nothing.
func assertSchedulesProtectOpenBao(t *testing.T, where string, a veleroApp) {
	t.Helper()
	scheds := a.Spec.Source.Helm.ValuesObject.Schedules
	if len(scheds) == 0 {
		t.Fatalf("%s: no schedules at spec.source.helm.valuesObject.schedules — the check examined nothing", where)
	}
	for name, s := range scheds {
		scope := VeleroScope{Include: s.Template.IncludedNamespaces, Exclude: s.Template.ExcludedNamespaces}
		if err := VeleroScopeProtectsOpenBao(scope); err != nil {
			t.Errorf("%s: schedule %q: %v", where, name, err)
		}
		// includeClusterResources: true backs up EVERY cluster-scoped object,
		// including the PersistentVolumes bound to llz-openbao's Raft PVCs — the
		// namespace exclusion would no longer keep its volumes out. Left unset,
		// Velero takes only cluster objects related to included namespaces.
		if ic := s.Template.IncludeClusterResources; ic != nil && *ic {
			t.Errorf("%s: schedule %q sets includeClusterResources: true — that backs up llz-openbao's PVs despite the exclusion", where, name)
		}
		if sv := s.Template.SnapshotVolumes; sv == nil || *sv {
			t.Errorf("%s: schedule %q must set snapshotVolumes: false — this platform cannot restore a snapshot-sourced PVC", where, name)
		}
	}
}

// THE OPENBAO EXCLUSION, across every path a schedule can take into the cluster:
// the base manifest (what a build without the render carries) and the rendered
// patch for every shape of spec — defaults, a replaced exclude list, a named
// include list, globs. The patch REPLACES the base schedule, so each rendered
// patch is checked on its own.
func TestVeleroScheduleNeverIncludesOpenBao(t *testing.T) {
	assertSchedulesProtectOpenBao(t, "velero.yaml (base)", parseVeleroApp(t, readVelero(t, "velero.yaml")))
	for _, tog := range []ComponentToggle{
		{},
		{BackupExcludeNamespaces: "kube-system"},
		{BackupExcludeNamespaces: "team-a team-b"},
		{BackupIncludeNamespaces: "team-a team-b"},
		{BackupIncludeNamespaces: "llz-*"}, // a glob that matches llz-openbao: the exclude must win
		{BackupIntervalHours: intp(6), Retention: "7d"},
	} {
		p := RenderVeleroPatch("us-ord-10", "acme-velero-prod", tog)
		assertSchedulesProtectOpenBao(t, "rendered patch for "+SizingSummary(tog), parseVeleroApp(t, p))
	}
}

// And the guard itself must say no: dropping the exclusion, or naming
// llz-openbao in the include list, is an error — not a scope it vouches for.
func TestVeleroScopeProtectsOpenBaoRefuses(t *testing.T) {
	for _, s := range []VeleroScope{
		{Include: []string{"*"}, Exclude: []string{"llz-velero"}},
		{Include: []string{"*"}, Exclude: []string{"llz-openbao"}},
		{Include: []string{"llz-openbao"}, Exclude: VeleroMandatoryExcludes},
		{Include: []string{"*"}},
	} {
		if err := VeleroScopeProtectsOpenBao(s); err == nil {
			t.Errorf("scope %+v accepted", s)
		}
	}
	if err := VeleroScopeProtectsOpenBao(VeleroScopeFor(ComponentToggle{})); err != nil {
		t.Errorf("the default scope is refused: %v", err)
	}
}

func TestVeleroScopeFor(t *testing.T) {
	def := VeleroScopeFor(ComponentToggle{})
	if strings.Join(def.Include, ",") != "*" {
		t.Errorf("default include = %v", def.Include)
	}
	if !strings.HasPrefix(strings.Join(def.Exclude, ","), "llz-openbao,llz-velero,kube-system") || len(def.Exclude) != 2+len(VeleroDefaultExcludes) {
		t.Errorf("default exclude = %v", def.Exclude)
	}
	// A replaced list drops the defaults but never the mandatory pair, and never
	// duplicates one the author restated.
	got := VeleroScopeFor(ComponentToggle{BackupExcludeNamespaces: "llz-openbao team-x"})
	if strings.Join(got.Exclude, ",") != "llz-openbao,llz-velero,team-x" {
		t.Errorf("replaced exclude = %v", got.Exclude)
	}
}

func TestVeleroKnobsValidate(t *testing.T) {
	ok := []ComponentToggle{
		{},
		{BackupIntervalHours: intp(24), Retention: "30d"},
		{BackupIntervalHours: intp(6), Retention: "1d"},
		{Retention: "8784h"},
		{BackupIncludeNamespaces: "team-a team-*"},
		{BackupIncludeNamespaces: "*", BackupExcludeNamespaces: "kube-system"},
	}
	for _, tog := range ok {
		if errs := validateComponentSizing("e", "velero", tog); len(errs) != 0 {
			t.Errorf("%+v refused: %v", tog, errs)
		}
	}
	bad := map[string]ComponentToggle{
		"interval not a divisor of 24": {BackupIntervalHours: intp(5)},
		"TTL shorter than two runs":    {BackupIntervalHours: intp(24), Retention: "36h"},
		"TTL under a day":              {BackupIntervalHours: intp(1), Retention: "6h"},
		"TTL over a year":              {Retention: "2y"},
		"not a duration":               {Retention: "a month"},
		"include names llz-openbao":    {BackupIncludeNamespaces: "llz-openbao"},
		"include names llz-velero":     {BackupIncludeNamespaces: "team-a llz-velero"},
		"include mixes *":              {BackupIncludeNamespaces: "* team-a"},
		"exclude everything":           {BackupExcludeNamespaces: "*"},
		"bad namespace":                {BackupExcludeNamespaces: "Team_A"},
		"backupKeep is openbao's":      {BackupKeep: intp(3)},
	}
	for why, tog := range bad {
		if errs := validateComponentSizing("e", "velero", tog); len(errs) == 0 {
			t.Errorf("%s: %+v accepted", why, tog)
		}
	}
	// And velero's namespace knobs on another component are a mistake, not a no-op.
	if errs := validateComponentSizing("e", "openbaoBackup", ComponentToggle{BackupExcludeNamespaces: "x"}); len(errs) == 0 {
		t.Error("backupExcludeNamespaces on openbaoBackup was accepted")
	}
}

func TestVeleroRetentionHours(t *testing.T) {
	for in, want := range map[string]int{"30d": 720, "720h": 720, "2w": 336, "1y": 8760, "90m": 2, "30s": 1} {
		if got, ok := VeleroRetentionHours(in); !ok || got != want {
			t.Errorf("VeleroRetentionHours(%q) = %d,%v want %d", in, got, ok, want)
		}
	}
	if _, ok := VeleroRetentionHours("monthly"); ok {
		t.Error("a non-duration converted")
	}
	if VeleroSchedule(24) != "37 1 * * *" || VeleroSchedule(6) != "37 */6 * * *" {
		t.Errorf("schedules: %q %q", VeleroSchedule(24), VeleroSchedule(6))
	}
}

// The rendered patch must land on the Application the component ships, carry
// this deployment's bucket and endpoint, and render the defaults when no knob is
// set — so a spec without knobs and the base manifest agree on cadence and TTL.
func TestVeleroPatchTargetsTheShippedApplication(t *testing.T) {
	base := parseVeleroApp(t, readVelero(t, "velero.yaml"))
	if base.Metadata.Name != VeleroApplication {
		t.Fatalf("velero.yaml names %q, the patch targets %q", base.Metadata.Name, VeleroApplication)
	}
	// The chart names the Schedule <release>-<key>; the alerts and the drill key on it.
	if base.Spec.Source.Helm.ReleaseName+"-cluster" != VeleroScheduleName {
		t.Errorf("release %q yields schedule %q-cluster, but VeleroScheduleName is %q",
			base.Spec.Source.Helm.ReleaseName, base.Spec.Source.Helm.ReleaseName, VeleroScheduleName)
	}
	if _, ok := base.Spec.Source.Helm.ValuesObject.Schedules["cluster"]; !ok {
		t.Error("velero.yaml has no `cluster` schedule key — the one the patch replaces")
	}
	baseRaw := readVelero(t, "velero.yaml")
	for _, want := range []string{`schedule: "` + VeleroSchedule(VeleroDefaultIntervalHours) + `"`, "ttl: 720h"} {
		if !strings.Contains(baseRaw, want) {
			t.Errorf("velero.yaml lacks %q — the base default drifted from the render default", want)
		}
	}

	p := parseVeleroApp(t, RenderVeleroPatch("us-ord-10", "acme-velero-prod", ComponentToggle{}))
	bsl := p.Spec.Source.Helm.ValuesObject.Configuration.BackupStorageLocation
	if p.Metadata.Name != VeleroApplication || len(bsl) != 1 {
		t.Fatalf("patch = %+v", p)
	}
	c := bsl[0].Config
	if bsl[0].Bucket != "acme-velero-prod" || c["region"] != "us-ord-10" || c["s3Url"] != "https://us-ord-10.linodeobjects.com" {
		t.Errorf("BSL = %+v", bsl[0])
	}
	// Virtual-hosted (the objProxy rewrite is exact-host; path-style would be
	// captured), SSE-C under the Secret the component's ExternalSecret writes, and
	// no CRC32 trailer framing (Linode's Ceph rejects it).
	if c["s3ForcePathStyle"] != "false" || c["customerKeyEncryptionSecret"] != "velero-ssec-key/customer-key" || c["checksumAlgorithm"] != "" {
		t.Errorf("BSL config = %v", c)
	}
	if _, ok := c["checksumAlgorithm"]; !ok {
		t.Error("checksumAlgorithm must be present and empty: absent means the plugin's CRC32 default")
	}
	raw := RenderVeleroPatch("us-ord-10", "acme-velero-prod", ComponentToggle{})
	for _, want := range []string{`schedule: "37 1 * * *"`, "ttl: 720h"} {
		if !strings.Contains(raw, want) {
			t.Errorf("default patch lacks %q:\n%s", want, raw)
		}
	}

	// The two Secrets the chart values name are the two the component's
	// ExternalSecrets create.
	es := readVelero(t, "externalsecret.yaml")
	if want := base.Spec.Source.Helm.ValuesObject.Credentials.ExistingSecret; !strings.Contains(es, "name: "+want+"\n") {
		t.Errorf("credentials.existingSecret %q is not an ExternalSecret target", want)
	}
	if !strings.Contains(es, "name: velero-ssec-key\n") || !strings.Contains(es, "secretKey: customer-key") ||
		!strings.Contains(es, "decodingStrategy: Base64") {
		t.Error("the velero-ssec-key ExternalSecret must write customer-key, base64-decoded to the 32 raw bytes the plugin requires")
	}
}

// The component declares exactly the patch render writes, and is carved.
func TestVeleroComponentDeclaresItsPatch(t *testing.T) {
	for _, c := range Components {
		if c.Name != "velero" {
			continue
		}
		if !c.DefaultDisabled || c.CarvedApp == nil || c.CarvedApp.Namespace != VeleroNamespace {
			t.Errorf("velero must be default-disabled and carved into %s: %+v", VeleroNamespace, c)
		}
		if len(c.Patches) != 1 || c.Patches[0].Kind != "Application" || c.Patches[0].Name != VeleroApplication {
			t.Fatalf("velero patches = %+v", c.Patches)
		}
		return
	}
	t.Fatal("no velero component")
}

// The alerts key on the schedule the chart renders, and the NetworkPolicy selects
// the pod the chart labels (helm template of the pinned chart: Deployment velero,
// pod label app.kubernetes.io/name=velero, metrics on :8085). A policy selecting
// nothing leaves the server under the default-deny alone.
func TestVeleroAlertsAndPolicyTargetTheChart(t *testing.T) {
	rules := readVelero(t, "prometheusrule.yaml")
	if n := strings.Count(rules, `schedule="`+VeleroScheduleName+`"`); n < 4 {
		t.Errorf("only %d rule selectors name schedule=%q", n, VeleroScheduleName)
	}
	if strings.Contains(strings.ReplaceAll(rules, `schedule="`+VeleroScheduleName+`"`, ""), `schedule="`) {
		t.Error("a rule selects a schedule other than " + VeleroScheduleName)
	}
	for _, alert := range []string{"LLZVeleroBackupFailing", "LLZVeleroBackupStale", "LLZVeleroBackupNeverSucceeded",
		"LLZVeleroStorageUnavailable", "LLZVeleroMetricsAbsent"} {
		if !strings.Contains(rules, "alert: "+alert) {
			t.Errorf("prometheusrule.yaml lacks %s", alert)
		}
	}
	np := readVelero(t, "network-policy.yaml")
	for _, want := range []string{"app.kubernetes.io/name: velero", "port: 8085", "port: 6443", "kubernetes.io/metadata.name: monitoring"} {
		if !strings.Contains(np, want) {
			t.Errorf("network-policy.yaml lacks %q", want)
		}
	}
}
