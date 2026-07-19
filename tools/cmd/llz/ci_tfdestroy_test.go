package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestRunCITFDestroy_TwoPhase(t *testing.T) {
	var calls [][]string
	prev := tfDestroyRunFn
	tfDestroyRunFn = func(_ io.Writer, args ...string) error {
		calls = append(calls, args)
		return nil
	}
	t.Cleanup(func() { tfDestroyRunFn = prev })

	var buf bytes.Buffer
	if err := runCITFDestroy(&buf, "primary.tfvars", "destroy-plan.bin", false); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatalf("want 2 terraform calls (plan+apply), got %d: %v", len(calls), calls)
	}
	plan := strings.Join(calls[0], " ")
	if !strings.Contains(plan, "plan -destroy") || !strings.Contains(plan, "-var-file=primary.tfvars") || !strings.Contains(plan, "-out=destroy-plan.bin") {
		t.Errorf("plan call = %v", calls[0])
	}
	if apply := strings.Join(calls[1], " "); apply != "apply destroy-plan.bin" {
		t.Errorf("apply call = %q, want 'apply destroy-plan.bin'", apply)
	}
}

func TestRunCITFDestroy_RefreshOnly(t *testing.T) {
	var calls [][]string
	prev := tfDestroyRunFn
	tfDestroyRunFn = func(_ io.Writer, args ...string) error {
		calls = append(calls, args)
		return nil
	}
	t.Cleanup(func() { tfDestroyRunFn = prev })

	var buf bytes.Buffer
	if err := runCITFDestroy(&buf, "primary.tfvars", "destroy-plan.bin", true); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatalf("refresh-only should make 1 call, got %d: %v", len(calls), calls)
	}
	got := strings.Join(calls[0], " ")
	if !strings.Contains(got, "apply -refresh-only -auto-approve") || !strings.Contains(got, "-var-file=primary.tfvars") {
		t.Errorf("refresh-only call = %v", calls[0])
	}
	if strings.Contains(got, "-destroy") {
		t.Errorf("refresh-only must not destroy: %v", calls[0])
	}
}

func TestRunCITFDestroy_RequiresVarFile(t *testing.T) {
	if err := runCITFDestroy(&bytes.Buffer{}, "", "p", false); err == nil {
		t.Error("want error when --var-file is empty")
	}
}
