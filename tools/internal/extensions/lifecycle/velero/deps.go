package velero

// deps.go — what the drill is handed.
//
// It reaches three things: the cluster (to read the Schedule, create Backups,
// Restores and the canary, and read them back), the Linode API (to mint a
// temporary read-only key on the velero bucket), and — for the scrape check —
// this same binary, re-executed. Each handle is built from the restore-drill
// binding, so what the drill may do is what that binding declares.

import (
	"context"
	"os"
	"os/exec"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/baosnap"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

// Deps carries the drill's capabilities.
type Deps struct {
	// Cluster reads every object the drill watches, always as `-o json`.
	Cluster capability.Cluster
	// Writer is the drill's mutations: apply a Backup / Restore / the canary
	// objects, delete them again.
	Writer capability.Writer
	// Keys builds a Linode client able to mint and revoke the temporary bucket key.
	Keys func(token string) baosnap.KeyAPI
	// TempStore mints the read-only key and returns the bucket under it.
	TempStore func(ctx context.Context, api baosnap.KeyAPI, label, objCluster, bucket, endpoint, perms string) (baosnap.Store, func(), error)
	// Scrape re-executes `llz ci assert-scrape-targets` for Velero's monitor and
	// rule group, as the assert suite re-executes its lanes: that verb belongs to
	// assert-observability, and calling it as a subprocess keeps its seams and its
	// os.Exit out of this one.
	Scrape func(args ...string) error
}

// caps is built FROM THE DECLARATION, here, rather than installed by
// internal/cli — the same self-service shape as openbaobackup's.
var caps = defaultDeps()

func defaultDeps() Deps {
	b := DrillBinding()
	return Deps{
		Cluster: capability.MustCluster(b),
		Writer:  capability.MustWriter(b),
		Keys: func(token string) baosnap.KeyAPI {
			return capability.CloudFor(b).Client(token, 30*time.Second)
		},
		TempStore: baosnap.TempStore,
		Scrape:    reexecScrape,
	}
}

// DrillBinding is the binding every drill handle is built from.
func DrillBinding() extension.Binding { return Extension().MustBinding("restore-drill") }

func reexecScrape(args ...string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self, append([]string{"ci", "assert-scrape-targets"}, args...)...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}
