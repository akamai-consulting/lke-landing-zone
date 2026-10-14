package credpaths

// The table had NO tests when it left internal/extensions/reconcilelanes, which is
// its own small finding: five packages read it and none of them checked it. What
// is worth checking is not the contents -- those change -- but the invariants
// every consumer already assumes and none of them states.

import (
	"strings"
	"testing"
)

func TestEveryPathCarriesAKnownClass(t *testing.T) {
	known := map[string]bool{
		CredClassAutomated: true, CredClassGenerateOnce: true, CredClassOnDemand: true,
		CredClassStatic: true, CredClassTracksSource: true,
	}
	for _, p := range CredPaths {
		if !known[p.Class] {
			// An unknown class silently falls out of every consumer's switch. The
			// credential-age gauge would publish it unlabelled, and the 90d alert
			// would then fire on a path with no rotator behind it and no way to
			// clear it — the exact permanent noise the classes exist to prevent.
			t.Errorf("%s has class %q, which is not one of the five", p.Path, p.Class)
		}
	}
}

func TestPathsAreUniqueAndWellFormed(t *testing.T) {
	seenPath := map[string]string{}
	seenCred := map[string]string{}
	for _, p := range CredPaths {
		if prev, dup := seenPath[p.Path]; dup {
			t.Errorf("duplicate path %s (also as %q) — two rows for one path means one of "+
				"them is silently ignored by every map-keyed consumer", p.Path, prev)
		}
		seenPath[p.Path] = p.Cred
		// Cred is the gauge's label value; two paths sharing one would make the
		// series collide and report whichever scraped last.
		if prev, dup := seenCred[p.Cred]; dup {
			t.Errorf("duplicate cred label %q (on %s and %s)", p.Cred, prev, p.Path)
		}
		seenCred[p.Cred] = p.Path

		if !strings.HasPrefix(p.Path, "secret/") {
			t.Errorf("%s does not start with secret/ — the KV v2 helpers rewrite that "+
				"prefix to secret/data or secret/metadata and would produce a broken path", p.Path)
		}
		if p.Cred == "" {
			t.Errorf("%s has no cred label", p.Path)
		}
	}
	if len(CredPaths) == 0 {
		t.Fatal("the table is empty — every consumer would report a clean bill of health")
	}
}

// DBAdminRoot is a PREFIX under which per-deployment paths hang, not a leaf, and
// it is the one entry that is not in CredPaths. Both facts are load-bearing: it is
// absent because there is one path per database cluster and the set is not known
// statically.
func TestDBAdminRootIsAPrefixOutsideTheTable(t *testing.T) {
	if !strings.HasPrefix(DBAdminRoot, "secret/") {
		t.Errorf("DBAdminRoot = %q, want a secret/ path", DBAdminRoot)
	}
	if strings.HasSuffix(DBAdminRoot, "/") {
		t.Errorf("DBAdminRoot = %q — callers join it, so a trailing slash doubles it", DBAdminRoot)
	}
	for _, p := range CredPaths {
		if p.Path == DBAdminRoot {
			t.Errorf("DBAdminRoot appears in CredPaths as a leaf; it is a prefix with one " +
				"path per database cluster, which is why it cannot be enumerated here")
		}
	}
}
