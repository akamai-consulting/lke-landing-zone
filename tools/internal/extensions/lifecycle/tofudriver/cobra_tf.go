package tofudriver

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tf "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/terraform"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cliopts"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/linode"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/tfbin"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/tfvars"
	"github.com/spf13/cobra"
)

// cobra_tf.go — `llz ci tf-apply` and `tf-import`, with the self-heal loop.
//
// THEY STAYED IN PACKAGE MAIN LONGER THAN THEIR SIBLINGS because this extension
// declared only ASSERTIONS at `provisioned`, and an apply is a TRANSITION.
// Moving them meant arguing two new bindings first; the declaration carries them
// now, and TestEachVerbDeclaresWhatItDoes pins what each one may touch.
//
// runTF/runTeed came along: their only callers in ci.go were this cluster.

// cluster-resource terraform addresses (stable; match the bootstrap modules).
const (
	addrVPC     = "module.cluster.linode_vpc.this"
	addrSubnet  = "module.cluster.linode_vpc_subnet.nodes"
	addrCluster = "module.cluster.linode_lke_cluster.this"
	// The pool is a root-level resource and the firewall lives directly in
	// llz-cluster; both were modules (module.node_pool / module.node_firewall)
	// before the wrappers were inlined. `moved` blocks migrate existing state,
	// but an IMPORT names the post-move address, so these must be the new ones.
	addrNodePool = "linode_lke_node_pool.this"
	addrFirewall = "module.cluster.linode_firewall.this"
)

// clusterUnreachableSettle is how long Heal C waits for the LKE-E control plane
// to settle before re-planning after a transient "Kubernetes cluster
// unreachable" apply failure. A package var so tests can zero it.
var clusterUnreachableSettle = 30 * time.Second

func TFImportCmd() *cobra.Command {
	var region string
	var nonfatal bool
	c := &cobra.Command{
		Use:   "tf-import",
		Short: "idempotently import existing Linode cluster resources into TF state",
		Long: "Native port of terraform-linode-import.sh. Run from the cluster terraform\n" +
			"working directory: for each cluster resource (VPC, subnet, LKE cluster, node\n" +
			"pool, node firewall) not already in state, it finds the live resource by label\n" +
			"via the Linode API (fully paginated) and `terraform import`s it. Also seeds a\n" +
			"kubeconfig (real or stub) so the kubernetes/helm/kubectl providers can init.\n" +
			"Reads LINODE_TOKEN (or LINODE_API_TOKEN). --nonfatal logs+skips import failures\n" +
			"instead of aborting (destroy workflows only).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return RunCITFImport(cliopts.Global, region, nonfatal) },
	}
	c.Flags().StringVar(&region, "region", "", "tfvars prefix, e.g. primary (required)")
	c.Flags().BoolVar(&nonfatal, "nonfatal", false, "log+skip import failures instead of aborting (destroy only)")
	return c
}
func TFApplyCmd() *cobra.Command {
	var varFile, plan string
	c := &cobra.Command{
		Use:   "tf-apply",
		Short: "terraform apply with self-heal for known idempotent failure modes",
		Long: "Native port of terraform-apply-with-heal.sh. Runs `terraform apply` once; on\n" +
			"failure it matches a known pattern, applies a targeted heal, re-plans, and\n" +
			"retries ONCE.\n\n" +
			"Heal B: a duplicate Cloud Firewall label → find the existing firewall by label\n" +
			"(paginated) and `terraform import` it so the retry adopts it.\n" +
			"Heal C: a connection-level flake against api.linode.com → settle, then re-plan\n" +
			"and retry.\n" +
			"Heal D: the provider's read-back of a just-created firewall's devices failing on\n" +
			"Linode read-after-write consistency → settle, then re-plan and retry.\n\n" +
			"(Heal A was a phantom helm_release in state. The workspace holding every\n" +
			"helm_release was deleted, so it could no longer match; it is gone.)\n\n" +
			"Any other error passes through. Reads LINODE_TOKEN (or TF_VAR_linode_token)\n" +
			"for Heal B.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return RunCITFApply(cliopts.Global, plan, varFile) },
	}
	c.Flags().StringVar(&plan, "plan", "", "saved terraform plan file to apply (required)")
	c.Flags().StringVar(&varFile, "var-file", "", "tfvars file for re-plan/import (required)")
	return c
}
func RunCITFImport(g cliopts.Opts, region string, nonfatal bool) error {
	if region == "" {
		return fmt.Errorf("--region is required (the tfvars prefix, e.g. primary)")
	}
	// Token first, so a missing credential still reports before a missing tfvars
	// file — the order this verb has always failed in.
	client, ctx, err := capability.CloudFor(cloudBinding("plan")).FromEnv()
	if err != nil {
		return err
	}
	return runTFImport(ctx, g, client, region, nonfatal)
}

