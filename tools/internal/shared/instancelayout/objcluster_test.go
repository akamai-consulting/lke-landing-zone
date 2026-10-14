package instancelayout

// TestValidateOBJCluster followed ValidateOBJCluster into the layout hub.

import "testing"

func TestValidateOBJCluster(t *testing.T) {
	cases := []struct {
		in      string
		wantErr bool
	}{
		{"", false},         // empty is allowed (caller decides)
		{"us-sea-1", false}, // legacy cluster
		{"us-ord-1", false}, // legacy cluster
		{"ap-south-1", false},
		{"us-iad-1", false},
		{"us-sea-2", false},  // newer-generation cluster — valid
		{"us-ord-10", false}, // newer-generation cluster — valid
		{"us-east-12", false},
		{"us-sea", true},    // not a cluster id (no datacenter ordinal)
		{"ussea1", true},    // not a cluster id
		{"0.0.0.0/0", true}, // a CIDR, not a cluster id
		{"us-sea-1 ", true}, // trailing space → not a match
		{"US-SEA-1", true},  // uppercase → not a match
	}
	for _, c := range cases {
		err := ValidateOBJCluster(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("validateOBJCluster(%q) err=%v, wantErr=%v", c.in, err, c.wantErr)
		}
	}
}
