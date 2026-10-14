package capability

// baoadmin.go — the handle for OpenBao's ADMIN surface: policy, auth, token,
// operator and status. The half that is not KV.
//
// IT EXISTS BECAUSE Secrets AND Custodian DO NOT COVER IT. Those two handle
// reading and placing secret MATERIAL; what will not fit them is all one shape —
// configuring the store rather than putting things in it. Counted non-test:
//
//	operator generate-root  11    policy write   9    token lookup   3
//	auth enable              3    policy list    2    operator init  2
//	status --porcelain       2    token revoke   1    bao status     9 sites
//
// That is not a long tail. It is a second surface with more call sites than the
// KV one, and every one of them was going through an unconstrained `bao` argv.
//
// WHY NOT A NEW GRANT. The vocabulary already distinguishes reading secret
// material (`secret-read`) from holding and placing it (`secret-custody`), and
// these operations divide on the same line: asking whether a pod is unsealed is a
// read, writing a policy that grants access to every path is custody in the
// strongest sense the model has. A `secret-admin` would be a grant nobody has
// declared, retro-fitted onto sixteen bindings.
//
// IT IS WIRED TO baoread.ExecFn — A POD EXEC — AND THAT IS PART OF ITS CONTRACT,
// not an implementation detail. OpenBao is reached two ways in this tree: ExecFn
// runs `bao` INSIDE a named pod, and ExecStdin runs it against a configured
// address with stdin piped. They are different transports with different auth and
// different failure modes.
//
// A caller on ExecStdin is therefore NOT a caller that can be moved here by
// swapping the function. credential-pat's jwt login was converted and reverted for
// exactly this: its tests stubbed ExecStdin, the handle read through ExecFn, and
// the login failed against a fake that was never consulted. That double-seam trap
// is a recurring one, and it is caught only because the tests stub the SEAM rather
// than the behaviour.
//
// Expressing the address-based transport is a second handle or a second
// constructor; one caller is not enough to know which, so ExecStdin callers stay
// raw and stay counted.
//
// AND ONE THING IT DELIBERATELY DOES NOT SOLVE. `operator generate-root` is the
// quorum flow: it reads recovery-key shares from the operator's terminal in raw
// mode, one at a time. A one-shot argv handle cannot express that, exactly as it
// could not express `kubectl exec`. Those callers stay raw and stay counted; what
// this handle removes is the eleven-times-repeated need for an UNCONSTRAINED bao
// launcher just to reach the operations around it.

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

// BaoAction is what a `bao` argv asks for.
type BaoAction int

const (
	// BaoRead observes the store's configuration or liveness: status, policy
	// listing, token introspection, the audit-device list.
	BaoRead BaoAction = iota
	// BaoAdmin changes who may reach what: policies, auth methods, token
	// lifecycle, and the operator flows that mint or restore root.
	BaoAdmin
	// BaoUnclassified is an argv this table does not describe. Always refused.
	BaoUnclassified
)

func (a BaoAction) String() string {
	switch a {
	case BaoRead:
		return "read"
	case BaoAdmin:
		return "admin"
	default:
		return "unclassified"
	}
}

// LISTED EXPLICITLY, like writeVerbs and the forge tables. There is no property of
// the string "policy write" that says it can grant access to every path in the
// store; a reviewer has to know, and a table is where knowing gets written down.
var (
	baoReads = map[string]bool{
		"status": true, "policy list": true, "policy read": true,
		"token lookup": true, "audit list": true, "auth list": true,
		"secrets list": true, "operator members": true,
	}
	baoAdmins = map[string]bool{
		"policy write": true, "policy delete": true,
		"auth enable": true, "auth disable": true, "auth tune": true,
		"token revoke": true, "token create": true, "token renew": true,
		"secrets enable": true, "secrets disable": true,
		"audit enable": true, "audit disable": true,
		"operator init": true, "operator unseal": true, "operator seal": true,
		"operator generate-root": true, "operator rekey": true, "operator raft": true,
		"write": true, "delete": true,
	}
)