// runTFImport is the resource walk, split from the credential wiring above so a
// test can drive it with a client aimed at a stub server and a state snapshot
// that is not a real state file. The regression it exists to hold is behavioural
// — "an address already in state is never imported" — and that is not a property
// of any parser, so it could not be tested through the parse helper alone.
func runTFImport(ctx context.Context, g cliopts.Opts, client *linode.Client, region string, nonfatal bool) error {
	vars, varFile, err := tfvars.ReadRegion("", region)
	if err != nil {
		return err
	}
	labels := tf.DeriveLabels(vars)

	if err := EnsureKubeconfig(ctx, g, client, labels.Cluster); err != nil {
		return err
	}

	// ONE SNAPSHOT FOR THE WHOLE WALK, and re-reading between steps would buy
	// nothing: no import below adds any OTHER address in this list to state
	// (importing the VPC does not create the subnet; importing the cluster does
	// not create the pool), so each address's presence is unaffected by the
	// imports around it. Five `state show` execs became one `show -json`.
	//
	// A READ FAILURE IS FATAL. It used to be indistinguishable from "nothing is
	// in state", which is how tf-import came to import a cluster OpenTofu was
	// already managing — see terraform/state.go for the full account. An empty
	// snapshot (greenfield, no state object yet) is NOT a failure and still
	// arrives here as an empty index.
	idx, err := readStateIndex()
	if err != nil {
		return fmt.Errorf("read terraform state: %w", err)
	}

	// ── VPC (always fatal — fast, no cluster dependency, even under --nonfatal) ──
	// linode_vpc.this is a COUNTED resource (llz-cluster module:
	// `count = local.create_vpc ? 1 : 0`, create_vpc = vpc_id == ""), so for a
	// dedicated VPC its real state address is this[0]. A shared-VPC deployment
	// (vpc_network set) has no such resource — nothing to import here — but the
	// subnet below still needs the VPC id, so we resolve it either way. Importing
	// the un-indexed `.this` fails with "Configuration for import target does not
	// exist", which silently orphaned the VPC/subnet (they could not be re-adopted
	// into state) and surfaced as label-collisions on the next apply.
	dedicatedVPC := vars.VPCNetwork == ""
	addrVPCEff := addrVPC + "[0]"
	var vpcID string
	vpcInState := dedicatedVPC && idx.Has(addrVPCEff)
	if vpcInState {
		fmt.Printf("%s already in state — skipping\n", addrVPCEff)
		vpcID = idx.ID(addrVPCEff)
	}
	// The id, separately from the import decision. IN STATE WITH AN UNREADABLE ID
	// falls through to the API lookup for the id ALONE — vpcInState still
	// suppresses the import, because "I cannot read its id" is not evidence that
	// a resource is unmanaged.
	if vpcID == "" {
		vpcs, err := client.ListVPCs(ctx)
		if err != nil {
			return fmt.Errorf("list VPCs: %w", err)
		}
		if id, ok := linode.FindIDByLabel(vpcs, labels.VPC); ok {
			vpcID = strconv.FormatUint(id, 10)
			// Only a dedicated VPC is managed (and thus imported) by this root; a
			// shared VPC is owned by the vpc/<network> root — we just reuse its id
			// for the subnet import.
			if dedicatedVPC && !vpcInState {
				if _, err := TfImport(g, varFile, addrVPCEff, vpcID, false); err != nil {
					return err
				}
			}
		} else if !vpcInState {
			fmt.Printf("VPC %q not found in Linode — skipping import\n", labels.VPC)
		}
	}

	// ── VPC subnet (always fatal; needs the VPC id) ──
	if idx.Has(addrSubnet) {
		fmt.Printf("%s already in state — skipping\n", addrSubnet)
	} else if vpcID == "" {
		fmt.Println("No VPC id available — skipping subnet import")
	} else {
		vpcNum, _ := strconv.ParseUint(vpcID, 10, 64)
		subs, err := client.ListVPCSubnets(ctx, vpcNum)
		if err != nil {
			return fmt.Errorf("list subnets of vpc %s: %w", vpcID, err)
		}
		if sid, ok := linode.FindIDByLabel(subs, labels.Subnet); ok {
			if _, err := TfImport(g, varFile, addrSubnet, vpcID+","+strconv.FormatUint(sid, 10), false); err != nil {
				return err
			}
		} else {
			fmt.Printf("Subnet %q not found in VPC %s — skipping import\n", labels.Subnet, vpcID)
		}
	}

	// ── Node firewall (nonfatal-aware; account-unique label) ──
	//
	// BEFORE the cluster on purpose. The firewall import depends on nothing the
	// cluster/pool imports produce (it resolves by account-unique label), and the
	// cluster import is the one step that can burn its full fatal deadline on a
	// stuck Linode state-refresh. Ordered AFTER the cluster, that hang returns a
	// fatal error that aborts runTFImport before the firewall is ever adopted —
	// stranding the account's orphaned node firewall as a label collision the next
	// apply trips over (exactly the wedge a killed cluster import left behind).
	// Ordered here, the firewall lands in state regardless of how the cluster goes.
	if idx.Has(addrFirewall) {
		fmt.Printf("%s already in state — skipping\n", addrFirewall)
	} else {
		fws, err := client.ListFirewalls(ctx)
		if err != nil {
			return fmt.Errorf("list firewalls: %w", err)
		}
		if fid, ok := linode.FindIDByLabel(fws, labels.Firewall); ok {
			if _, err := TfImport(g, varFile, addrFirewall, strconv.FormatUint(fid, 10), !nonfatal); err != nil {
				return err
			}
		} else {
			fmt.Printf("Firewall %q not found — skipping import\n", labels.Firewall)
		}
	}

	// ── LKE cluster (nonfatal-aware; a failed import clears the id so the pool
	//    import is skipped too) ──
	clusterInState := idx.Has(addrCluster)
	clusterID := idx.ID(addrCluster)
	if clusterInState {
		fmt.Printf("%s already in state — skipping\n", addrCluster)
	}
	// Same split as the VPC: present-but-unreadable resolves the id from the API
	// so the node-pool import below still has one, and imports nothing.
	if clusterID == "" {
		ids, err := client.ClustersWithLabel(ctx, labels.Cluster)
		if err != nil {
			return fmt.Errorf("list clusters: %w", err)
		}
		if len(ids) > 0 {
			clusterID = strconv.FormatUint(ids[0], 10)
			if !clusterInState {
				ok, err := TfImport(g, varFile, addrCluster, clusterID, !nonfatal)
				if err != nil {
					return err
				}
				if !ok {
					clusterID = ""
				}
			}
		} else if !clusterInState {
			fmt.Printf("Cluster %q not found in Linode — skipping import\n", labels.Cluster)
		}
	}

	// ── LKE node pool (nonfatal-aware; needs the cluster id) ──
	if idx.Has(addrNodePool) {
		fmt.Printf("%s already in state — skipping\n", addrNodePool)
	} else if clusterID == "" {
		fmt.Println("No cluster id available — skipping node pool import")
	} else {
		cNum, _ := strconv.ParseUint(clusterID, 10, 64)
		pools, err := client.ListNodePools(ctx, cNum)
		if err != nil {
			return fmt.Errorf("list node pools of cluster %s: %w", clusterID, err)
		}
		if pid, ok := tf.SelectNodePoolID(pools, labels.NodePool); ok {
			if _, err := TfImport(g, varFile, addrNodePool, clusterID+","+strconv.FormatUint(pid, 10), !nonfatal); err != nil {
				return err
			}
		} else {
			fmt.Printf("Node pool %q not found by label or tag — skipping import\n", labels.NodePool)
		}
	}

	return nil
}
func RunCITFApply(g cliopts.Opts, plan, varFile string) error {
	if plan == "" || varFile == "" {
		return fmt.Errorf("--plan and --var-file are required")
	}
	if g.DryRun {
		fmt.Fprintf(os.Stderr, "→ (dry-run) terraform apply -auto-approve %s (with self-heal + one retry)\n", plan)
		return nil
	}

	// First attempt — the happy path. -no-color is load-bearing: the heal
	// parsers anchor on the plain "  with <addr>," diagnostic lines.
	applyLog, code, err := RunTeed(tfbin.Bin(), "apply", "-no-color", "-auto-approve", plan)
	if err != nil {
		return fmt.Errorf("could not run terraform apply: %w", err)
	}
	if code == 0 {
		return nil
	}

	healed := false

	// (Heal A — a phantom helm_release in state, repaired with `terraform state
	// rm` — was removed. It matched on the literal `helm_release.` address prefix,
	// and a136aa5 deleted the cluster-bootstrap workspace that held every
	// helm_release and kubernetes_* data source in the repo. Nothing in the
	// remaining roots can produce that address, so the branch was unreachable.)

	// ── Heal B: duplicate Cloud Firewall label ──
	if !healed && tf.FirewallCollision(applyLog) {
		if err := HealFirewallCollision(g, applyLog, varFile, code); err != nil {
			return err
		}
		healed = true
	}

	// ── Heal C: transient Linode API flake ──
	// No state to repair: a connection-level failure (TLS handshake, i/o timeout,
	// EOF) against api.linode.com mid-apply. Let it settle, then fall through to
	// the shared re-plan + re-apply — the re-plan is load-bearing here, since the
	// failed apply already created earlier resources and staled the saved plan.
	//
	// RETARGETED: this used to anchor on the LKE-E apiserver (linodelke.net /
	// :6443), because the flake it absorbed came from the cluster-bootstrap
	// workspace's kubernetes/helm providers. a136aa5 deleted that workspace and
	// nothing left dials the apiserver, so the anchor had gone dead. The surviving
	// roots talk to api.linode.com for the whole 20-30 minute cluster apply, which
	// is where transient blips actually happen now (Heal D exists because one such
	// class burned a cold e2e).
	if !healed && tf.TransientAPIFlake(applyLog) {
		fmt.Fprintf(os.Stderr, "::warning::Apply hit a transient Linode API flake (connection-level error against api.linode.com). Waiting %s to settle, then retrying.\n", clusterUnreachableSettle)
		time.Sleep(clusterUnreachableSettle)
		healed = true
	}

	// ── Heal D: transient Cloud Firewall device-read flake ──
	// No state to repair: the node firewall was created but the provider's
	// immediate read-back of its attached devices failed on Linode read-after-
	// write consistency ("Failed to Get Devices for Firewall <id>", usually with
	// terraform's generic "Provider returned invalid result object after apply").
	// A settle + shared re-plan + re-apply re-reads the now-consistent firewall
	// and succeeds. This class of flake burned a whole cold e2e create (run
	// 29655607246) that "no self-heal pattern detected" refused to retry.
	if !healed && tf.FirewallDeviceReadFlake(applyLog) {
		fmt.Fprintf(os.Stderr, "::warning::Apply hit a transient Cloud Firewall device-read flake (Linode read-after-write consistency). Waiting %s to settle, then retrying.\n", clusterUnreachableSettle)
		time.Sleep(clusterUnreachableSettle)
		healed = true
	}

	if !healed {
		return fmt.Errorf("terraform apply failed (exit %d); no self-heal pattern detected, not retrying", code)
	}

	fmt.Fprintln(os.Stderr, "::notice::Re-planning after state heal.")
	if err := RunTF("plan", "-no-color", "-out="+plan, "-var-file="+varFile); err != nil {
		return fmt.Errorf("re-plan failed after state heal: %w", err)
	}
	fmt.Fprintln(os.Stderr, "::notice::Retrying apply after state heal.")
	return RunTF("apply", "-no-color", "-auto-approve", plan)
}

