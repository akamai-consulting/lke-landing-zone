package openbao

import (
	"context"
	"testing"
)

func TestValidatePath(t *testing.T) {
	if err := ValidatePath("secret/app/db"); err != nil {
		t.Errorf("ValidatePath(secret/...) = %v, want nil", err)
	}
	if err := ValidatePath("kv/app"); err == nil {
		t.Error("ValidatePath(non-secret) = nil, want error")
	}
}

func TestWriteGetCurrentVersion(t *testing.T) {
	f := newFakeBao(t)
	c := f.client()
	ctx := context.Background()
	const path = "secret/app"

	// Absent secret: version 0, key not present.
	if v, err := c.CurrentVersion(ctx, path); err != nil || v != 0 {
		t.Errorf("CurrentVersion(absent) = (%d, %v), want (0, nil)", v, err)
	}
	if _, ok, err := c.Get(ctx, path, "k"); err != nil || ok {
		t.Errorf("Get(absent) ok = %v, err = %v, want (false, nil)", ok, err)
	}

	if err := c.Write(ctx, path, map[string]string{"k": "v1"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if val, ok, err := c.Get(ctx, path, "k"); err != nil || !ok || val != "v1" {
		t.Errorf("Get(k) = (%q, %v, %v), want (v1, true, nil)", val, ok, err)
	}
	if v, err := c.CurrentVersion(ctx, path); err != nil || v != 1 {
		t.Errorf("CurrentVersion = (%d, %v), want (1, nil)", v, err)
	}
	// Present secret, absent key.
	if _, ok, _ := c.Get(ctx, path, "missing"); ok {
		t.Error("Get(present secret, missing key) ok = true, want false")
	}
}

func TestWriteError(t *testing.T) {
	f := newFakeBao(t)
	f.failWrite = true
	if err := f.client().Write(context.Background(), "secret/app", map[string]string{"k": "v"}); err == nil {
		t.Error("Write against a 500 endpoint = nil, want error")
	}
}

func TestDataHash(t *testing.T) {
	f := newFakeBao(t)
	c := f.client()
	ctx := context.Background()
	const path = "secret/app"

	if _, err := c.DataHash(ctx, path); err == nil {
		t.Error("DataHash(absent) = nil error, want 'secret absent'")
	}
	if err := c.Write(ctx, path, map[string]string{"k": "v"}); err != nil {
		t.Fatal(err)
	}
	h1, err := c.DataHash(ctx, path)
	if err != nil || h1 == "" {
		t.Fatalf("DataHash = (%q, %v), want non-empty hash", h1, err)
	}
	if h2, _ := c.DataHash(ctx, path); h2 != h1 {
		t.Errorf("DataHash not deterministic: %q vs %q", h1, h2)
	}
}

func TestRollbackToZeroDeletes(t *testing.T) {
	f := newFakeBao(t)
	c := f.client()
	ctx := context.Background()
	const path = "secret/app"

	if err := c.Write(ctx, path, map[string]string{"k": "v"}); err != nil {
		t.Fatal(err)
	}
	// priorVersion 0 means "nothing existed before" -> delete the secret.
	if err := c.Rollback(ctx, path, 0); err != nil {
		t.Fatalf("Rollback(0): %v", err)
	}
	if v, _ := c.CurrentVersion(ctx, path); v != 0 {
		t.Errorf("after rollback-to-0, version = %d, want 0 (deleted)", v)
	}
}

func TestRollbackToPriorVersion(t *testing.T) {
	f := newFakeBao(t)
	c := f.client()
	ctx := context.Background()
	const path = "secret/app"

	if err := c.Write(ctx, path, map[string]string{"k": "v1"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Write(ctx, path, map[string]string{"k": "v2"}); err != nil {
		t.Fatal(err)
	}
	// Restoring v1 writes its data as a new version, so the latest read is v1.
	if err := c.Rollback(ctx, path, 1); err != nil {
		t.Fatalf("Rollback(1): %v", err)
	}
	if val, ok, err := c.Get(ctx, path, "k"); err != nil || !ok || val != "v1" {
		t.Errorf("after rollback to v1, Get(k) = (%q, %v, %v), want v1", val, ok, err)
	}
}
