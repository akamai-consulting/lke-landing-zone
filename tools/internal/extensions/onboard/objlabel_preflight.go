package onboard

// objlabel_preflight.go — ask whether this instance's Object Storage bucket
// labels are still available, before an apply finds out the hard way.
//
// Linode bucket labels share ONE namespace per region, ACROSS ACCOUNTS. The
// objLabelPrefix work narrowed the collision — labels are now
// `<prefix>-loki-chunks-<env>` rather than the global `platform-...` — but it did
// not remove it: the prefix derives from the instance's repo short name, so two
// adopters who both call a repo `platform` still collide, and the namespace is
// still global. When it happens the operator gets the same thing they always did:
// `[400] The bucket '…' already exists`, mid-apply, out of Terraform, in CI,
// against a namespace they have no way to inspect.
//
// The question is answerable up front — `llz env add` and `llz doctor` already
// hold a Linode token and already call the object-storage API — and answering it
// up front is what the rest of this codebase does with every other unrecoverable
// mistake. A taken label costs one `llz spec set instance.objLabelPrefix=…` here,
// versus a failed apply and a rename there.
//
// Best-effort, like every other account-side check in the flow: no token, an
// unreachable API, or an ambiguous answer degrades to silence rather than
// blocking a build that would have worked. A HEAD that 404s proves availability;
// anything else proves nothing, and is treated as nothing.

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cli"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/linode"
)

// bucketLister is the slice of the Linode client this needs; seamed for tests.
type bucketLister interface {
	ListObjectStorageBuckets(ctx context.Context) ([]map[string]any, error)
}

// bucketPreflightClient returns a live client, or nil with no token.
var bucketPreflightClient = func() bucketLister {
	tok := firstNonEmpty(os.Getenv("LINODE_TOKEN"), os.Getenv("LINODE_API_TOKEN"))
	if tok == "" {
		return nil
	}
	return linode.NewClient(tok, 20*time.Second)
}

// objBucketLabels are the labels the object-storage root will create for a
// deployment, in the order the module creates them.
func objBucketLabels(prefix, env string) []string {
	return []string{
		prefix + "-loki-chunks-" + env,
		prefix + "-loki-ruler-" + env,
		prefix + "-loki-admin-" + env,
		prefix + "-harbor-registry-" + env,
	}
}

// checkBucketLabelsAvailable reports labels this instance intends to create that
// are ALREADY owned by this account — which is fine, it means a previous apply
// made them — and stays quiet otherwise.
//
// It cannot see another account's buckets: the Linode API lists only your own.
// So this catches the re-apply case and, crucially, gives the operator the
// vocabulary for the cross-account case when the apply does fail — which is why
// the remediation names objLabelPrefix rather than the deployment name. Renaming
// the deployment was the only remedy anyone documented, and it is the wrong one:
// it changes the state key, the tfvars, and the overlay directory.
func checkBucketLabelsAvailable(prefix, env string) {
	c := bucketPreflightClient()
	if c == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	have, err := c.ListObjectStorageBuckets(ctx)
	if err != nil {
		return
	}
	owned := map[string]bool{}
	for _, b := range have {
		owned[cli.AsString(b["label"])] = true
	}
	var mine []string
	for _, l := range objBucketLabels(prefix, env) {
		if owned[l] {
			mine = append(mine, l)
		}
	}
	if len(mine) == 0 {
		return
	}
	fmt.Printf("  %s %d of this deployment's bucket labels already exist on YOUR account (%s).\n",
		color.Yellow("!"), len(mine), color.Dim(mine[0]+", …"))
	fmt.Println(color.Dim("      Expected on a re-apply — Terraform adopts them. If an apply instead fails with"))
	fmt.Println(color.Dim("      `[400] … already exists`, the label is taken on ANOTHER account: Linode bucket"))
	fmt.Println(color.Dim("      labels share one namespace per region globally."))
	fmt.Printf("      %s\n", color.Dim("Fix that by changing the PREFIX, not the deployment name:"))
	fmt.Printf("      %s\n", color.Cyan("llz spec set instance.objLabelPrefix=<something-unique>"))
}
