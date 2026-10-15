package cnpgbackup

// deps.go — what the gate is handed.
//
// It reaches three things: the cluster (to read the Cluster, ObjectStore and
// Backup, create a Backup and — in the drill — the canary, a scratch recovery
// cluster and a verify Job), the Linode API (to mint a temporary read-only key on
// the cnpg bucket), and this same binary, re-executed, for the rule-group check.
// Each handle is built from the restore-drill binding, so what the gate may do is
// what that binding declares.

import (
	"context"
	"os"
	"os/exec"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/baosnap"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

// Deps carries the gate's capabilities.
type Deps struct {
	// Cluster reads every object the gate watches, always as `-o json`, and the
	// verify Job's log.
	Cluster capability.Cluster
	// Writer is the gate's mutations.
	Writer capability.Writer
	// Keys builds a Linode client able to mint and revoke the temporary bucket key.
	Keys func(token string) baosnap.KeyAPI
	// TempStore mints the read-only key and returns the bucket under it.
	TempStore func(ctx context.Context, api baosnap.KeyAPI, label, objCluster, bucket, endpoint, perms string) (baosnap.Store, func(), error)
	// Scrape re-executes `llz ci assert-scrape-targets` for the component's rule
	// group: that verb belongs to assert-observability, and a subprocess keeps its
	// seams and its os.Exit out of this one (velero does the same).
	Scrape func(args ...string) error
}

// caps is built FROM THE DECLARATION, here, rather than installed by
// internal/cli — the same self-service shape as velero's and openbaobackup's.
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

// DrillBinding is the binding every handle is built from.
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
