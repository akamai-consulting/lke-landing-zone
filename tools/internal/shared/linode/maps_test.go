package linode

import (
	"encoding/json"
	"testing"
)

func TestMapTags(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
		want []string
	}{
		{"present", map[string]any{"tags": []any{"a", "b"}}, []string{"a", "b"}},
		{"skips-non-strings", map[string]any{"tags": []any{"a", 7, "b"}}, []string{"a", "b"}},
		{"missing", map[string]any{}, []string{}},
		{"wrong-type", map[string]any{"tags": "a,b"}, []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := MapTags(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("MapTags(%v) = %v, want %v", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("MapTags(%v)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestMapString(t *testing.T) {
	m := map[string]any{"label": "prod", "id": 7}
	if got := MapString(m, "label"); got != "prod" {
		t.Errorf("MapString label = %q, want prod", got)
	}
	if got := MapString(m, "missing"); got != "" {
		t.Errorf("MapString missing = %q, want empty", got)
	}
	if got := MapString(m, "id"); got != "" {
		t.Errorf("MapString of non-string = %q, want empty", got)
	}
}

func TestMapUint(t *testing.T) {
	// The Linode client decodes numbers as json.Number; float64 is the
	// encoding/json default. Both must yield the numeric value.
	cases := []struct {
		name string
		val  any
		want uint64
	}{
		{"json.Number", json.Number("613260"), 613260},
		{"float64", float64(42), 42},
		{"string-is-zero", "613260", 0},
		{"missing-is-zero", nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := map[string]any{}
			if tc.val != nil {
				m["id"] = tc.val
			}
			if got := MapUint(m, "id"); got != tc.want {
				t.Errorf("MapUint(%v) = %d, want %d", tc.val, got, tc.want)
			}
		})
	}
}

func TestMapIDString(t *testing.T) {
	if got := MapIDString(map[string]any{"id": json.Number("99")}); got != "99" {
		t.Errorf("MapIDString = %q, want 99", got)
	}
	if got := MapIDString(map[string]any{}); got != "" {
		t.Errorf("MapIDString(no id) = %q, want empty", got)
	}
	// id == 0 is treated as absent (formats to empty), matching mIDString.
	if got := MapIDString(map[string]any{"id": float64(0)}); got != "" {
		t.Errorf("MapIDString(id=0) = %q, want empty", got)
	}
}

func TestVolumeLinodeIDNull(t *testing.T) {
	cases := []struct {
		name string
		m    map[string]any
		want bool
	}{
		{"absent", map[string]any{}, true},
		{"explicit-null", map[string]any{"linode_id": nil}, true},
		{"attached", map[string]any{"linode_id": json.Number("5")}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := VolumeLinodeIDNull(tc.m); got != tc.want {
				t.Errorf("VolumeLinodeIDNull(%v) = %v, want %v", tc.m, got, tc.want)
			}
		})
	}
}
