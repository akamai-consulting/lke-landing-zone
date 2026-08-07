package onboard

// tokens_statekey.go — keep `llz tokens` from leaking the state-bucket key it
// mints, and surface the ones earlier runs already leaked.
//
// THE LEAK. runTokens creates a bucket-scoped read_write OBJ key
// (`llz-tfstate-<repoSlug>`) early, then prompts for three GitHub PATs, then
// generates the state passphrase, and only THEN writes .llz/secrets.env. The
// secret half of an OBJ key is shown exactly once, so an interrupt anywhere in
// that window — Ctrl-C at a prompt, which is the natural move for an operator who
// realises they need to go create a PAT first — destroyed the only copy while the
// credential stayed live at Linode. The re-run found nothing cached and minted
// another. Nothing sweeps this label: `llz reap` and `llz ci reap-objkeys` target
// the loki/harbor labels and the in-cluster PAT, and the scheduled reaper drains
// a different label entirely. They accumulate, on the bucket holding the
// encrypted Terraform state.
//
// WHY REPORT RATHER THAN DRAIN. The obvious symmetry with
// drainSupersededObjKeys is wrong here. At the point the new key exists, the
// repo's TF_STATE_ACCESS_KEY still names the OLD one, and the push that
// supersedes it happens later and can fail. Deleting first would turn a failed
// push into an instance that cannot read its own state — trading a credential
// leak for an outage. So this names them and leaves the choice with the operator.

import (
	"context"
	"fmt"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cli"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
)

// objKeyLister is the slice of the Linode client this needs, so the report is
// testable without a network.
type objKeyLister interface {
	ListObjectStorageKeys(ctx context.Context) ([]map[string]any, error)
}

// stateKeyLabel is the label runTokens mints the state-bucket key under. Already
// instance-scoped (repoSlug of instance_repo), so a census over it cannot see
// another instance's keys.
func stateKeyLabel(instanceRepo string) string { return "llz-tfstate-" + repoSlug(instanceRepo) }

// reportOrphanedStateKeys names pre-existing keys under the state-key label.
//
// Silent when there are none, or when the answer cannot be obtained — an
// unanswerable listing is not evidence of a leak, and this must never block the
// wizard it runs inside.
func reportOrphanedStateKeys(ctx context.Context, c objKeyLister, instanceRepo string) {
	if c == nil {
		return
	}
	keys, err := c.ListObjectStorageKeys(ctx)
	if err != nil {
		return
	}
	label := stateKeyLabel(instanceRepo)
	var ids []uint64
	for _, k := range keys {
		if cli.AsString(k["label"]) != label {
			continue
		}
		if id, ok := cli.AsUint64(k["id"]); ok {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return
	}
	fmt.Printf("\n%s %d existing object-storage key(s) already carry %q.\n", color.Yellow("!"), len(ids), label)
	fmt.Println(color.Dim("      A key's secret half is shown once, so an existing one cannot be reused —"))
	fmt.Println(color.Dim("      this run mints a fresh key and the old ones are superseded by the push."))
	fmt.Printf("      %s %v\n", color.Dim("ids:"), ids)
	fmt.Println(color.Dim("      Once the push has landed and a Terraform run has succeeded, revoke them in"))
	fmt.Println(color.Dim("      the Linode console (Object Storage → Access Keys). Not automated: the repo"))
	fmt.Println(color.Dim("      still points at one of them until the push below succeeds."))
}

// checkpointEnvFiles writes the local caches mid-wizard so a credential that has
// already been created survives an interrupt. Best-effort: the authoritative
// write happens at the end of runTokens and reports its own errors, and a
// checkpoint that cannot be written must not abort a run that is otherwise fine.
func checkpointEnvFiles(secrets, vars map[string]string) {
	_ = WriteEnvFile(".llz/secrets.env", secrets)
	_ = WriteEnvFile(".llz/vars.env", vars)
}
