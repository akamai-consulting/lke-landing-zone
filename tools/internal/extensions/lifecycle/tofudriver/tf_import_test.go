package tofudriver

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cliopts"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/linode"
)

// tf_import_test.go — the gate on "tf-import never imports what is already in
// state".
//
// THE REGRESSION IT HOLDS. gsap-apl's prod apply, first run after the linode
// provider v4 bump: tf-import printed `already in state — skipping` for the VPC,
// the subnet and the firewall, then `Importing module.cluster.linode_lke_cluster
// .this (id=635371)` for a cluster OpenTofu was already managing, and the apply
// died on `Error: Resource already managed by OpenTofu`. Under linode v3.14.1 the
// same step had skipped it.
//
// WHY NO EXISTING LANE SAW IT. release-e2e is greenfield — the cluster genuinely
// is absent there and importing is correct — so the "already in state" branch is
// only ever exercised by a RE-apply of an existing instance, which no lane does.
// This test is the lane: it hands the walk a state snapshot that already holds
// every address and asserts nothing is imported.

// stubShowJSON swaps the `tofu show -json` seam for the duration of the test.
func stubShowJSON(t *testing.T, out string, err error) {
	t.Helper()
	prev := tfShowJSONFn
	tfShowJSONFn = func() ([]byte, error) { return []byte(out), err }
	t.Cleanup(func() { tfShowJSONFn = prev })
}

// importFixture puts the process in a workspace tf-import can run against: a
// tfvars file it can read and a kubeconfig already on disk, so EnsureKubeconfig
// short-circuits without reaching the API.
func importFixture(t *testing.T) *linode.Client {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	tfvars := "cluster_label = \"gsap-apl-prod\"\nnode_pool_label = \"gsap-apl-prod-pool\"\nregion = \"us-ord\"\n"
	if err := os.WriteFile(filepath.Join(dir, "prod.tfvars"), []byte(tfvars), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "generated"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "generated", "gsap-apl-prod-kubeconfig.yaml"), []byte("apiVersion: v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A server that FAILS the test if touched. With everything already in state
	// the walk must resolve every id from the snapshot and make no API call at
	// all — an unexpected request is itself a finding.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected Linode API call: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return linode.NewClient("tok", 0).WithBase(srv.URL)
}

// fullState is a snapshot holding every address the walk considers.
const fullState = `{"format_version":"1.0","values":{"root_module":{` +
	`"resources":[{"address":"linode_lke_node_pool.this","values":{"id":"999"}}],` +
	`"child_modules":[{"address":"module.cluster","resources":[` +
	`{"address":"module.cluster.linode_vpc.this[0]","values":{"id":"12345"}},` +
	`{"address":"module.cluster.linode_vpc_subnet.nodes","values":{"id":"777"}},` +
	`{"address":"module.cluster.linode_firewall.this","values":{"id":"888"}},` +
	`{"address":"module.cluster.linode_lke_cluster.this","values":{"id":"635371"}}` +
	`]}]}}}`

func TestImportSkipsEveryAddressAlreadyInState(t *testing.T) {
	client := importFixture(t)
	stubShowJSON(t, fullState, nil)

	var err error
	out := captureTFStdout(t, func() {
		// DryRun so a bug that DID reach the import would print rather than exec —
		// the assertion below is on that line, which is exactly the line the failed
		// prod run printed.
		err = runTFImport(context.Background(), cliopts.Opts{DryRun: true}, client, "prod", false)
	})
	if err != nil {
		t.Fatalf("runTFImport: %v", err)
	}
	if strings.Contains(out, "Importing ") {
		t.Errorf("nothing may be imported when every address is in state; got:\n%s", out)
	}
	for _, addr := range []string{
		"module.cluster.linode_vpc.this[0]",
		"module.cluster.linode_vpc_subnet.nodes",
		"module.cluster.linode_firewall.this",
		"module.cluster.linode_lke_cluster.this",
		"linode_lke_node_pool.this",
	} {
		if !strings.Contains(out, addr+" already in state — skipping") {
			t.Errorf("%s must be reported as already in state; got:\n%s", addr, out)
		}
	}
}

// A resource present in state with an UNREADABLE id must still not be imported.
// This is the precise shape of the production failure: the id was what the old
// helper could not get hold of, and it answered "not in state".
func TestImportSkipsInStateResourceWithNoReadableID(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile(filepath.Join(dir, "prod.tfvars"), []byte("cluster_label = \"gsap-apl-prod\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "generated"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "generated", "gsap-apl-prod-kubeconfig.yaml"), []byte("apiVersion: v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The cluster is in state but carries no id. The API is allowed here — the
	// walk is entitled to resolve the id it needs for the node-pool import — but
	// an IMPORT of the cluster is not.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[],"page":1,"pages":1,"results":0}`)
	}))
	t.Cleanup(srv.Close)
	client := linode.NewClient("tok", 0).WithBase(srv.URL)

	noID := `{"values":{"root_module":{"child_modules":[{"address":"module.cluster","resources":[` +
		`{"address":"module.cluster.linode_lke_cluster.this","values":{"label":"gsap-apl-prod"}}` +
		`]}]}}}`
	stubShowJSON(t, noID, nil)

	var err error
	out := captureTFStdout(t, func() {
		err = runTFImport(context.Background(), cliopts.Opts{DryRun: true}, client, "prod", false)
	})
	if err != nil {
		t.Fatalf("runTFImport: %v", err)
	}
	if strings.Contains(out, "Importing module.cluster.linode_lke_cluster.this") {
		t.Errorf("an in-state cluster with an unreadable id must not be imported; got:\n%s", out)
	}
	if !strings.Contains(out, "module.cluster.linode_lke_cluster.this already in state — skipping") {
		t.Errorf("the cluster must still be reported as in state; got:\n%s", out)
	}
}

// A state read that FAILS must abort, not fall through to importing. The old
// helper's error branch returned "" — the same value it returned for "absent" —
// which is how a read failure became an import against live infrastructure.
func TestImportAbortsWhenStateCannotBeRead(t *testing.T) {
	client := importFixture(t)
	stubShowJSON(t, "", errTFShow)

	var err error
	_ = captureTFStdout(t, func() {
		err = runTFImport(context.Background(), cliopts.Opts{DryRun: true}, client, "prod", false)
	})
	if err == nil {
		t.Fatal("an unreadable state must abort tf-import, not be treated as an empty state")
	}
	if !strings.Contains(err.Error(), "read terraform state") {
		t.Errorf("error should name the state read; got %v", err)
	}
}

// captureTFStdout collects what the walk printed. The verb's stdout IS the
// operator record the failed prod run was read from, so the assertions are on it.
func captureTFStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	w.Close()
	os.Stdout = orig
	return <-done
}

// errTFShow stands in for whatever `tofu show -json` failed with.
var errTFShow = errors.New("tofu show -json: exit status 1: Failed to load state")
