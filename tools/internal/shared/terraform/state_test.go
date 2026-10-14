package terraform

import "testing"

// The shapes below are real `tofu show -json` output (OpenTofu v1.12.5), trimmed
// to the fields ParseStateIndex reads.

const showJSONWithCluster = `{"format_version":"1.0","terraform_version":"1.12.5","values":{"root_module":{` +
	`"resources":[{"address":"linode_lke_node_pool.this","mode":"managed","type":"linode_lke_node_pool",` +
	`"values":{"id":"999","label":"pool"}}],` +
	`"child_modules":[{"address":"module.cluster","resources":[` +
	`{"address":"module.cluster.linode_lke_cluster.this","values":{"id":"635371","label":"gsap-apl-prod"}},` +
	`{"address":"module.cluster.linode_vpc.this[0]","values":{"id":"12345"}}` +
	`]}]}}}`

// The document a workspace with NO state yet returns — exit 0, no `values`.
const showJSONEmptyState = `{"format_version":"1.0"}`

func TestParseStateIndexFindsNestedAndRootResources(t *testing.T) {
	idx, err := ParseStateIndex([]byte(showJSONWithCluster))
	if err != nil {
		t.Fatalf("ParseStateIndex: %v", err)
	}
	// THE REGRESSION. This is the address tf-import tried to import while
	// OpenTofu was already managing it; it must resolve as present.
	if !idx.Has("module.cluster.linode_lke_cluster.this") {
		t.Error("LKE cluster nested in module.cluster must be found in state")
	}
	if got := idx.ID("module.cluster.linode_lke_cluster.this"); got != "635371" {
		t.Errorf("cluster id = %q, want 635371", got)
	}
	// A ROOT-level resource, which lives in a different part of the document.
	if !idx.Has("linode_lke_node_pool.this") || idx.ID("linode_lke_node_pool.this") != "999" {
		t.Error("root-level node pool must be found with its id")
	}
	// A COUNTED address keeps its index — importing the un-indexed form is the
	// orphaning bug the VPC comment in cobra_tf.go records.
	if !idx.Has("module.cluster.linode_vpc.this[0]") {
		t.Error("counted VPC address must be found with its [0] index")
	}
	if idx.Has("module.cluster.linode_firewall.this") {
		t.Error("an address not in the document must not be reported present")
	}
}

// NO STATE IS NOT AN ERROR. A greenfield first build has no state object, and
// importing is exactly the right thing to do there — so this path must return a
// clean empty index rather than a failure.
func TestParseStateIndexEmptyStateIsNotAnError(t *testing.T) {
	idx, err := ParseStateIndex([]byte(showJSONEmptyState))
	if err != nil {
		t.Fatalf("empty state must not error: %v", err)
	}
	if len(idx) != 0 {
		t.Errorf("empty state index = %v, want empty", idx)
	}
	if idx.Has("module.cluster.linode_lke_cluster.this") {
		t.Error("empty state must report nothing present")
	}
}

// MALFORMED IS AN ERROR, and this is the assertion that separates this code from
// what it replaced: the old helper answered "nothing is in state" when it could
// not read the state, and the caller imported on top of live infrastructure.
func TestParseStateIndexMalformedIsAnError(t *testing.T) {
	if _, err := ParseStateIndex([]byte("Error: Failed to load state\n")); err == nil {
		t.Fatal("malformed `show -json` output must return an error, not an empty index")
	}
}

// A provider is free to make `id` a number, and a resource may carry no id at
// all. Neither makes the resource unmanaged, so both must still index.
func TestParseStateIndexTolerantOfIDShape(t *testing.T) {
	doc := `{"values":{"root_module":{"resources":[` +
		`{"address":"a.numeric","values":{"id":635371}},` +
		`{"address":"b.none","values":{"label":"x"}}` +
		`]}}}`
	idx, err := ParseStateIndex([]byte(doc))
	if err != nil {
		t.Fatalf("ParseStateIndex: %v", err)
	}
	if got := idx.ID("a.numeric"); got != "635371" {
		t.Errorf("numeric id = %q, want 635371", got)
	}
	if !idx.Has("b.none") {
		t.Error("a resource with no id is still in state and must be present")
	}
	if got := idx.ID("b.none"); got != "" {
		t.Errorf("missing id = %q, want empty", got)
	}
}

// The fail-closed arms: a resource entry with no address at all, and an `id`
// that is neither a string nor a number. Neither may take the walk down with it
// — an index that stops early reports the resources after it as absent, and
// "absent" is what makes tf-import import.
func TestParseStateIndexSkipsUnusableEntriesWithoutLosingTheRest(t *testing.T) {
	doc := `{"values":{"root_module":{"resources":[` +
		`{"values":{"id":"orphaned"}},` +
		`{"address":"a.objectid","values":{"id":{"nested":1}}},` +
		`{"address":"b.nullid","values":{"id":null}},` +
		`{"address":"c.fine","values":{"id":"635371"}}` +
		`]}}}`
	idx, err := ParseStateIndex([]byte(doc))
	if err != nil {
		t.Fatalf("ParseStateIndex: %v", err)
	}
	if len(idx) != 3 {
		t.Errorf("index = %v, want the three addressed entries", idx)
	}
	for _, addr := range []string{"a.objectid", "b.nullid"} {
		if !idx.Has(addr) {
			t.Errorf("%s is in state and must be present even with an unreadable id", addr)
		}
		if got := idx.ID(addr); got != "" {
			t.Errorf("%s id = %q, want empty", addr, got)
		}
	}
	if got := idx.ID("c.fine"); got != "635371" {
		t.Errorf("an entry after the unusable ones must still index; got %q", got)
	}
}
