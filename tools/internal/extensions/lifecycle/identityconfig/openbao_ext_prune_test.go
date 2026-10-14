package identityconfig

import (
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
)

// The manifest must record EXACTLY what the real step renderer creates. A new
// kind of extension object added to openbaoExtensionSteps but not to
// desiredManifest would be created and never revoked — the drift this file
// exists to end — so this calls both real functions rather than restating either.
func TestManifestCoversEveryExtensionStep(t *testing.T) {
	ext, _ := extFixture()
	ext.Mounts = append(ext.Mounts, clusterspec.OpenBaoMount{Path: "pki-no-operators", Type: "pki"})
	var policies, roles, mounts []string
	for _, s := range openbaoExtensionSteps(ext) {
		a := s.args
		switch {
		case len(a) >= 3 && a[0] == "policy" && a[1] == "write":
			policies = append(policies, a[2])
		case len(a) >= 2 && a[0] == "write" && strings.HasPrefix(a[1], "auth/kubernetes/role/"):
			roles = append(roles, strings.TrimPrefix(a[1], "auth/kubernetes/role/"))
		case len(a) >= 3 && a[0] == "secrets" && a[1] == "enable":
			for _, f := range a {
				if p, ok := strings.CutPrefix(f, "-path="); ok {
					mounts = append(mounts, p)
				}
			}
		}
	}
	if len(policies) < 2 || len(roles) < 1 || len(mounts) < 2 {
		t.Fatalf("examined too little (policies=%v roles=%v mounts=%v) — did the step argv shape change?", policies, roles, mounts)
	}
	m := desiredManifest(ext)
	for _, c := range []struct {
		kind           string
		written, noted []string
	}{{"policy", policies, m.Policies}, {"role", roles, m.Roles}, {"mount", mounts, m.Mounts}} {
		sort.Strings(c.written)
		if strings.Join(c.written, ",") != strings.Join(c.noted, ",") {
			t.Errorf("%s: steps write %v but the manifest records %v — an unrecorded object is never revoked", c.kind, c.written, c.noted)
		}
	}
}

func TestManifestRoundTrip(t *testing.T) {
	ext, _ := extFixture()
	m := desiredManifest(ext)
	text := renderManifest(m)
	if !strings.Contains(text, `capabilities = ["deny"]`) || strings.Count(text, `path "`) != 1 {
		t.Fatalf("the manifest policy must grant nothing:\n%s", text)
	}
	got, err := parseManifest(text)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.Policies, ",") != strings.Join(m.Policies, ",") || strings.Join(got.Roles, ",") != strings.Join(m.Roles, ",") ||
		strings.Join(got.Mounts, ",") != strings.Join(m.Mounts, ",") {
		t.Fatalf("round trip lost names: wrote %+v, read %+v", m, got)
	}
}

// The manifest decides what is DELETED, so anything odd in it stops the prune.
func TestParseManifestRefusesMalformedRecords(t *testing.T) {
	for _, text := range []string{
		"# role: Has Spaces\n",
		"# policy: ../../sys\n",
		"# mount:\n",
		"# role: UPPER\n",
	} {
		if _, err := parseManifest(text); err == nil {
			t.Errorf("parseManifest(%q) accepted a record it must refuse", text)
		}
	}
	if m, err := parseManifest("# llz header\n# unrelated comment\npath \"x\" {}\n"); err != nil || !m.empty() {
		t.Fatalf("non-record lines must be ignored: %+v, %v", m, err)
	}
}

func TestPlanPrune(t *testing.T) {
	prev := extManifest{
		Policies: []string{"app-issue", "old-issue", "pki-x-operator", "platform-ci", "team-a-writer", extManifestPolicy},
		Roles:    []string{"app", "old", "eso"},
		Mounts:   []string{"pki-x", "pki-old"},
	}
	cur := extManifest{Policies: []string{"app-issue"}, Roles: []string{"app"}, Mounts: []string{"pki-x"}}
	p := planPrune(prev, cur)
	if strings.Join(p.Policies, ",") != "old-issue,pki-x-operator" {
		t.Errorf("policies = %v — want the removed extension ones, never a platform, team or manifest name", p.Policies)
	}
	if strings.Join(p.Roles, ",") != "old" {
		t.Errorf("roles = %v — want the removed one, never a platform role", p.Roles)
	}
	if strings.Join(p.OrphanMounts, ",") != "pki-old" {
		t.Errorf("orphan mounts = %v", p.OrphanMounts)
	}
	steps := pruneSteps(p)
	if len(steps) != 3 || steps[0].args[0] != "delete" || steps[0].args[1] != "auth/kubernetes/role/old" {
		t.Fatalf("roles must be revoked first: %+v", steps)
	}
	for _, s := range steps {
		if !s.fatal || (s.args[0] == "secrets" && s.args[1] == "disable") {
			t.Fatalf("every revoke is fatal and no mount is ever disabled: %+v", s)
		}
	}
}

// fakeBao is a one-policy OpenBao: `policy read` of the manifest answers with
// whatever was last written (or deleted), so the read-back is exercised for real.
type fakeBao struct {
	manifest string // current stored text; "" = no manifest
	readErr  error  // non-nil: a read failure that is NOT "no such policy"
	failArgs string // a call whose joined args contain this fails
	// strip simulates an OpenBao that drops comment lines on store.
	strip   bool
	calls   []string
	written string
}