// HealFirewallCollision resolves the colliding firewall by label (paginated) and
// imports it into the resource address terraform tried to create, so the retry
// adopts it instead of recreating.
func HealFirewallCollision(g cliopts.Opts, applyLog, varFile string, applyExit int) error {
	fwAddr := tf.ParseFirewallAddress(applyLog)
	if fwAddr == "" {
		return fmt.Errorf("firewall label collision detected but could not parse the resource address — original error stands (exit %d)", applyExit)
	}
	token := tfApplyLinodeToken()
	if token == "" {
		return fmt.Errorf("firewall collision but LINODE_TOKEN / TF_VAR_linode_token is unset — cannot look up the existing firewall (exit %d)", applyExit)
	}
	content, err := os.ReadFile(varFile)
	if err != nil {
		return fmt.Errorf("read %s: %w", varFile, err)
	}
	label := tf.ResolveFirewallLabel(tf.ParseTFVars(string(content)))

	client := capability.CloudFor(cloudBinding("plan")).Client(token, 60*time.Second)
	fws, err := client.ListFirewalls(context.Background())
	if err != nil {
		return fmt.Errorf("list firewalls: %w", err)
	}
	id, ok := linode.FindIDByLabel(fws, label)
	if !ok {
		return fmt.Errorf("firewall %q collided on create but was not found by label in the account — cannot import (exit %d)", label, applyExit)
	}
	fmt.Fprintf(os.Stderr, "::warning::Firewall label %q already exists (id=%d); importing it into %s so the retry adopts it.\n", label, id, fwAddr)
	if err := RunTF("import", "-var-file="+varFile, fwAddr, strconv.FormatUint(id, 10)); err != nil {
		return fmt.Errorf("terraform import %s %d failed — original apply error stands (exit %d): %w", fwAddr, id, applyExit, err)
	}
	return nil
}

