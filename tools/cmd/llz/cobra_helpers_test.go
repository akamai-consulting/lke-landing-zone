package main

// cobra_helpers_test.go — fixtures for tests that came with moved commands.

func clusterDef(name, extra string) string {
	return `apiVersion: llz.akamai-consulting.io/v1alpha1
kind: ClusterDefinition
metadata: { name: ` + name + ` }
spec:
  cluster:
    clusterLabel: inst-` + name + `
    region: us-ord
    bootstrap: { name: inst-` + name + ` }
    objectStorage: { cluster: us-ord-1 }
` + extra
}
