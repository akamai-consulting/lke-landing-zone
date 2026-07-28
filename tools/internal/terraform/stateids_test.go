package terraform

import "testing"

// The gsap-apl shape: the node firewall is still at the pre-migration SOURCE
// address inside a child module, while the destination address is absent. An
// importer that only checks the destination would adopt it a second time.
const gsapStateJSON = `{
  "values": {
    "root_module": {
      "resources": [
        {"address": "linode_vpc.this", "values": {"id": "12345"}}
      ],
      "child_modules": [
        {
          "resources": [
            {"address": "module.cluster.linode_lke_cluster.this", "values": {"id": 633257}}
          ],
          "child_modules": [
            {
              "resources": [
                {"address": "module.cluster.module.node_firewall.linode_firewall.this",
                 "values": {"id": "75288222"}}
              ]
            }
          ]
        }
      ]
    }
  }
}`

func TestStateIDsFindsNestedModuleResource(t *testing.T) {
	ids, err := StateIDs(gsapStateJSON)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := ids["75288222"]
	if !ok {
		t.Fatalf("firewall id not found; got %+v", ids)
	}
	if want := "module.cluster.module.node_firewall.linode_firewall.this"; got != want {
		t.Errorf("addr = %q, want %q", got, want)
	}
}

// Linode ids arrive quoted for some resource types and bare for others.
func TestStateIDsHandlesNumericAndStringIDs(t *testing.T) {
	ids, _ := StateIDs(gsapStateJSON)
	if got := ids["633257"]; got != "module.cluster.linode_lke_cluster.this" {
		t.Errorf("numeric id not normalized: %q (all: %+v)", got, ids)
	}
	if got := ids["12345"]; got != "linode_vpc.this" {
		t.Errorf("root-module resource missing: %q", got)
	}
}

// The guard must fail open — a missing or empty state cannot block a first import.
func TestStateIDsEmptyState(t *testing.T) {
	for _, in := range []string{"", "{}", `{"values":{}}`} {
		ids, err := StateIDs(in)
		if err != nil {
			t.Errorf("StateIDs(%q) errored: %v", in, err)
		}
		if len(ids) != 0 {
			t.Errorf("StateIDs(%q) = %+v, want empty", in, ids)
		}
	}
}

func TestStateIDsInvalidJSON(t *testing.T) {
	if _, err := StateIDs("not json"); err == nil {
		t.Error("want an error on unparseable input so the caller can fail open deliberately")
	}
}
