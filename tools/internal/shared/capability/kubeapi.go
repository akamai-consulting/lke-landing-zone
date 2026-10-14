package capability

// kubeapi.go — the handle for the IN-CLUSTER REST client, which was the second
// cluster-mutation transport and the one the fence could not see.
//
// ─────────────────────────────────────────────────────────────────────────────
// WHY Writer COULD NOT COVER IT. Writer fences the kubectl-argv path: it builds an
// argv, classifies the verb, and shells out. The reconcile daemon runs IN-POD and
// reaches the apiserver over HTTPS with its ServiceAccount token — there is no
// argv to classify and no kubectl to run. Two transports, one grant, and only one
// of them was policed. A lane could hold a REFUSING Writer and still MergePatch
// anything in the cluster.
//
// THAT IS THE CODE THAT LEAST DESERVES THE GAP. The lanes behind this client run
// continuously, unattended, with no human present when they act.
//
// STRUCTURAL TYPING, SO capability DOES NOT IMPORT kube. KubeClient below is
// satisfied by *kube.Client implicitly. The dependency would otherwise run
// capability -> kube, and capability is imported by every extension: anything it
// reaches becomes a dependency of all of them, which is the rule the model package
// already keeps.
//
// CLASSIFICATION IS BY METHOD, NOT BY ARGV, and that is what makes this simpler
// than Writer. The mutating surface is exactly CreateJSON and MergePatch, fixed at
// compile time, so there is no verb table to drift and no unclassified case to
// fail closed on. TestKubeClientMutatingSurfaceIsStillTwoMethods fails if that
// stops being true.
// ─────────────────────────────────────────────────────────────────────────────

import (
	"context"
	"fmt"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

// KubeClient is the in-cluster REST surface this handle fences. *kube.Client
// satisfies it implicitly.
//
// WATCH IS DELIBERATELY ABSENT, for two reasons that agree. It is a READ, and this
// model already holds that "the read grants are unrestricted, because reading is
// not what the reviewer is being protected from" — so fencing it would buy nothing
// the grant line does not already say. And its signature carries kube.WatchEvent,
// which would force this package to import kube and put that dependency in front of
// every extension. A lane keeps watching on the raw client and mutates through
// this.
type KubeClient interface {
	GetJSON(ctx context.Context, path string) (map[string]any, int, error)
	CreateJSON(ctx context.Context, path string, obj any) (int, error)
	MergePatch(ctx context.Context, path string, patch any) error
}

// KubeFor returns a REST client fenced by a binding's declared grants.
//
// READS NEED cluster-read OR cluster-write, mutations need cluster-write — the
// same implication Writer makes, and for the same reason: every mutating lane
// reads back what it wrote, and forcing it to declare both would make the grant
// line noisier without making it more informative.
//
// A BINDING WITH NEITHER GETS A REFUSING CLIENT, NOT NIL. The refusal is a
// permission error at the call site rather than a panic three frames away, which
// is the rule every handle in this package follows.
func KubeFor(b extension.Binding, inner KubeClient) KubeAPI {
	var read, write bool
	for _, g := range b.Grants {
		switch g {
		case extension.ClusterRead:
			read = true
		case extension.ClusterWrite:
			write = true
		}
	}
	if !read && !write {
		return deniedKube{}
	}
	return kubeAPI{inner: inner, mutate: write}
}

// KubeAPI is what a lane holds. Every method is present; the ones a binding did
// not earn refuse.
type KubeAPI interface {
	GetJSON(ctx context.Context, path string) (map[string]any, int, error)
	CreateJSON(ctx context.Context, path string, obj any) (int, error)
	MergePatch(ctx context.Context, path string, patch any) error
}

type kubeAPI struct {
	inner  KubeClient
	mutate bool
}

func (k kubeAPI) GetJSON(ctx context.Context, path string) (map[string]any, int, error) {
	return k.inner.GetJSON(ctx, path)
}

func (k kubeAPI) CreateJSON(ctx context.Context, path string, obj any) (int, error) {
	if !k.mutate {
		return 0, k.refuse("create", path)
	}
	return k.inner.CreateJSON(ctx, path, obj)
}

func (k kubeAPI) MergePatch(ctx context.Context, path string, patch any) error {
	if !k.mutate {
		return k.refuse("patch", path)
	}
	return k.inner.MergePatch(ctx, path, patch)
}

// refuse names the PATH as well as the verb. A lane's failure is about one object,
// and "cannot patch" without saying what is a message that sends someone to the
// wrong file.
func (kubeAPI) refuse(what, path string) error {
	return fmt.Errorf("capability: this binding did not declare %q, so it cannot %s %s "+
		"through the in-cluster API", extension.ClusterWrite, what, path)
}

// DeniedKube is the refusing client, exported so a caller with no binding at all
// can be explicit about it rather than passing nil.
func DeniedKube() KubeAPI { return deniedKube{} }

type deniedKube struct{}

func (deniedKube) err(what, path string) error {
	return fmt.Errorf("capability: this binding declared neither %q nor %q, so it has no "+
		"in-cluster API handle (attempted: %s %s)",
		extension.ClusterRead, extension.ClusterWrite, what, path)
}

func (d deniedKube) GetJSON(_ context.Context, path string) (map[string]any, int, error) {
	return nil, 0, d.err("get", path)
}
func (d deniedKube) CreateJSON(_ context.Context, path string, _ any) (int, error) {
	return 0, d.err("create", path)
}
func (d deniedKube) MergePatch(_ context.Context, path string, _ any) error {
	return d.err("patch", path)
}
