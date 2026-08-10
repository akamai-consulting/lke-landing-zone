package firewall

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

// The Linode client this package builds must come from its own DECLARATION. It
// holds cloud-read on a single binding, so cloudBinding() cannot pick the wrong
// one — but a second binding added later could, and this is what would notice.
func TestCloudBindingComesFromTheDeclaration(t *testing.T) {
	b := cloudBinding()
	var cloudGrants []extension.Grant
	for _, g := range b.Grants {
		if g == extension.CloudRead || g == extension.CloudMutate {
			cloudGrants = append(cloudGrants, g)
		}
	}
	if len(cloudGrants) == 0 {
		t.Fatalf("cloudBinding returned a binding with no cloud grant: %v", b.Grants)
	}
	// This extension reads the account to resolve firewall inputs; it must not
	// have quietly gained the grant that deletes things.
	for _, g := range cloudGrants {
		if g == extension.CloudMutate {
			t.Errorf("cloud-firewall-bootstrap gained cloud-mutate — it resolves inputs by " +
				"READING the account, and the handle would now permit DELETE")
		}
	}
	if Extension().Name == "" {
		t.Error("the extension has no name")
	}
}
