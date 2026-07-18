package main

// ci_harbor_steps.go — the shared helper left behind by the retired Harbor CI
// steps. `llz ci harbor-port-forward`, `harbor-ensure-project` and
// `harbor-smoke` were REMOVED along with the workflow's `harbor` job: the
// active-path provisioning runs IN-CLUSTER as the harbor-robot-provisioner
// CronJob (`llz ci harbor-provisioner`, ci_harbor_provisioner.go), which talks
// to harbor-core.harbor.svc directly — the port-forward existed only because
// HARBOR_URL is internal DNS the GitHub runner cannot resolve, and the
// ensure-project/smoke logic now lives inside the provisioner loop.

import (
	"os"
	"strings"
)

// baoKVGetField reads one field of a KV path via the in-pod bao CLI, "" on any
// failure (unseeded path, sealed pod) — the bash `|| true`.
func baoKVGetField(path, field string) string {
	token := os.Getenv("OPENBAO_ROOT_TOKEN")
	out, _, err := baoExecFn(rootOpenbaoPod, token, "", "kv", "get", "-field="+field, path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}
