package capability

// cloud.go — the handles a `cloud-read` or `cloud-mutate` binding receives.
//
// SEVENTH AND EIGHTH CAPABILITY, and between them the largest hole left in the
// vocabulary: 20 declarations of `cloud-read` and 21 of `cloud-mutate`, forty-one
// in total, with nothing behind either. Every one of them could destroy an LKE
// cluster, and the declaration said so or did not with equal effect.
//
// `cloud-mutate` is the most destructive grant in the model. Its holders delete
// clusters, detach and delete Volumes, tear down NodeBalancers and VPCs, and
// revoke tokens. Until now a binding declaring `cloud-read` could do all of it,
// because the only thing standing between the two was the word in the
// declaration.
//
// ────────────────────────────────────────────────────────────────────────────
// THIS IS THE EASIEST CONVERSION IN THE CAMPAIGN, NOT THE HARDEST, AND THAT IS
// WHY IT CAME AFTER THE REPO FENCE RATHER THAN BEFORE.
//
// `read-repo` had NO seam: 124 os.ReadFile calls across 40 packages, every one a
// separate conversion. The Linode client is the opposite shape:
//
//   - `linode.Client.do` is a TRUE chokepoint. `c.http.Do` appears exactly once
//     in the package, and all fifty-two Client methods funnel through it — the
//     function's own header says so, because it was written to collapse five
//     hand-rolled copies.
//   - The METHOD IS ALREADY A PARAMETER of that function. Nothing has to be
//     parsed out of an argv the way kubectl's verb does.
//   - There are 30 CONSTRUCTION sites against 106+ call sites, so the gate goes
//     where the client is built and every existing call keeps working untouched.
//   - Zero raw `linode-cli` exec. The census found none, so there is no escape
//     hatch to count and no allowlist to keep.
//
// So the classification is the whole design, and it fits in one table.
// ────────────────────────────────────────────────────────────────────────────
//
// HTTP GIVES THE CLASSIFICATION FOR FREE, which kubectl did not. `kubectl
// rollout status` reads and `kubectl rollout restart` writes, so capability.go
// needed a sub-verb table to tell them apart. HTTP has no such ambiguity: the
// method IS the mutation flag, and Linode's API uses it correctly. GET and HEAD
// read; POST, PUT, DELETE and PATCH change something.
//
// THE DEFAULT IS STILL REFUSAL. An unrecognised method is refused by BOTH handles
// rather than assumed safe — same rule as the kubectl verb table, and for the
// same reason: a method nobody has classified is a decision someone has to make,
// not a gap to fall through.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/linode"
)

// readMethods are the HTTP methods a `cloud-read` handle may issue.
var readMethods = map[string]bool{
	http.MethodGet:  true,
	http.MethodHead: true,
	// OPTIONS is a capability probe against the endpoint, not the resource.
	http.MethodOptions: true,
}

// mutateMethods are what `cloud-mutate` adds. Listed explicitly rather than
// derived as "not a read", so a method nobody has classified is refused by both
// and shows up as a decision rather than as a silent widening.
var mutateMethods = map[string]bool{
	http.MethodPost:   true,
	http.MethodPut:    true,
	http.MethodPatch:  true,
	http.MethodDelete: true,
}

// Cloud is the handle a `cloud-read` or `cloud-mutate` binding receives. Both
// grants produce the same INTERFACE and different permissions, exactly as Cluster
// does — a caller cannot tell from the type which it holds and should not try.
type Cloud interface {
	// Client returns a Linode client whose every request is checked against this
	// binding's grants. The returned *linode.Client is the ordinary one, so all
	// fifty-two of its methods keep working untouched.
	Client(token string, timeout time.Duration) *linode.Client
	// FromEnv is Client for the ambient-credential path (LINODE_TOKEN).
	FromEnv() (*linode.Client, context.Context, error)
	// Permits reports whether this handle would allow an HTTP method, without
	// issuing anything — the early-fail door every other handle here has.
	Permits(method string) error
}

// ErrNoCloudRead and ErrNoCloudMutate are the refusals. Distinct values rather
// than one message, for the reason ErrNoSecretRead and ErrNoSecretCustody are:
// "I may not look at the account" and "I may not change it" are different bugs,
// and only one of them can delete a cluster.
var (
	ErrNoCloudRead   = errors.New("this binding declares neither cloud-read nor cloud-mutate, so it may not reach the Linode API")
	ErrNoCloudMutate = errors.New("this binding does not declare cloud-mutate, so it may not change anything in the Linode account")
)

type cloud struct {
	read, mutate bool
}

func (c cloud) Permits(method string) error {
	m := strings.ToUpper(strings.TrimSpace(method))
	switch {
	case m == "":
		return fmt.Errorf("capability: refusing a Linode request with no HTTP method — " +
			"the method is what the grant is checked against, so a request that has none cannot be judged")
	case readMethods[m]:
		if c.read || c.mutate {
			return nil
		}
		return fmt.Errorf("%w (attempted: %s)", ErrNoCloudRead, m)
	case mutateMethods[m]:
		if c.mutate {
			return nil
		}
		return fmt.Errorf("capability: %s changes the Linode account and cannot go through a "+
			"cloud-read handle — declare %q. %w", m, extension.CloudMutate, ErrNoCloudMutate)
	default:
		return fmt.Errorf("capability: HTTP method %q is not classified — add it to readMethods or "+
			"mutateMethods in internal/shared/capability/cloud.go, which is a decision about "+
			"whether it changes the account", m)
	}
}

