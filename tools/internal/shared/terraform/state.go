package terraform

// state.go — reading WHAT IS ALREADY IN TERRAFORM STATE, from `tofu show -json`.
//
// THE DEFECT THIS REPLACES. `tf-import` decided whether a resource was already
// managed with a helper that ran `terraform state show <addr>` and regexed the
// rendered `id = "…"` line, returning "" when it found nothing. That "" carried
// three unrelated meanings at once:
//
//   1. the address is genuinely absent from state (import it),
//   2. the command failed (state unreadable, backend not initialised),
//   3. the address IS in state but its id did not render as a quoted string.
//
// The caller read all three as (1) and imported. On gsap-apl's prod cluster,
// under the linode provider v4 bump, the LKE cluster hit (2)-or-(3) while the
// VPC, subnet and firewall next to it still hit the happy path — so tf-import
// announced `Importing module.cluster.linode_lke_cluster.this` for a cluster
// already in state and OpenTofu ended the apply with
// `Error: Resource already managed by OpenTofu`. Under linode v3.14.1 the same
// step printed `already in state — skipping`.
//
// WHY `show -json` AND NOT `state list` / `state show`. Measured against
// OpenTofu v1.12.5, because all three commands disagree about how they say
// "nothing there":
//
//   state list <addr>   absent address  → EXIT 1 ("Unknown resource instance")
//   state list          no state at all → EXIT 1 ("No state file was found")
//   state show <addr>   absent address  → EXIT 1
//   show -json          no state at all → EXIT 0, `{"format_version":"1.0"}`
//   show -json          state present   → EXIT 0, every address + its values
//
// Only `show -json` distinguishes "read the state, it holds nothing" from "could
// not read the state" by EXIT CODE rather than by matching English in a
// diagnostic. That matters because a greenfield first build legitimately has no
// state object at all, so "error" cannot simply be re-routed to fatal: the
// no-state case has to be a clean, empty answer or every first apply breaks.
//
// It is also the only one of the three that does not depend on how a provider
// renders a value — the id arrives as JSON, not as a line of HCL-shaped text
// that a regex has to survive.

import (
	"encoding/json"
	"fmt"
)

// StateIndex maps a resource address to the id recorded in state.
//
// PRESENCE AND ID ARE SEPARATE ANSWERS, and keeping them separate is the whole
// point: a resource can be in state with an id this code cannot read, and that
// is emphatically not the same as it being absent. Membership answers "must I
// import this?"; the value answers "what id do dependents need?" — and an empty
// value for a present key means "in state, id unavailable, go ask the API",
// never "import it".
type StateIndex map[string]string

// Has reports whether addr is managed in state.
func (s StateIndex) Has(addr string) bool { _, ok := s[addr]; return ok }

// ID returns the recorded id for addr, or "" when addr is absent OR present
// without a readable id. Callers must consult Has first; on a present address
// with no id the correct fallback is a live API lookup, not an import.
func (s StateIndex) ID(addr string) string { return s[addr] }

// showJSON is the subset of `tofu show -json` this needs. Everything else in
// that document (planned values, configuration, checks) is deliberately not
// modelled — an unknown field is not an error, so a future OpenTofu that adds
// one does not break the read.
type showJSON struct {
	Values *struct {
		RootModule stateModule `json:"root_module"`
	} `json:"values"`
}

type stateModule struct {
	Resources    []stateResource `json:"resources"`
	ChildModules []stateModule   `json:"child_modules"`
}

type stateResource struct {
	Address string `json:"address"`
	// json.RawMessage rather than a typed field: `id` is a string for every
	// resource llz imports today, but the type is the provider's to choose and a
	// number here must not fail the whole parse. Decoded leniently below.
	Values map[string]json.RawMessage `json:"values"`
}

// ParseStateIndex builds a StateIndex from `tofu show -json` output.
//
// An EMPTY document (`{"format_version":"1.0"}`, which is what a workspace with
// no state yet returns) yields an empty index and no error — that is the
// greenfield first build, where importing is exactly right.
//
// Malformed JSON is an ERROR, and returning one is the correction: the old code
// path's equivalent condition returned "nothing in state" and imported on top of
// live infrastructure.
func ParseStateIndex(showOutput []byte) (StateIndex, error) {
	var doc showJSON
	if err := json.Unmarshal(showOutput, &doc); err != nil {
		return nil, fmt.Errorf("parse `show -json` output: %w", err)
	}
	idx := StateIndex{}
	if doc.Values == nil {
		return idx, nil
	}
	walkStateModule(doc.Values.RootModule, idx)
	return idx, nil
}

// walkStateModule indexes a module and everything nested under it. Recursive
// because child_modules nest arbitrarily: llz's own resources sit one level down
// (module.cluster.*) and one at the root (linode_lke_node_pool.this), but a
// module that gains a submodule must not silently drop out of the index — the
// failure that produces is an import of a resource that is already managed.
func walkStateModule(m stateModule, idx StateIndex) {
	for _, r := range m.Resources {
		if r.Address == "" {
			continue
		}
		idx[r.Address] = stateResourceID(r)
	}
	for _, child := range m.ChildModules {
		walkStateModule(child, idx)
	}
}

// stateResourceID reads `values.id`, accepting either JSON shape a provider may
// use. A resource with no id at all is still INDEXED (with ""), because it is
// still in state.
func stateResourceID(r stateResource) string {
	raw, ok := r.Values["id"]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		return n.String()
	}
	return ""
}
