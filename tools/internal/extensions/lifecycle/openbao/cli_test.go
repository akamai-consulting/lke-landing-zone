package openbao

import (
	"reflect"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/openbao"
)

func TestBaoExecArgv(t *testing.T) {
	got := openbao.ExecArgv("platform-openbao-0", "s.tok", []string{"policy", "list"})
	// Targets the LOOPBACK listener (8210) and VERIFIES the server against the
	// pod-mounted CA. The network listener (8200) now requires a client
	// certificate that an exec'd `bao` does not carry, and the openbao-tls cert
	// gained a 127.0.0.1 SAN so skip-verify is no longer needed here.
	//
	// The BAO_* names must come through, not only the VAULT_* aliases: the chart
	// puts BAO_ADDR=…:8200 in the container, and OpenBao prefers a present BAO_*
	// over VAULT_* unconditionally, so a VAULT_ADDR-only argv is silently
	// overridden back onto the mTLS listener. See baoread.LoopbackEnv.
	want := []string{
		"-n", "llz-openbao", "exec", "-i", "-c", "openbao", "platform-openbao-0", "--",
		"env",
		"BAO_ADDR=https://127.0.0.1:8210", "BAO_CACERT=/openbao/tls/ca.crt",
		"VAULT_ADDR=https://127.0.0.1:8210", "VAULT_CACERT=/openbao/tls/ca.crt",
		"BAO_TOKEN=s.tok", "VAULT_TOKEN=s.tok", "bao",
		"policy", "list",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExecArgv\n got: %v\nwant: %v", got, want)
	}
	// bao's own flags must survive untouched as trailing args.
	got2 := openbao.ExecArgv("platform-openbao-0", "t", []string{"write", "-f", "auth/approle/role/x/secret-id", "-format=json"})
	tail := got2[len(got2)-4:]
	if !reflect.DeepEqual(tail, []string{"write", "-f", "auth/approle/role/x/secret-id", "-format=json"}) {
		t.Errorf("bao flags not passed through: %v", tail)
	}
}

// `exec` sets SetInterspersed(false) so bao's own flags pass through cobra
// WITHOUT a `--` separator (the workflow callers omit it). Lock that in, and
// confirm the explicit `--` form still works.
