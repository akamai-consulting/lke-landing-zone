package main

import (
	"os"
	"strings"
	"testing"
)

func withTempObjkeyStub(t *testing.T) *stubLinode {
	t.Helper()
	stub := &stubLinode{}
	prev := tempObjkeyLinodeClient
	tempObjkeyLinodeClient = func(string) rotatorLinodeAPI { return stub }
	t.Cleanup(func() { tempObjkeyLinodeClient = prev })
	return stub
}

func TestTempObjkeyCreate(t *testing.T) {
	t.Setenv("LINODE_API_TOKEN", "tok")
	stub := withTempObjkeyStub(t)
	envFile := withGHAEnvFile(t)

	if err := runCITempObjkeyCreate("e2e", "https://us-ord-1.linodeobjects.com",
		"platform-loki-chunks-e2e, platform-harbor-registry-e2e"); err != nil {
		t.Fatal(err)
	}
	if stub.objCreates != 1 {
		t.Fatalf("mints = %d, want 1", stub.objCreates)
	}
	b, _ := os.ReadFile(envFile)
	for _, want := range []string{"TEMP_OBJKEY_ID=201", "TEMP_OBJKEY_ACCESS=AK", "TEMP_OBJKEY_SECRET=SK"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("$GITHUB_ENV missing %q:\n%s", want, b)
		}
	}

	// Bad endpoint → refused before any mint.
	if err := runCITempObjkeyCreate("e2e", "https://example.com/not-obj", "b"); err == nil {
		t.Error("underivable OBJ cluster must error")
	}
	if err := runCITempObjkeyCreate("", "", ""); err == nil {
		t.Error("missing flags must error")
	}
}

func TestTempObjkeyDelete(t *testing.T) {
	t.Setenv("LINODE_API_TOKEN", "tok")
	stub := withTempObjkeyStub(t)

	// Unset id → clean no-op (create may have been skipped on a re-run).
	t.Setenv("TEMP_OBJKEY_ID", "")
	if err := runCITempObjkeyDelete(); err != nil {
		t.Fatalf("unset id must no-op, got %v", err)
	}
	if len(stub.deleted) != 0 {
		t.Errorf("no-op must not delete, got %v", stub.deleted)
	}

	t.Setenv("TEMP_OBJKEY_ID", "314")
	if err := runCITempObjkeyDelete(); err != nil {
		t.Fatal(err)
	}
	if len(stub.deleted) != 1 || stub.deleted[0] != 314 {
		t.Errorf("deleted = %v, want [314]", stub.deleted)
	}

	t.Setenv("TEMP_OBJKEY_ID", "not-a-number")
	if err := runCITempObjkeyDelete(); err == nil {
		t.Error("garbage id must error")
	}
}
