package callerperms

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func run(t *testing.T, root string) (string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	err := Run(root, &out, &errOut)
	return out.String(), errOut.String(), err
}

const wfDir = ".github/workflows/"

// TestUnderGrantedCallFires injects the EXACT defect: the delivered
// secret-rotation.yml's shape, where the callee needs id-token: write for OIDC
// and the caller holds only contents: read. GitHub answers that with
// startup_failure — no jobs, no logs — so this is the only place it can be seen.
func TestUnderGrantedCallFires(t *testing.T) {
	root := t.TempDir()
	write(t, root, wfDir+"secret-rotation.yml", `
permissions:
  contents: read
jobs:
  call:
    uses: ./.github/workflows/llz-secret-rotation.yml
`)
	write(t, root, wfDir+"llz-secret-rotation.yml", `
on: {workflow_call: {}}
permissions:
  contents: read
jobs:
  propagate:
    permissions:
      contents: read
      id-token: write
    steps: []
`)
	_, errOut, err := run(t, root)
	if err == nil {
		t.Fatal("an under-granted reusable call must fail — GitHub kills the run at startup")
	}
	for _, want := range []string{"secret-rotation.yml", "call", "id-token", "startup_failure"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("report must name %q:\n%s", want, errOut)
		}
	}
}

// TestJobLevelGrantCovers — the fix shape. A job-level block REPLACES the
// workflow-level one, so it has to list everything, and listing it must pass.
func TestJobLevelGrantCovers(t *testing.T) {
	root := t.TempDir()
	write(t, root, wfDir+"caller.yml", `
permissions:
  contents: read
jobs:
  call:
    permissions:
      contents: read
      id-token: write
    uses: ./.github/workflows/callee.yml
`)
	write(t, root, wfDir+"callee.yml", `
on: {workflow_call: {}}
jobs:
  j:
    permissions: {contents: read, id-token: write}
    steps: []
`)
	if _, errOut, err := run(t, root); err != nil {
		t.Fatalf("a covering grant must pass: %v\n%s", err, errOut)
	}
}

// TestWriteAllCovers — GitHub accepts the bare strings, and missing that form
// would report a caller holding write-all as holding nothing: a confident false
// finding, which is how a gate gets switched off.
func TestWriteAllCovers(t *testing.T) {
	root := t.TempDir()
	write(t, root, wfDir+"caller.yml", `
jobs:
  call:
    permissions: write-all
    uses: ./.github/workflows/callee.yml
`)
	write(t, root, wfDir+"callee.yml", `
on: {workflow_call: {}}
jobs:
  j:
    permissions: {id-token: write, packages: write}
    steps: []
`)
	if _, errOut, err := run(t, root); err != nil {
		t.Fatalf("write-all covers everything: %v\n%s", err, errOut)
	}
}

// TestReadOnlyCalleeNeedsNothingExtra — the common case must stay quiet, or the
// gate is noise. 13 of the 14 delivered caller/callee pairs are exactly this.
func TestReadOnlyCalleeNeedsNothingExtra(t *testing.T) {
	root := t.TempDir()
	write(t, root, wfDir+"caller.yml", "permissions:\n  contents: read\njobs:\n  call:\n    uses: ./.github/workflows/callee.yml\n")
	write(t, root, wfDir+"callee.yml", "on: {workflow_call: {}}\npermissions:\n  contents: read\njobs:\n  j:\n    steps: []\n")
	if _, errOut, err := run(t, root); err != nil {
		t.Fatalf("a read-only callee must not be reported: %v\n%s", err, errOut)
	}
}

// TestRemoteCallsAreOutOfScope — a remote reusable workflow cannot be read from
// this tree. Declining is right; guessing would be worse than not checking.
func TestRemoteCallsAreOutOfScope(t *testing.T) {
	root := t.TempDir()
	write(t, root, wfDir+"caller.yml", `
permissions: {contents: read}
jobs:
  remote:
    uses: other-org/other-repo/.github/workflows/x.yml@v1
  local:
    uses: ./.github/workflows/callee.yml
`)
	write(t, root, wfDir+"callee.yml", "on: {workflow_call: {}}\npermissions: {contents: read}\njobs:\n  j:\n    steps: []\n")
	if _, _, err := run(t, root); err != nil {
		t.Fatalf("a remote call must be skipped, not guessed at: %v", err)
	}
}

// TestFailsClosedOnAnEmptyCorpus — "OK, 0 scanned" is what a wrong --root looks
// like, and it is indistinguishable from a clean tree.
func TestFailsClosedOnAnEmptyCorpus(t *testing.T) {
	if _, _, err := run(t, t.TempDir()); err == nil {
		t.Fatal("no workflows at all must fail closed")
	}
}

// TestFailsClosedWhenNoLocalCallsExist — workflows present but no reusable call
// means the check examined nothing, which must not read as a pass.
func TestFailsClosedWhenNoLocalCallsExist(t *testing.T) {
	root := t.TempDir()
	write(t, root, wfDir+"plain.yml", "on: push\npermissions: {contents: read}\njobs:\n  j:\n    steps: []\n")
	_, _, err := run(t, root)
	if err == nil || !strings.Contains(err.Error(), "vacuous") {
		t.Fatalf("want the vacuity failure, got %v", err)
	}
}

// TestUnparseableWorkflowIsAnError — a file the guard cannot read is one it
// cannot vouch for.
func TestUnparseableWorkflowIsAnError(t *testing.T) {
	root := t.TempDir()
	write(t, root, wfDir+"bad.yml", "jobs:\n  a: [unclosed\n")
	if _, _, err := run(t, root); err == nil {
		t.Fatal("unparseable YAML must fail rather than be skipped")
	}
}

// TestReadEscalationIsAFinding pins the gap that let a real breakage ship.
//
// The guard compared only `level == "write"`, so a callee job asking
// `pull-requests: read` under a caller holding `contents: read` passed — and
// GitHub refuses to start that run exactly as it refuses a write escalation.
// llz-terraform.yml's changed-paths job did precisely this, and the guard
// reported "OK — 20 local reusable call(s) cover their callee" while every
// dispatch of that pipeline would have died at startup with no jobs and no logs.
func TestReadEscalationIsAFinding(t *testing.T) {
	caller := perms{"contents": "read"}
	if caller.holds("pull-requests", "read") {
		t.Error("contents:read does NOT cover pull-requests:read — GitHub sees pull-requests:none " +
			"and refuses to start the run")
	}
	if !caller.holds("contents", "read") {
		t.Error("contents:read must cover contents:read")
	}
	// none is not an ask.
	if !caller.holds("packages", "") {
		t.Error("an unrequested scope cannot be an escalation")
	}
	// write covers read; read does not cover write.
	w := perms{"contents": "write"}
	if !w.holds("contents", "read") {
		t.Error("write must cover read")
	}
	if caller.holds("contents", "write") {
		t.Error("read must not cover write")
	}
	// The wildcards still work, in both directions.
	if !(perms{"*": "read"}).holds("pull-requests", "read") {
		t.Error("read-all must cover any read")
	}
	if (perms{"*": "read"}).holds("contents", "write") {
		t.Error("read-all must not cover a write")
	}
	if !(perms{"*": "write"}).holds("anything", "write") {
		t.Error("write-all must cover any write")
	}
}