// RunTF runs a terraform subcommand with inherited stdio.
func RunTF(args ...string) error {
	cmd := tfbin.Command(args...)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
	return cmd.Run()
}

// RunTeed runs a command streaming combined stdout+stderr to the terminal while
// also capturing it, and returns (output, exitCode, startErr). startErr is non-nil
// only when the process could not be started/observed; a non-zero terraform exit
// is reported via exitCode, not startErr.
func RunTeed(name string, args ...string) (string, int, error) {
	var buf bytes.Buffer
	w := io.MultiWriter(os.Stdout, &buf)
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = w, w
	err := cmd.Run()
	if err == nil {
		return buf.String(), 0, nil
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return buf.String(), ee.ExitCode(), nil
	}
	return buf.String(), -1, err
}

// EnsureKubeconfig writes generated/<cluster>-kubeconfig.yaml if absent so the
// kubernetes/helm/kubectl providers can initialise: the real kubeconfig when the
// cluster exists and the API serves it, otherwise a stub.
func EnsureKubeconfig(ctx context.Context, g cliopts.Opts, client *linode.Client, clusterLabel string) error {
	path := filepath.Join("generated", clusterLabel+"-kubeconfig.yaml")
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if g.DryRun {
		fmt.Fprintln(os.Stderr, "→ (dry-run) ensure kubeconfig "+path)
		return nil
	}
	if err := os.MkdirAll("generated", 0o755); err != nil {
		return fmt.Errorf("mkdir generated: %w", err)
	}
	var b64 string
	if ids, err := client.ClustersWithLabel(ctx, clusterLabel); err == nil && len(ids) > 0 {
		if kc, err := client.GetKubeconfig(ctx, ids[0]); err == nil {
			b64 = kc
		}
	}
	content, stub := tf.KubeconfigContent(b64)
	if stub {
		fmt.Printf("Kubeconfig unavailable for %q — writing stub for provider init\n", clusterLabel)
	} else {
		fmt.Printf("Kubeconfig written to %s\n", path)
	}
	return os.WriteFile(path, content, 0o600)
}

