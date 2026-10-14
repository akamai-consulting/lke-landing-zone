package clusterspec

import (
	"strings"
	"testing"
)

// The defect: every instance created literally `platform-loki-chunks-<env>`, and
// Linode bucket labels share one namespace per region ACROSS ACCOUNTS — so the
// second adopter to use a deployment name in a region got "[400] already exists".
func TestObjLabelPrefixIsPerInstance(t *testing.T) {
	a := &LandingZone{Metadata: Metadata{Name: "acme-platform"}}
	b := &LandingZone{Metadata: Metadata{Name: "globex-platform"}}

	if ObjLokiChunksBucket(a.ObjLabelPrefix(), "lab") == ObjLokiChunksBucket(b.ObjLabelPrefix(), "lab") {
		t.Fatal("two instances produce the same bucket label for the same deployment name")
	}
	if got, want := ObjLokiChunksBucket(a.ObjLabelPrefix(), "lab"), "acme-platform-loki-chunks-lab"; got != want {
		t.Errorf("loki chunks = %q, want %q", got, want)
	}
	if got, want := ObjHarborRegistryBucket(a.ObjLabelPrefix(), "lab"), "acme-platform-harbor-registry-lab"; got != want {
		t.Errorf("harbor registry = %q, want %q", got, want)
	}
	// Key labels carry it too: they are per-account rather than global, but reap
	// and the rotation table match by EXACT label, so two instances in one Linode
	// account would otherwise reap and rotate each other's keys.
	for _, l := range ObjKeyLabels(a.ObjLabelPrefix(), "lab") {
		if !strings.HasPrefix(l, "acme-platform-") {
			t.Errorf("key label %q is not namespaced to the instance", l)
		}
	}
}

// An explicit spec value wins; otherwise it is derived, because metadata.name
// comes from the GitHub repo short name and may not be a legal bucket label.
func TestObjLabelPrefixExplicitBeatsDerived(t *testing.T) {
	lz := &LandingZone{Metadata: Metadata{Name: "Derived_Name"}}
	if got, want := lz.ObjLabelPrefix(), "derived-name"; got != want {
		t.Errorf("derived = %q, want %q", got, want)
	}
	lz.Spec.Instance.ObjLabelPrefix = "chosen"
	if got := lz.ObjLabelPrefix(); got != "chosen" {
		t.Errorf("explicit = %q, want chosen", got)
	}
}

func TestSanitizeObjLabelPrefix(t *testing.T) {
	cases := map[string]string{
		"acme":                 "acme",
		"Acme_Platform":        "acme-platform",    // uppercase + underscore
		"my.instance.repo":     "my-instance-repo", // dots
		"--leading-trailing--": "leading-trailing",
		"a  b":                 "a-b", // runs collapse to ONE hyphen
		"___":                  "",    // nothing legal survives → caller must reject
		"":                     "",
	}
	for in, want := range cases {
		if got := SanitizeObjLabelPrefix(in); got != want {
			t.Errorf("Sanitize(%q) = %q, want %q", in, got, want)
		}
	}
	// An absurd name is bounded, but ordinary ones are NOT truncated — a silent
	// rename of a cloud resource is worse than a long label.
	long := SanitizeObjLabelPrefix(strings.Repeat("x", 100))
	if len(long) != ObjLabelPrefixMaxLen() {
		t.Errorf("long name truncated to %d, want %d", len(long), ObjLabelPrefixMaxLen())
	}
}

func TestValidateRejectsAnUnusableObjLabelPrefix(t *testing.T) {
	base := func() *LandingZone {
		return &LandingZone{
			APIVersion: APIVersion, Kind: Kind,
			Metadata: Metadata{Name: "acme"},
			Spec: Spec{
				Instance:     Instance{Repo: "o/acme", Forge: "github"},
				Environments: map[string]Environment{},
			},
		}
	}
	t.Run("an explicit illegal prefix is rejected", func(t *testing.T) {
		lz := base()
		lz.Spec.Instance.ObjLabelPrefix = "Bad_Prefix"
		errs := validateObjLabelPrefix(lz)
		if len(errs) == 0 {
			t.Fatal("an illegal bucket-label prefix must not reach terraform apply")
		}
		if !strings.Contains(errs[0].Error(), "spec.instance.objLabelPrefix") {
			t.Errorf("error should name the field the operator can fix: %v", errs[0])
		}
	})
	t.Run("a name that sanitizes to nothing is rejected, not defaulted", func(t *testing.T) {
		lz := base()
		lz.Metadata.Name = "___"
		errs := validateObjLabelPrefix(lz)
		if len(errs) == 0 {
			t.Fatal("an empty prefix would collide with every other instance")
		}
		// Silently substituting a shared default is the bug this replaces.
		if strings.Contains(errs[0].Error(), "platform") {
			t.Errorf("must not suggest a shared default: %v", errs[0])
		}
	})
	t.Run("an over-long prefix is rejected", func(t *testing.T) {
		lz := base()
		lz.Spec.Instance.ObjLabelPrefix = strings.Repeat("a", ObjLabelPrefixMaxLen()+1)
		if len(validateObjLabelPrefix(lz)) == 0 {
			t.Fatal("an over-long prefix produces labels Linode rejects at apply")
		}
	})
	t.Run("the derived default is accepted", func(t *testing.T) {
		if errs := validateObjLabelPrefix(base()); len(errs) != 0 {
			t.Fatalf("a normal instance name must validate: %v", errs)
		}
	})
}

// Empty in, empty out: a caller that failed to resolve a prefix must get a value
// that fails loudly downstream, never a plausible label pointing somewhere else.
func TestObjLabelRefusesToGuess(t *testing.T) {
	if got := ObjLokiChunksBucket("", "lab"); got != "" {
		t.Errorf("no prefix must yield no label, got %q", got)
	}
	if got := ObjLokiChunksBucket("acme", ""); got != "" {
		t.Errorf("no env must yield no label, got %q", got)
	}
}

// A divergent aplValues.repoURL has no working configuration behind it: the
// AppProject sourceRepos allowlist holds only the instance repo plus the
// template, so Argo REJECTS a carved App pointing elsewhere. Silently ignoring a
// field the adopter set would be the next bug, so it is rejected at render.
func TestValidateAplValuesRepo(t *testing.T) {
	lz := func(url string) *LandingZone {
		return &LandingZone{
			Metadata: Metadata{Name: "acme"},
			Spec: Spec{
				Instance: Instance{Repo: "o/acme"},
				Environments: map[string]Environment{"lab": {Cluster: Cluster{
					Bootstrap: Bootstrap{AplValues: AplValues{RepoURL: url}},
				}}},
			},
		}
	}
	if errs := validateAplValuesRepo(lz("")); len(errs) != 0 {
		t.Errorf("unset must pass: %v", errs)
	}
	// The spellings an operator actually writes for the same repo.
	for _, same := range []string{
		"https://github.com/o/acme.git",
		"https://github.com/o/acme",
		"https://github.com/o/acme/",
		"https://GitHub.com/O/Acme.git",
	} {
		if errs := validateAplValuesRepo(lz(same)); len(errs) != 0 {
			t.Errorf("%q is the instance repo: %v", same, errs)
		}
	}
	errs := validateAplValuesRepo(lz("https://github.com/other/values.git"))
	if len(errs) == 0 {
		t.Fatal("a values repo Argo cannot serve must not render")
	}
	if !strings.Contains(errs[0].Error(), "environments.lab") {
		t.Errorf("error should name the deployment: %v", errs[0])
	}
}
