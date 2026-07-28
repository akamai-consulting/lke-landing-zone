package terraform

import "encoding/json"

// stateids.go answers "is this cloud object ALREADY tracked in state, at any
// address?" — the question an importer that resolves resources by label has to
// ask before adopting one.
//
// `llz ci tf-import` used to ask only "is the destination address populated?".
// That is a different question, and during a `moved` migration the two disagree:
// the object sits at the pre-migration SOURCE address, the destination is empty,
// so the importer adopts it a second time. Terraform then finds the destination
// occupied, cannot apply the `moved` block, and plans to destroy the source —
// which is the same live object. That is how akamai/gsap-apl run 29701131691
// deleted node firewall 75288222 and then failed its node pool with
// `[400] [firewall_id] The provided ID did not match any existing Firewalls`.

// stateShow mirrors the subset of `terraform show -json` this needs: every
// resource's address + id, recursively through child modules.
type stateShow struct {
	Values struct {
		RootModule stateModule `json:"root_module"`
	} `json:"values"`
}

type stateModule struct {
	Resources []struct {
		Address string `json:"address"`
		Values  struct {
			ID json.RawMessage `json:"id"`
		} `json:"values"`
	} `json:"resources"`
	ChildModules []stateModule `json:"child_modules"`
}

// StateIDs parses `terraform show -json` and returns id → state address for every
// resource in state, including nested modules. Ids are compared as strings
// because terraform renders them as either a JSON string or a number depending on
// the provider (Linode ids arrive both ways across resource types).
//
// An empty/unparseable state yields an empty map and no error: callers use this
// as a guard, and a missing state must not block a first import.
func StateIDs(showJSON string) (map[string]string, error) {
	if len(showJSON) == 0 {
		return map[string]string{}, nil
	}
	var s stateShow
	if err := json.Unmarshal([]byte(showJSON), &s); err != nil {
		return nil, err
	}
	out := map[string]string{}
	var walk func(m stateModule)
	walk = func(m stateModule) {
		for _, r := range m.Resources {
			if id := normalizeJSONID(r.Values.ID); id != "" {
				// First address wins — deterministic, and any duplicate is exactly
				// the aliasing this guard exists to report.
				if _, dup := out[id]; !dup {
					out[id] = r.Address
				}
			}
		}
		for _, c := range m.ChildModules {
			walk(c)
		}
	}
	walk(s.Values.RootModule)
	return out, nil
}

// normalizeJSONID renders a resource id as a string whether terraform emitted it
// quoted ("75288222") or bare (75288222).
func normalizeJSONID(raw json.RawMessage) string {
	if len(raw) == 0 {
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
