package healthsla

// cobra_deps.go — the shell-out seam the moved command reaches through.
//
// execOutput delegates to kubectlprobe.Exec through a CLOSURE, never by
// assignment: an assignment snapshots whatever the seam pointed at when this
// package initialised, which defeats a test that swaps it later.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"

func execOutput(name string, args ...string) ([]byte, error) { return kubectlprobe.Exec(name, args...) }
