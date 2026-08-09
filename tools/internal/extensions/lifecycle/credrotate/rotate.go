package credrotate

// credrotate — the shared rotation framework every `llz credentials` subcommand
// uses: argument resolution, the dry-run arming rule, the GitHub-secret fan-out,
// and the narrow Linode API slices each rotator needs.
//
// EXTRACTED AS INFRASTRUCTURE, NOT AS AN EXTENSION, and for the reason
// internal/baoread was: the credential family sits behind two walls, and this is
// the second. `credential-objkey`, `credential-pat` and `credential-linode` all
// reach `rotatorOpts`, `writeRotatedSecret` and a client constructor from here, so
// none of them could move while this stayed in package main. Go's rules make it
// literal — three files cannot share a type that lives in a package they are
// leaving.
//
// THE DRY-RUN RULE IS THE POINT OF Resolve, and it is why this is one framework
// rather than three copies. A rotator that is not armed must say so and change
// nothing; `--apply` OR `ROTATION_APPLY=true` arms it. Three subcommands
// re-implementing that would be three chances to default the wrong way, on code
// whose failure mode is deleting a live credential.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cli"
)

// SetSecret writes a GitHub secret, scoped to an environment when env != "".
//
// THE ONE SEAM. Package main owns the forge credentials and the writer; this
// package owns WHICH secrets a rotation must reach, which is the part the three
// rotators share. The default is inert rather than real: unlike a summary sink or
// a spec reader, a default that actually wrote secrets would make an
// un-installed caller mutate the operator's repo.
var SetSecret = func(name, env, value string) error { return nil }

// Install wires the capability main owns. Call once, before any rotation runs.
func Install(setSecret func(name, env, value string) error) { SetSecret = setSecret }

// firstNonEmpty returns the first non-empty string. Pure, localised — the sixth
// package in this campaign to reach that conclusion about these three lines.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// Opts is the global argument set every `llz credentials` subcommand
// shares — the cobra-flag equivalent of the hand-rolled argument preamble the
// standalone rotator binaries used before they were folded into llz. (That
// preamble, cli.ParseRotatorArgs, outlived its callers and has been removed.)
type Opts struct {
	Token string
	Apply bool
}

// Resolve applies the env defaults (LINODE_TOKEN / ROTATION_APPLY), emits the
// dry-run banner when unarmed, and returns the token + armed flag.
// Resolve applies the env defaults (LINODE_TOKEN / ROTATION_APPLY), emits the
// dry-run banner when unarmed, and returns the token + armed flag.
func (o *Opts) Resolve() (token string, apply bool, err error) {
	token = firstNonEmpty(o.Token, os.Getenv("LINODE_TOKEN"))
	if token == "" {
		return "", false, fmt.Errorf("a Linode PAT is required (env LINODE_TOKEN)")
	}
	apply = o.Apply || cli.EnvBool("ROTATION_APPLY", false)
	if !apply {
		slog.Warn("DRY-RUN: no Linode API write will be made. Pass --apply (or ROTATION_APPLY=true) to arm.")
	}
	return token, apply, nil
}

// WriteRotatedSecret persists a freshly-rotated account credential into the GitHub
// secret `name` for EVERY infra-<deployment> environment (or repo-level when
// deployments is empty). The shared Linode credentials (LINODE_API_TOKEN, the
// TF-state OBJ key) live as a per-environment copy — the infra-<env> environments
// are the main-only secret-injection boundary, so each deployment holds its own
// copy and a rotation must update all of them, not a single repo-level secret.
// WriteRotatedSecret persists a freshly-rotated account credential into the GitHub
// secret `name` for EVERY infra-<deployment> environment (or repo-level when
// deployments is empty). The shared Linode credentials (LINODE_API_TOKEN, the
// TF-state OBJ key) live as a per-environment copy — the infra-<env> environments
// are the main-only secret-injection boundary, so each deployment holds its own
// copy and a rotation must update all of them, not a single repo-level secret.
func WriteRotatedSecret(name, value string, deployments []string) error {
	if len(deployments) == 0 {
		return SetSecret(name, "", value) // repo-level fallback (pre-env-scoped instances)
	}
	for _, d := range deployments {
		if err := SetSecret(name, "infra-"+d, value); err != nil {
			return fmt.Errorf("set %s in infra-%s: %w", name, d, err)
		}
	}
	return nil
}

// Client constructors as package vars so the commands are exercisable without
// network access (same seam pattern as newKubeconfigClient / newACLClient).
// Client constructors as package vars so the commands are exercisable without
// network access (same seam pattern as newKubeconfigClient / newACLClient).
var (
	NewPATClient    = func(token string) PATAPI { return capability.CloudFor(patCloudBinding()).Client(token, 30*time.Second) }
	NewObjKeyClient = func(token string) ObjKeyAPI {
		return capability.CloudFor(objKeyCloudBinding()).Client(token, 30*time.Second)
	}
)

// PATAPI is the slice of the Linode client the PAT rotation uses.
// PATAPI is the slice of the Linode client the PAT rotation uses.
type PATAPI interface {
	CreateProfileToken(ctx context.Context, label, scopes, expiry string) (map[string]any, error)
	ListProfileTokens(ctx context.Context) ([]map[string]any, error)
	DeleteProfileToken(ctx context.Context, id uint64) error
}

// ObjKeyAPI is the slice of the Linode client the OBJ-key rotation uses.
// ObjKeyAPI is the slice of the Linode client the OBJ-key rotation uses.
type ObjKeyAPI interface {
	CreateObjectStorageKey(ctx context.Context, label, cluster, bucket, permissions string) (map[string]any, error)
	ListObjectStorageKeys(ctx context.Context) ([]map[string]any, error)
	DeleteObjectStorageKey(ctx context.Context, id uint64) error
}

// maskGHA asks GitHub Actions to redact a value from the log. Pure, localised —
// four lines, and package main keeps its own for the verbs that stayed.
func maskGHA(v string) {
	if os.Getenv("GITHUB_ACTIONS") != "" && v != "" {
		fmt.Printf("::add-mask::%s\n", v)
	}
}

// tfvarsValue returns the first `key = "value"` assignment in tfvars content.
// Pure, localised — internal/configreadiness keeps its own copy for the same
// reason, and internal/terraform owns the real parser for the fixed struct.
func tfvarsValue(content, key string) string {
	for _, line := range strings.Split(content, "\n") {
		i := strings.IndexByte(line, '=')
		if i < 0 || strings.TrimSpace(line[:i]) != key {
			continue
		}
		val := strings.TrimSpace(line[i+1:])
		if len(val) >= 2 && val[0] == '"' {
			if j := strings.IndexByte(val[1:], '"'); j >= 0 {
				return val[1 : 1+j]
			}
		}
		return val
	}
	return ""
}
