package main

// ci_harbor_steps.go — the shared helper left behind by the retired Harbor CI
// steps. `llz ci harbor-port-forward`, `harbor-ensure-project` and
// `harbor-smoke` were REMOVED along with the workflow's `harbor` job: the
// active-path provisioning runs IN-CLUSTER as the harbor-robot-provisioner
// CronJob (`llz ci harbor-provisioner`, ci_harbor_provisioner.go), which talks
// to harbor-core.harbor.svc directly — the port-forward existed only because
// HARBOR_URL is internal DNS the GitHub runner cannot resolve, and the
// ensure-project/smoke logic now lives inside the provisioner loop.

// baoKVGetField and its classifying sibling baoKVGetFieldOK now live in
// bao_read.go — the "" this returned on a sealed pod was gating credential
// overwrites.
