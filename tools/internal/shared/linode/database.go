package linode

// database.go — the Linode Managed PostgreSQL credential endpoints used by
// `llz ci rotate-db-admin` (docs/designs/shared-managed-postgres.md).
//
// Only the CREDENTIAL surface is here. Provisioning is Terraform's job (the
// `databases` root); the rotator never creates or destroys a cluster, it only
// resets the admin password on one that already exists.
//
// The admin user is fixed to `akmadmin` on the Aiven-backed platform and cannot
// be changed or supplemented, which is what makes reset-in-place the only
// available rotation: there is no "mint a second credential, verify it, then
// drain the first" path the way there is for PATs and object-storage keys. That
// asymmetry is the whole reason the rotator is shaped the way it is — see the
// verify-AFTER-reset note in ci_rotate_dbadmin.go.

import (
	"context"
	"encoding/json"
	"fmt"
)

// DBCredentials is the admin credential pair a Managed Database reports.
type DBCredentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// ListPostgresInstances returns every Managed PostgreSQL cluster on the account.
// Used to resolve a cluster's live status before and after a reset.
func (c *Client) ListPostgresInstances(ctx context.Context) ([]map[string]any, error) {
	return c.listAllPages(ctx, "/v4/databases/postgresql/instances")
}

// PostgresInstance GETs one Managed PostgreSQL cluster. The `status` field is
// what the rotator waits on: a credential reset puts the cluster into `updating`
// and the new password is not servable until it returns to `active`.
func (c *Client) PostgresInstance(ctx context.Context, id uint64) (map[string]any, error) {
	resp, err := c.get(ctx, "v4", fmt.Sprintf("databases/postgresql/instances/%d", id))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GET database %d: HTTP %d: %s", id, resp.StatusCode, readBody(resp))
	}
	dec := json.NewDecoder(resp.Body)
	dec.UseNumber()
	var out map[string]any
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("GET database %d: parse response: %w", id, err)
	}
	return out, nil
}

// PostgresCredentials reads a cluster's CURRENT admin credentials.
//
// This is a privileged read that returns a live password in plaintext, so every
// caller must treat the result as secret material — the rotator masks it in GHA
// before it can reach a log.
func (c *Client) PostgresCredentials(ctx context.Context, id uint64) (DBCredentials, error) {
	resp, err := c.get(ctx, "v4", fmt.Sprintf("databases/postgresql/instances/%d/credentials", id))
	if err != nil {
		return DBCredentials{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return DBCredentials{}, fmt.Errorf("GET database %d credentials: HTTP %d: %s", id, resp.StatusCode, readBody(resp))
	}
	var creds DBCredentials
	if err := json.NewDecoder(resp.Body).Decode(&creds); err != nil {
		return DBCredentials{}, fmt.Errorf("GET database %d credentials: parse response: %w", id, err)
	}
	return creds, nil
}

// ResetPostgresCredentials regenerates a cluster's admin password.
//
// DESTRUCTIVE AND IMMEDIATE: the old password stops working as soon as the
// platform applies the reset. There is no overlap window and no way to choose
// the new password, so a caller cannot verify the replacement before committing
// to it — the commit already happened. Callers must therefore be certain they
// can PERSIST what comes back (see the rotator's fail-loud path), because a
// reset whose result is lost locks everyone out of the database.
//
// The reset is asynchronous: a 2xx here means accepted, not applied. Poll
// PostgresInstance for `status == "active"` and re-read PostgresCredentials.
func (c *Client) ResetPostgresCredentials(ctx context.Context, id uint64) error {
	url := fmt.Sprintf("%s/v4/databases/postgresql/instances/%d/credentials/reset", c.base, id)
	resp, err := c.post(ctx, url, map[string]any{})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("reset database %d credentials: HTTP %d: %s", id, resp.StatusCode, readBody(resp))
	}
	return nil
}
