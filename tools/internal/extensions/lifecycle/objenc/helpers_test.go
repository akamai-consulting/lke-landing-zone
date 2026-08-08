package objenc

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"testing"
)

// testDeps is the Deps a unit test hands the encryption verbs.
//
// KVGet DEFAULTS TO KVUnknown, not KVAbsent, and that is the fixture doing the
// same job the production code does. Absent means "not seeded, mint one"; unknown
// means "refuse". A fixture defaulting to absent would let every seeding test pass
// through the minting branch without ever asserting that the refusal branch works
// — and the refusal branch is the one protecting every object already written.
func testDeps(t *testing.T) Deps {
	t.Helper()
	return Deps{
		KVGet:      func(string, string) (string, KVVerdict) { return "", KVUnknown },
		KVPut:      func(string, map[string]string) error { return nil },
		KubectlOut: func(...string) (string, error) { return "", nil },
		// SecretField does the REAL base64 decode. A no-op stub returned "" for
		// every field, which made each secret-reading case pass through the
		// "credential missing" branch — the assertion still ran, against nothing.
		SecretField: func(raw []byte, field string) (string, error) {
			var sec struct {
				Data map[string]string `json:"data"`
			}
			if err := json.Unmarshal(raw, &sec); err != nil {
				return "", err
			}
			v, ok := sec.Data[field]
			if !ok {
				return "", fmt.Errorf("no field %q", field)
			}
			b, err := base64.StdEncoding.DecodeString(v)
			return string(b), err
		},
		MaskGHALines: func(...string) {},
	}
}