func (f *fakeBao) Run(_, _, stdin string, args ...string) (string, string, error) {
	joined := strings.Join(args, " ")
	f.calls = append(f.calls, joined)
	switch {
	case f.failArgs != "" && strings.Contains(joined, f.failArgs):
		return "", "permission denied", errors.New("exit 2")
	case joined == "policy read "+extManifestPolicy:
		if f.readErr != nil {
			return "", "connection refused", f.readErr
		}
		if f.manifest == "" {
			return "", "No policy named: " + extManifestPolicy, errors.New("exit 2")
		}
		return f.manifest, "", nil
	case strings.HasPrefix(joined, "policy write "+extManifestPolicy):
		f.written = stdin
		f.manifest = stdin
		if f.strip {
			var kept []string
			for _, l := range strings.Split(stdin, "\n") {
				if !strings.HasPrefix(l, "#") {
					kept = append(kept, l)
				}
			}
			f.manifest = strings.Join(kept, "\n")
		}
	case joined == "policy delete "+extManifestPolicy:
		f.manifest = ""
	}
	return "", "", nil
}

func (f *fakeBao) did(prefix string) bool {
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func TestReconcileExtensionManifest(t *testing.T) {
	ext, _ := extFixture()
	cur := renderManifest(desiredManifest(ext))
	withOld := renderManifest(extManifest{
		Policies: append(desiredManifest(ext).Policies, "old-issue"),
		Roles:    append(desiredManifest(ext).Roles, "old-role"),
	})

	t.Run("an OpenBao that drops the record's comments fails the run", func(t *testing.T) {
		f := &fakeBao{strip: true}
		err := reconcileExtensionManifest(f, "p", "t", ext)
		if err == nil || !strings.Contains(err.Error(), "not preserving the record") {
			t.Fatalf("a manifest that does not read back must fail the run, got %v", err)
		}
	})
	t.Run("a manifest that survives deletion fails the run", func(t *testing.T) {
		f := &fakeBao{manifest: cur}
		// Make the delete a silent no-op: the record is still there afterwards.
		g := &stickyBao{fakeBao: f}
		if err := reconcileExtensionManifest(g, "p", "t", clusterspec.OpenBaoExtensions{}); err == nil {
			t.Fatal("a record that is still there after its delete must fail the run")
		}
	})
	t.Run("never declared: one read, nothing written", func(t *testing.T) {
		f := &fakeBao{}
		if err := reconcileExtensionManifest(f, "p", "t", clusterspec.OpenBaoExtensions{}); err != nil {
			t.Fatal(err)
		}
		if len(f.calls) != 1 {
			t.Fatalf("an instance that never declared spec.openbao must only read: %v", f.calls)
		}
	})
	t.Run("first run: no prune, manifest written", func(t *testing.T) {
		f := &fakeBao{}
		if err := reconcileExtensionManifest(f, "p", "t", ext); err != nil {
			t.Fatal(err)
		}
		if f.did("delete") || f.did("policy delete") || f.written != cur {
			t.Fatalf("first run must prune nothing and record the spec: calls=%v written=%q", f.calls, f.written)
		}
	})
	t.Run("removed entries are revoked, then recorded", func(t *testing.T) {
		f := &fakeBao{manifest: withOld}
		if err := reconcileExtensionManifest(f, "p", "t", ext); err != nil {
			t.Fatal(err)
		}
		if !f.did("delete auth/kubernetes/role/old-role") || !f.did("policy delete old-issue") || f.written != cur {
			t.Fatalf("calls=%v written=%q", f.calls, f.written)
		}
	})
	t.Run("everything removed: revoked and the record deleted", func(t *testing.T) {
		f := &fakeBao{manifest: cur}
		if err := reconcileExtensionManifest(f, "p", "t", clusterspec.OpenBaoExtensions{}); err != nil {
			t.Fatal(err)
		}
		if !f.did("delete auth/kubernetes/role/"+ext.KubernetesRoles[0].Name) || !f.did("policy delete "+extManifestPolicy) || f.written != "" {
			t.Fatalf("calls=%v", f.calls)
		}
	})
	t.Run("a failed revoke fails the run and keeps the old record", func(t *testing.T) {
		f := &fakeBao{manifest: withOld, failArgs: "auth/kubernetes/role/old-role"}
		if err := reconcileExtensionManifest(f, "p", "t", ext); err == nil {
			t.Fatal("a revoke that failed must fail the run")
		}
		if f.written != "" {
			t.Fatal("the manifest must not be rewritten after a failed revoke — the next run would forget to retry it")
		}
	})
	t.Run("an unreadable manifest prunes nothing", func(t *testing.T) {
		for _, f := range []*fakeBao{{readErr: errors.New("exit 2")}, {manifest: "# role: Bad Name\n"}} {
			if err := reconcileExtensionManifest(f, "p", "t", ext); err == nil {
				t.Fatalf("must fail closed: %+v", f)
			}
			if f.did("delete") || f.did("policy delete") || f.written != "" {
				t.Fatalf("nothing may be deleted or written on an unreadable record: %v", f.calls)
			}
		}
	})
}

// stickyBao is a fakeBao whose manifest delete reports success but keeps it.
type stickyBao struct{ *fakeBao }

func (s *stickyBao) Run(pod, token, stdin string, args ...string) (string, string, error) {
	if strings.Join(args, " ") == "policy delete "+extManifestPolicy {
		s.calls = append(s.calls, "policy delete "+extManifestPolicy)
		return "", "", nil
	}
	return s.fakeBao.Run(pod, token, stdin, args...)
}

func TestSameManifestIsOrderInsensitive(t *testing.T) {
	a := extManifest{Policies: []string{"b", "a"}, Roles: []string{"r"}}
	b := extManifest{Policies: []string{"a", "b"}, Roles: []string{"r"}}
	if !sameManifest(a, b) || sameManifest(a, extManifest{Policies: []string{"a"}, Roles: []string{"r"}}) {
		t.Fatal("sameManifest must compare as sets")
	}
}
