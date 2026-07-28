package main

import (
	"errors"
	"testing"
)

const guardStateJSON = `{"values":{"root_module":{"child_modules":[{"resources":[
  {"address":"module.cluster.module.node_firewall.linode_firewall.this","values":{"id":"75288222"}}
]}]}}}`

func stubTFShow(t *testing.T, out string, err error) {
	t.Helper()
	prev := tfShowJSONFn
	tfShowJSONFn = func() (string, error) { return out, err }
	t.Cleanup(func() { tfShowJSONFn = prev })
}

// The firewall is tracked at the pre-migration SOURCE address, so an import into
// the destination must be suppressed — importing it twice is what blocks the
// `moved` block and makes terraform destroy the live firewall (gsap-apl 29701131691).
func TestTFStateAddrForIDFindsObjectAtAnotherAddress(t *testing.T) {
	stubTFShow(t, guardStateJSON, nil)
	got := tfStateAddrForID("75288222")
	if want := "module.cluster.module.node_firewall.linode_firewall.this"; got != want {
		t.Errorf("tfStateAddrForID = %q, want %q", got, want)
	}
}

// An object nothing tracks must still import.
func TestTFStateAddrForIDUnknownObject(t *testing.T) {
	stubTFShow(t, guardStateJSON, nil)
	if got := tfStateAddrForID("99999999"); got != "" {
		t.Errorf("tfStateAddrForID = %q, want \"\"", got)
	}
}

// Fail OPEN: a broken/absent state must not block a first import.
func TestTFStateAddrForIDFailsOpen(t *testing.T) {
	for name, tc := range map[string]struct {
		out string
		err error
	}{
		"terraform errored": {"", errors.New("no state")},
		"unparseable":       {"not json", nil},
		"empty":             {"", nil},
	} {
		t.Run(name, func(t *testing.T) {
			stubTFShow(t, tc.out, tc.err)
			if got := tfStateAddrForID("75288222"); got != "" {
				t.Errorf("want fail-open \"\", got %q", got)
			}
		})
	}
}

func TestTFStateAddrForIDEmptyID(t *testing.T) {
	stubTFShow(t, guardStateJSON, nil)
	if got := tfStateAddrForID(""); got != "" {
		t.Errorf("want \"\" for an empty id, got %q", got)
	}
}
