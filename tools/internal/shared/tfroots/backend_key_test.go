package tfroots

import (
	"fmt"
	"strings"
	"testing"
)

// Every root's backend.tf documents the -backend-config `key` an operator must pass
// at init, and each root must name ITS OWN state prefix.
//
// This is not cosmetic. The roots' backend.tf files are copies of one another, and
// the databases root shipped carrying object-storage's key verbatim. `terraform init`
// against another root's state key loads that root's state, and every resource in it
// is absent from this configuration — so the next plan proposes DESTROYING them.
// Following that comment on the databases root would have proposed destroying the
// registry and loki buckets in order to create a database.
//
// CI derives the key from the module name (.github/actions/terraform-init), so this
// only bites a by-hand apply — which is what the runbooks describe, and the only way
// to run a root whose workflow jobs do not exist yet.
func TestBackendTfDocumentsOwnStateKey(t *testing.T) {
	// vpc is instance-wide rather than per-deployment, so its key omits <region>.
	want := map[string]string{
		"cluster":        `key      = "cluster/<region>/terraform.tfstate"`,
		"object-storage": `key      = "object-storage/<region>/terraform.tfstate"`,
		"databases":      `key      = "databases/<region>/terraform.tfstate"`,
		"vpc":            `key      = "vpc/terraform.tfstate"`,
	}
	for root, wantKey := range want {
		b, err := embedded.ReadFile(fmt.Sprintf("roots/%s/backend.tf", root))
		if err != nil {
			t.Errorf("%s: %v", root, err)
			continue
		}
		if !strings.Contains(string(b), wantKey) {
			t.Errorf("%s/backend.tf does not document its own state key.\nwant a line containing: %s\n"+
				"A backend key naming ANOTHER root loads that root's state, and the next plan "+
				"proposes destroying everything in it that this configuration does not declare.", root, wantKey)
		}
		// And must not advertise a different root's prefix.
		for other := range want {
			if other == root {
				continue
			}
			if strings.Contains(string(b), fmt.Sprintf(`key      = "%s/`, other)) {
				t.Errorf("%s/backend.tf advertises the %s root's state key", root, other)
			}
		}
	}
}