// ClassifyBao reports what a `bao` argv asks for. Exported so the refusal message
// and the tests agree with the checker about what an argv IS.
func ClassifyBao(args []string) BaoAction {
	if len(args) == 0 {
		return BaoUnclassified
	}
	// `kv` is the OTHER surface. It has typed operations on Secrets/Custodian and
	// must not be reachable through a general argv here, or the fail-closed verdict
	// those depend on could be bypassed by spelling the read differently.
	if args[0] == "kv" {
		return BaoUnclassified
	}
	key := args[0]
	if len(args) > 1 && !strings.HasPrefix(args[1], "-") {
		key = args[0] + " " + args[1]
	}
	switch {
	case baoAdmins[key]:
		return BaoAdmin
	case baoReads[key]:
		return BaoRead
	// A bare verb with a flag after it — `status --porcelain` — still classifies.
	case baoAdmins[args[0]]:
		return BaoAdmin
	case baoReads[args[0]]:
		return BaoRead
	default:
		return BaoUnclassified
	}
}

// BaoAdminHandle runs OpenBao's non-KV operations, if the binding's grants permit.
type BaoAdminHandle interface {
	// Run executes `bao` with args against a pod, returning stdout and stderr.
	Run(pod, token, stdin string, args ...string) (stdout, stderr string, err error)
	// Permits reports whether Run would be allowed, without running it.
	Permits(args ...string) error
}

type baoAdmin struct {
	exec    func(pod, token, stdin string, args ...string) (string, string, error)
	read    bool
	custody bool
}

func (b baoAdmin) Permits(args ...string) error {
	switch ClassifyBao(args) {
	case BaoRead:
		if !b.read {
			return fmt.Errorf("%w: `bao %s` reads the store's configuration",
				ErrNoBaoRead, strings.Join(args, " "))
		}
	case BaoAdmin:
		if !b.custody {
			return fmt.Errorf("%w: `bao %s` changes who may reach what",
				ErrNoBaoAdmin, strings.Join(args, " "))
		}
	default:
		return fmt.Errorf("%w: `bao %s` — add it to the table in capability/baoadmin.go, "+
			"or use Secrets/Custodian if it is KV", ErrBaoUnclassified, strings.Join(args, " "))
	}
	return nil
}

func (b baoAdmin) Run(pod, token, stdin string, args ...string) (string, string, error) {
	if err := b.Permits(args...); err != nil {
		return "", "", err
	}
	return b.exec(pod, token, stdin, args...)
}

var (
	ErrNoBaoRead       = errors.New("this binding declares neither secret-read nor secret-custody, so it may not read OpenBao's configuration")
	ErrNoBaoAdmin      = errors.New("this binding does not declare secret-custody, so it may not change OpenBao's policies, auth or token lifecycle")
	ErrBaoUnclassified = errors.New("unclassified bao argv, refused rather than assumed safe")
)

type deniedBaoAdmin struct{}

func (deniedBaoAdmin) Permits(args ...string) error {
	return fmt.Errorf("%w: `bao %s`", ErrNoBaoRead, strings.Join(args, " "))
}
func (d deniedBaoAdmin) Run(_, _, _ string, args ...string) (string, string, error) {
	return "", "", d.Permits(args...)
}

// DeniedBaoAdmin is the refusing handle, exported so a caller assembling its own
// Deps has somewhere safe to default to.
func DeniedBaoAdmin() BaoAdminHandle { return deniedBaoAdmin{} }

// baoAdminHandle builds the handle a binding's grants entitle it to. Custody
// implies read, for the reason it does on Secrets: every path that changes a
// policy reads it back.
func baoAdminHandle(b extension.Binding, exec func(pod, token, stdin string, args ...string) (string, string, error)) BaoAdminHandle {
	var read, custody bool
	for _, g := range b.Grants {
		switch g {
		case extension.SecretRead:
			read = true
		case extension.SecretCustody:
			custody = true
		}
	}
	if !read && !custody {
		return deniedBaoAdmin{}
	}
	return baoAdmin{exec: exec, read: read || custody, custody: custody}
}

// BaoActions returns the classified argv keys, so tests can agree with this table
// rather than transcribe it.
func BaoActions() (reads, admins []string) {
	for k := range baoReads {
		reads = append(reads, k)
	}
	for k := range baoAdmins {
		admins = append(admins, k)
	}
	sort.Strings(reads)
	sort.Strings(admins)
	return reads, admins
}
