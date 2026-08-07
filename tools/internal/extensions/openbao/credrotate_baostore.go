package openbao

// credrotate_baostore.go — wires internal/credrotate's OpenBao login seam.
//
// Package main owns the in-cluster login path (the ServiceAccount token, the
// mounted CA, the address and mount defaults) and six other verbs use it; the
// rotation table owns WHICH ROLE a rotation logs in as. That is the split.

import (
	"context"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/openbao"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/credrotate"
)

func init() {
	credrotate.InstallBaoStore(func(ctx context.Context, role string) (openbao.BaoStore, error) {
		return openbao.OpenInClusterStore(ctx, role)
	})
}
