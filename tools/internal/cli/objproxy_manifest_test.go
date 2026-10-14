package cli

import (
	"os"
	"testing"

	"gopkg.in/yaml.v3"
)

// A non-root container cannot read a root-owned Secret file unless fsGroup makes
// the kubelet group-own the projection. runAsUser and defaultMode are each
// individually sensible and TOGETHER they killed every pod in an e2e run:
//
//	llz: read SSE-C key: open /etc/llz/ssec/key: permission denied
//
// No unit test could see it — it is a property of two manifest fields, so the check
// belongs on the manifest.
func TestObjProxyDaemonSetSecretsAreReadableByItsUser(t *testing.T) {
	raw, err := os.ReadFile("../../../platform-apl/components/objProxy/obj-proxy/daemonset.yaml")
	if err != nil {
		t.Fatalf("could not read the DaemonSet (%v) — a skip here would reproduce the gap it closes", err)
	}
	var ds struct {
		Spec struct {
			Template struct {
				Spec struct {
					SecurityContext struct {
						RunAsUser *int64 `yaml:"runAsUser"`
						FSGroup   *int64 `yaml:"fsGroup"`
					} `yaml:"securityContext"`
					Volumes []struct {
						Name   string `yaml:"name"`
						Secret *struct {
							DefaultMode *int `yaml:"defaultMode"`
						} `yaml:"secret"`
					} `yaml:"volumes"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(raw, &ds); err != nil {
		t.Fatal(err)
	}
	sc := ds.Spec.Template.Spec.SecurityContext
	if sc.RunAsUser == nil {
		t.Fatal("runAsUser is unset — this test's premise no longer holds, revisit it")
	}
	if sc.FSGroup == nil {
		t.Fatal("runAsUser is set but fsGroup is NOT: Secret volumes are root-owned, so every " +
			"mounted credential is unreadable and the process dies with `permission denied` at startup")
	}
	if *sc.FSGroup != *sc.RunAsUser {
		t.Errorf("fsGroup %d does not match runAsUser %d — the group bit only helps if the process is in that group",
			*sc.FSGroup, *sc.RunAsUser)
	}
	for _, v := range ds.Spec.Template.Spec.Volumes {
		if v.Secret == nil || v.Secret.DefaultMode == nil {
			continue
		}
		mode := *v.Secret.DefaultMode
		if mode&0o040 == 0 {
			t.Errorf("volume %q has defaultMode %#o with no GROUP read bit — fsGroup cannot help, "+
				"and the container user (root-owned files) gets permission denied", v.Name, mode)
		}
		if mode&0o004 != 0 {
			t.Errorf("volume %q has defaultMode %#o — world-readable. The SSE-C key is the one "+
				"unrecoverable secret here; group-readable (0440) is enough", v.Name, mode)
		}
	}
}