// policy is Permits in the shape linode.Client wants. The URL is carried into the
// refusal because "DELETE was refused" is far less useful than which resource it
// would have deleted.
func (c cloud) policy() linode.MethodPolicy {
	return func(method, url string) error {
		if err := c.Permits(method); err != nil {
			return fmt.Errorf("%w (%s %s)", err, method, url)
		}
		return nil
	}
}

func (c cloud) Client(token string, timeout time.Duration) *linode.Client {
	return linode.NewClient(token, timeout).WithPolicy(c.policy())
}

func (c cloud) FromEnv() (*linode.Client, context.Context, error) {
	cl, ctx, err := linode.ClientFromEnv()
	if err != nil {
		return nil, ctx, err
	}
	return cl.WithPolicy(c.policy()), ctx, nil
}

// deniedCloud is what a binding declaring NEITHER cloud grant receives. Non-nil
// and refusing, like every absent capability here: a nil handle panics at the
// call site and reads as a crash rather than as a permission fault.
//
// Note it still hands back a WORKING client object — one whose every request is
// refused. Returning nil would make the caller's next line a panic, and the
// caller would report "llz crashed" rather than "this binding may not do that".
type deniedCloud struct{}

func (deniedCloud) Permits(method string) error {
	return fmt.Errorf("%w (attempted: %s)", ErrNoCloudRead, strings.ToUpper(method))
}

func (d deniedCloud) Client(token string, timeout time.Duration) *linode.Client {
	return linode.NewClient(token, timeout).WithPolicy(func(method, url string) error {
		return fmt.Errorf("%w (%s %s)", d.Permits(method), method, url)
	})
}

func (d deniedCloud) FromEnv() (*linode.Client, context.Context, error) {
	cl, ctx, err := linode.ClientFromEnv()
	if err != nil {
		return nil, ctx, err
	}
	return cl.WithPolicy(func(method, url string) error {
		return fmt.Errorf("%w (%s %s)", d.Permits(method), method, url)
	}), ctx, nil
}

// DeniedCloud is the refusing handle, exported so a caller assembling its own
// Deps defaults to refusal rather than to nil.
func DeniedCloud() Cloud { return deniedCloud{} }

// ReadOnlyCloud is the handle for internal/verbs, THE TREE THAT DECLARES NOTHING.
//
// ─────────────────────────────────────────────────────────────────────────────
// IT EXISTS BECAUSE THE CLUSTER HAD A RULE THERE AND THE CLOUD DID NOT.
//
// seambypass_test.go's TestVerbsDoNotMutateTheCluster states the claim that tree
// lives under: "A COMMAND THAT ONLY LOOKS MUST ONLY LOOK". `llz doctor`,
// `llz argocd-diagnostics` and `llz phase-timing` read a platform and print it,
// and a mutating kubectl verb there is a command doing something its name does not
// say. That rule was enforced for kubectl and for nothing else.
//
// Three call sites in that tree built a Linode client with `linode.NewClient(tok,
// …)` and no policy at all — doctor's version check, onboard's bucket preflight,
// onboard's token wizard. The client they got carries PutControlPlaneACL and
// ResetPostgresCredentials. No verb called either, which is why this is a fence
// added while the count is still zero rather than a bug being fixed; the point of
// a fence is that it holds before anyone tests it.
//
// WHY NOT A BINDING. There is none to look up: internal/verbs declares no
// extension, which verbs_test.go pins deliberately. Minting one to satisfy
// CloudFor would be the one-line bypass the mint guard exists to refuse, and it
// would put a fake declaration in the tree specifically so a check would pass.
//
// WHY NOT A POLICY WRITTEN AT THE CALL SITE. The read/mutate split lives in
// readMethods and mutateMethods in this file, and a second copy in internal/verbs
// is the two-places-one-truth failure this repo has been burned by most. This
// returns the SAME cloud value CloudFor builds for a cloud-read binding, so the
// classification has exactly one home.
//
// IT IS NOT FOR internal/extensions, and TestExtensionsDoNotTakeTheUndeclaredCloud
// enforces that. An extension has a binding; taking this instead would be reaching
// past its own declaration for a capability it may not have declared, which is the
// same shape as calling kubectlprobe.Exec directly.
// ─────────────────────────────────────────────────────────────────────────────
func ReadOnlyCloud() Cloud { return cloud{read: true} }

// CloudFor builds the Cloud handle a binding's grants entitle it to.
//
// `cloud-mutate` IMPLIES `cloud-read`, for the reason cluster-write implies
// cluster-read and secret-custody implies secret-read: every mutating path reads
// back what it changed — the reaper lists before it deletes, rotation reads the
// token it is replacing — and forcing both onto the grant line would make it
// noisier without making it more informative. The converse does not hold, which
// is the entire point.
func CloudFor(b extension.Binding) Cloud {
	var read, mutate bool
	for _, g := range b.Grants {
		switch g {
		case extension.CloudRead:
			read = true
		case extension.CloudMutate:
			mutate = true
		}
	}
	if !read && !mutate {
		return deniedCloud{}
	}
	return cloud{read: read, mutate: mutate}
}

// ClassifiedMethods returns every HTTP method this package knows, for the
// disjointness test and so the refusal message above can be checked against it.
func ClassifiedMethods() (read, mutate []string) {
	for m := range readMethods {
		read = append(read, m)
	}
	for m := range mutateMethods {
		mutate = append(mutate, m)
	}
	sort.Strings(read)
	sort.Strings(mutate)
	return read, mutate
}
