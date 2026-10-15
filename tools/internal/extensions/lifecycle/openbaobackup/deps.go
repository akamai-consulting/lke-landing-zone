package openbaobackup

// deps.go — what the restore drill is handed.
//
// The backup verb needs nothing from here: it runs in a pod and talks only to
// OpenBao and one bucket, through seams in backup.go. The DRILL runs in CI and
// reaches three things — the cluster (to run and read a Job, and to find the
// active pod), the Linode API (to mint a temporary bucket key), and a
// port-forward. Each is built from the restore-drill binding, so what it may do is
// what that binding declares.

import (
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/baosnap"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/openbao"
)

// Deps carries the drill's capabilities.
type Deps struct {
	// Cluster reads: the CronJob, the Job's status and logs, the active pod.
	Cluster capability.Cluster
	// Writer is the two mutations the drill makes to Kubernetes: apply the
	// exercise Job, and delete the previous one.
	Writer capability.Writer
	// Keys builds a Linode client able to mint and revoke the temporary bucket key.
	Keys func(token string) baosnap.KeyAPI
	// PortForwardPod opens a tunnel to one OpenBao pod's loopback listener.
	PortForwardPod func(pod string) (addr string, stop func(), err error)
}

// caps is built FROM THE DECLARATION, here, rather than installed by
// internal/cli: every handle comes from the restore-drill binding (MustCluster /
// MustWriter panic at package init if that binding ever loses the grant), so there is
// no second place a wider handle could be chosen. Same self-service shape as
// reconcilelanes' laneBinding. Tests replace it wholesale.
var caps = defaultDeps()

func defaultDeps() Deps {
	b := DrillBinding()
	return Deps{
		Cluster: capability.MustCluster(b),
		Writer:  capability.MustWriter(b),
		Keys: func(token string) baosnap.KeyAPI {
			return capability.CloudFor(b).Client(token, 30*time.Second)
		},
		PortForwardPod: openbao.PortForwardPod,
	}
}

// DrillBinding is the binding every drill handle is built from.
func DrillBinding() extension.Binding { return Extension().MustBinding("restore-drill") }