// TfImport runs `terraform import` for a resource, with the script's timeouts
// (300s fatal / 120s non-fatal). When fatal it returns the error; when non-fatal
// it logs a warning and returns (false, nil) so the caller can skip dependents.
// Honors --dry-run (prints, imports nothing). ok reports whether the resource is
// now in state.
func TfImport(g cliopts.Opts, varFile, addr, id string, fatal bool) (ok bool, err error) {
	fmt.Printf("Importing %s (id=%s)\n", addr, id)
	if g.DryRun {
		fmt.Fprintf(os.Stderr, "→ (dry-run) terraform import -var-file=%s %s %s\n", varFile, addr, id)
		return true, nil
	}
	timeout := 300 * time.Second
	if !fatal {
		timeout = 120 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := tfbin.CommandContext(ctx, "import", "-var-file="+varFile, addr, id)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	runErr := cmd.Run()
	// A context-deadline kill returns from Run() as an opaque "signal: killed"
	// (SIGKILL from CommandContext), which reads like an OOM/crash. When the
	// deadline is what fired, say so — a hung Linode state-refresh (e.g. a stuck
	// LKE cluster whose kubeconfig/pool read never returns) is then diagnosable at
	// a glance instead of sending the next reader digging through the runner log.
	if ctx.Err() == context.DeadlineExceeded {
		runErr = fmt.Errorf("timed out after %s (terraform import hung — likely a stuck Linode state-refresh)", timeout)
	}
	if runErr != nil {
		if fatal {
			return false, fmt.Errorf("import %s: %w", addr, runErr)
		}
		fmt.Printf("WARNING: import of %s timed out or failed — skipping (post-destroy API cleanup will delete it)\n", addr)
		return false, nil
	}
	return true, nil
}

// tfShowJSONFn is the `tofu show -json` exec seam. A package var on the same
// pattern as tfPlanRunFn, so a test can hand the walk a state snapshot without a
// backend, a provider or a state file.
var tfShowJSONFn = func() ([]byte, error) {
	cmd := tfbin.Command("show", "-json")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		// The stderr text is the whole diagnostic value here — `.Output()` alone
		// yields "exit status 1", which is what made the old helper's failure
		// branch indistinguishable from an empty state in the first place.
		return nil, fmt.Errorf("%s show -json: %w: %s", tfbin.Bin(), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// readStateIndex returns every address currently in state, with its id.
//
// See terraform/state.go for why this reads `show -json` rather than
// `state show` or `state list`, and why "no state at all" must be an empty
// answer rather than an error.
func readStateIndex() (tf.StateIndex, error) {
	out, err := tfShowJSONFn()
	if err != nil {
		return nil, err
	}
	return tf.ParseStateIndex(out)
}
