package main

import (
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
)

// setHCLField writes a rendered HCL value through regexp replacement, where
// `$name`/`${name}` in the REPLACEMENT are expansion syntax unless the literal
// variant is used. Every value it writes comes from the spec, and spec.cluster.tags
// is free-form — nothing upstream constrains it — so `$` in a tag was silently
// eaten: `cost$1center` rendered as `"cost"`, tagging real infrastructure wrong
// with no error at any layer.
//
// Tags are the reachable case; the same applied to every assignment, including the
// databases map keys (those are additionally caught by validate.EnvName, but the
// writer must not depend on a validator three layers up to avoid corrupting data).
func TestRenderTfvars_DollarInValuesIsLiteral(t *testing.T) {
	base, err := tfrootExample("cluster")
	if err != nil {
		t.Fatalf("read embedded cluster tfvars.example: %v", err)
	}
	out := renderTfvars(base, clusterspec.ClusterTFVars(clusterspec.Cluster{
		ClusterLabel: "c", Region: "us-ord", K8sVersion: "v1.33",
		NodePool: clusterspec.NodePool{Type: "g8-dedicated-8-4", Count: 3},
		Tags:     []string{"cost$1center", "owner${team}", "plain"},
	}))
	for _, want := range []string{`"cost$1center"`, `"owner${team}"`, `"plain"`} {
		if !strings.Contains(out, want) {
			t.Errorf("tag %s was mangled by $-expansion; got:\n%s", want,
				lineWithPrefix(out, "tags"))
		}
	}
}

func lineWithPrefix(s, prefix string) string {
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(l, prefix) {
			return l
		}
	}
	return "(no " + prefix + " line)"
}
