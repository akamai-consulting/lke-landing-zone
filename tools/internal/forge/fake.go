package forge

import "context"

// Fake is an in-memory Forge for tests: it records mutations and serves reads
// from its own maps, so command tests can assert "what did llz try to do to the
// forge?" without a gh binary or network. Replaces the ad-hoc package-var seams
// (ghSetSecretFn, ghSetRepoSecretFn) with a single injectable backend.
type Fake struct {
	FlavorVal Flavor
	Secrets   map[string]string // key: scopeKey(scope)+"/"+name
	Vars      map[string]Variable
	Workflows []WorkflowCall
	Repos     []RepoCall
	Locks     []LockCall

	// APIFunc serves API(); nil returns (nil, nil) so reads degrade to "nothing
	// configured", matching the production callers that treat 404s as empty.
	APIFunc func(ctx context.Context, req APIRequest) ([]byte, error)
}

// LockCall records one LockEnvironmentToBranch invocation.
type LockCall struct {
	Env    string
	Branch string
}

// WorkflowCall records one RunWorkflow invocation.
type WorkflowCall struct {
	Workflow string
	Fields   map[string]string
}

// RepoCall records one CreateRepo invocation.
type RepoCall struct {
	SrcDir  string
	Private bool
}

var _ Forge = (*Fake)(nil)

// NewFake returns a Fake with initialized maps (GitHub flavor by default).
func NewFake() *Fake {
	return &Fake{FlavorVal: GitHub, Secrets: map[string]string{}, Vars: map[string]Variable{}}
}

func (f *Fake) Flavor() Flavor {
	if f.FlavorVal == "" {
		return GitHub
	}
	return f.FlavorVal
}

func scopeKey(s Scope) string {
	if s.Env != "" {
		return "env:" + s.Env
	}
	return "repo"
}

func (f *Fake) SetSecret(_ context.Context, name, value string, scope Scope) error {
	f.Secrets[scopeKey(scope)+"/"+name] = value
	return nil
}

func (f *Fake) SetVariable(_ context.Context, name, value string, scope Scope) error {
	f.Vars[scopeKey(scope)+"/"+name] = Variable{Name: name, Value: value}
	return nil
}

func (f *Fake) SecretNames(_ context.Context, scope Scope) ([]string, error) {
	prefix := scopeKey(scope) + "/"
	var names []string
	for k := range f.Secrets {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			names = append(names, k[len(prefix):])
		}
	}
	return names, nil
}

func (f *Fake) Variables(_ context.Context, scope Scope) ([]Variable, error) {
	prefix := scopeKey(scope) + "/"
	var vars []Variable
	for k, v := range f.Vars {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			vars = append(vars, v)
		}
	}
	return vars, nil
}

func (f *Fake) CreateRepo(_ context.Context, srcDir string, private bool) error {
	f.Repos = append(f.Repos, RepoCall{SrcDir: srcDir, Private: private})
	return nil
}

func (f *Fake) LockEnvironmentToBranch(_ context.Context, env, branch string) error {
	f.Locks = append(f.Locks, LockCall{Env: env, Branch: branch})
	return nil
}

func (f *Fake) API(ctx context.Context, req APIRequest) ([]byte, error) {
	if f.APIFunc == nil {
		return nil, nil
	}
	return f.APIFunc(ctx, req)
}

func (f *Fake) RunWorkflow(_ context.Context, workflow string, fields map[string]string) error {
	f.Workflows = append(f.Workflows, WorkflowCall{Workflow: workflow, Fields: fields})
	return nil
}
