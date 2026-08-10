package capability

// forge.go — the handle for the git forge, which in this tree means the `gh` CLI.
//
// THIRD CAPABILITY, and the first one nobody designed. It surfaced because the
// seam ratchet gave four packages the wrong remedy: `kubectlprobe.Exec` takes the
// BINARY as its first argument, so branch-policy, build-preflight,
// credential-state-passphrase and template-commit were being told to "take a
// Cluster handle" for code that runs `gh api`. A guard being wrong is what
// measured the gap.
//
// THE SURFACE IS SMALL AND ALREADY SPLIT. Censused across the tree: `api` 9,
// `secret set` 6, `variable set` 4, `workflow run` 4, `release list|download` 4,
// `secret delete` 1, `repo create` 1, `auth token|status` 2. Thirty-one calls, and
// they fall into three groups that the EXISTING DECLARATIONS already distinguish —
// which is the strongest evidence available that the split is real rather than
// invented here:
//
//	build-preflight    `gh api repos/<r>`   declares cloud-read      — a read
//	template-commit    `gh auth token`      declares cloud-read      — a read
//	branch-policy      `gh api -X PUT …`    declares cloud-mutate    — a write
//	state-passphrase   `gh secret set`      declares secret-custody  — custody
//
// So the handle is gated by three different grants, unlike Cluster (cluster-read /
// cluster-write) and Secrets (secret-read / secret-custody). That is not a new
// shape so much as an honest one: placing a credential in a GitHub environment and
// triggering a workflow are different powers, and `forge-env-seed` already recorded
// that its custody was "TRUE and undeclarable" while both went through one seam.
//
// `gh api` IS BOTH, and it is classified the way kubectl's verbs are — by reading
// the argv. There the discriminator is the subcommand; here it is the HTTP method,
// because `gh api repos/x` reads and `gh api -X PUT repos/x` does not. Anything
// unrecognised is REFUSED rather than assumed read, for the reason the kubectl
// classifier gives: a new subcommand that nobody classified must not arrive with
// the permissions of the safest one.

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

// ForgeAction is what a `gh` argv asks for.
type ForgeAction int

const (
	// ForgeRead observes the forge: reading the API, listing releases, asking for
	// the current token.
	ForgeRead ForgeAction = iota
	// ForgeMutate changes the forge: workflow runs, variables, repositories, and
	// any `gh api` carrying a mutating HTTP method.
	ForgeMutate
	// ForgeCustody places or removes CREDENTIAL MATERIAL. Split out from
	// ForgeMutate because setting a repository secret and dispatching a workflow
	// are different powers, and the declarations already say so.
	ForgeCustody
	// ForgeUnclassified is an argv this table does not describe. Always refused.
	ForgeUnclassified
)

func (a ForgeAction) String() string {
	switch a {
	case ForgeRead:
		return "read"
	case ForgeMutate:
		return "mutate"
	case ForgeCustody:
		return "custody"
	default:
		return "unclassified"
	}
}

// forgeReads, forgeMutations and forgeCustody are keyed by "<command> <subcommand>"
// where the subcommand disambiguates, and by "<command>" where it does not.
//
// LISTED EXPLICITLY, like writeVerbs, rather than derived from a rule. There is no
// property of the string "workflow run" that says it dispatches a pipeline; a
// reviewer has to know, and a table is where knowing gets written down.
var (
	forgeReads = map[string]bool{
		"auth token": true, "auth status": true,
		"release list": true, "release download": true, "release view": true,
		"secret list": true, "variable list": true, "repo view": true,
		"run list": true, "run view": true, "workflow list": true, "workflow view": true,
	}
	forgeMutations = map[string]bool{
		"workflow run": true, "workflow enable": true, "workflow disable": true,
		"variable set": true, "variable delete": true,
		"repo create": true, "repo delete": true, "repo edit": true,
		"run cancel": true, "run rerun": true, "release create": true,
		"release upload": true, "release delete": true,
	}
	forgeCustodyOps = map[string]bool{
		"secret set": true, "secret delete": true,
	}
	// mutatingMethods are the HTTP methods that make `gh api` a write. GET and
	// HEAD are reads; an ABSENT method is a read, because that is gh's default.
	mutatingMethods = map[string]bool{
		"POST": true, "PUT": true, "PATCH": true, "DELETE": true,
	}
)

// ClassifyForge reports what a `gh` argv asks for. Exported because the refusal
// message and the tests must agree with the checker about what an argv IS — the
// same reason Verb is exported for kubectl.
func ClassifyForge(args []string) ForgeAction {
	if len(args) == 0 {
		return ForgeUnclassified
	}
	// `gh api` is classified by HTTP method, not by name.
	if args[0] == "api" {
		return classifyAPIMethod(args[1:])
	}
	key := args[0]
	if len(args) > 1 && !strings.HasPrefix(args[1], "-") {
		key = args[0] + " " + args[1]
	}
	switch {
	case forgeCustodyOps[key]:
		return ForgeCustody
	case forgeMutations[key]:
		return ForgeMutate
	case forgeReads[key]:
		return ForgeRead
	default:
		return ForgeUnclassified
	}
}

// classifyAPIMethod works out what HTTP method `gh api` will actually send.
//
// ─────────────────────────────────────────────────────────────────────────────
// IT USED TO MODEL `-X` AS THE ONLY THING THAT SETS THE METHOD, AND READ ONLY THE
// FIRST ONE. Three ways past it, all probed against a cloud-read handle and all
// permitted:
//
//	gh api -X GET repos/o/r -X DELETE          -> read
//	gh api repos/o/r/issues -f title=x         -> read
//	gh api graphql -f query=mutation{...}      -> read
//
// THE FIRST IS THE kubectl `--dry-run` DEFECT AGAIN, in a different tool: the
// classifier and the program disagree about which token is authoritative. gh parses
// with pflag, where a repeated flag takes the LAST value, and this returned on the
// FIRST. So the argv gh executes as DELETE was judged as GET.
//
// THE SECOND IS THE ONE MOST LIKELY TO BE REACHED, because it needs no adversary —
// it is how `gh api` is ordinarily written. From gh's own manual: "The default HTTP
// request method is GET normally and POST if any parameters were added." `-f`, `-F`
// and `--input` all add parameters, so an argv with no `-X` at all can be a write,
// and "absent method means GET" was true only of the argv nobody sends.
//
// THE THIRD IS REFUSED RATHER THAN CLASSIFIED, and that is the honest answer.
// GitHub's GraphQL endpoint accepts POST only, so gh always POSTs it — but a
// GraphQL document can be a query or a mutation, and telling them apart means
// parsing GraphQL. Guessing either way is wrong in one direction, so it goes to
// ForgeUnclassified, which every grant refuses. Nothing in this tree calls
// `gh api graphql`; when something does, the first caller makes the decision, which
// is what this package already does for a kubectl verb nobody has classified.
// ─────────────────────────────────────────────────────────────────────────────
func classifyAPIMethod(rest []string) ForgeAction {
	var explicit string
	var params, graphql bool

	for i := 0; i < len(rest); i++ {
		a := rest[i]
		switch {
		case a == "-X" || a == "--method":
			if i+1 >= len(rest) {
				// A dangling flag: the argv is malformed and its intent unknown.
				return ForgeUnclassified
			}
			// LAST WINS, not first — pflag's rule, and gh's by construction.
			explicit = rest[i+1]
			i++
		case strings.HasPrefix(a, "--method="):
			explicit = strings.TrimPrefix(a, "--method=")
		case strings.HasPrefix(a, "-X="):
			explicit = strings.TrimPrefix(a, "-X=")
		case paramFlags[a]:
			params = true
			// Consume the value so a field like `-f method=GET` cannot be mistaken
			// for anything but data.
			if i+1 < len(rest) {
				i++
			}
		case hasAnyPrefix(a, paramFlagPrefixes):
			params = true
		case a == "graphql" && !strings.HasPrefix(a, "-"):
			graphql = true
		}
	}

	// An explicit method beats every inference, in both directions: `-X GET` with
	// fields is a GET with a body, which gh will send as written.
	if explicit != "" {
		switch u := strings.ToUpper(explicit); {
		case mutatingMethods[u]:
			return ForgeMutate
		case u == "GET" || u == "HEAD":
			// A GraphQL GET is not a thing GitHub serves; if someone writes one the
			// argv is confused enough to be worth refusing.
			if graphql {
				return ForgeUnclassified
			}
			return ForgeRead
		default:
			return ForgeUnclassified
		}
	}
	if graphql {
		return ForgeUnclassified
	}
	if params {
		return ForgeMutate
	}
	return ForgeRead
}

// paramFlags and paramFlagPrefixes are the `gh api` flags that ADD PARAMETERS, and
// therefore flip its default method from GET to POST.
var paramFlags = map[string]bool{
	"-f": true, "--raw-field": true, "-F": true, "--field": true, "--input": true,
}

var paramFlagPrefixes = []string{"-f=", "-F=", "--raw-field=", "--field=", "--input="}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// Forge is the handle a binding receives for the git forge. One Run, gated by
// three grants, because that is how many distinct powers `gh` actually offers.
type Forge interface {
	// Run executes `gh` with args, if the binding's grants permit that argv.
	Run(args ...string) ([]byte, error)
	// Permits reports whether Run would be allowed, without running it.
	Permits(args ...string) error
}

type forge struct {
	exec    func(string, ...string) ([]byte, error)
	read    bool
	mutate  bool
	custody bool
}

func (f forge) Permits(args ...string) error {
	switch ClassifyForge(args) {
	case ForgeRead:
		if !f.read {
			return fmt.Errorf("%w: `gh %s` reads the forge", ErrNoForgeRead, strings.Join(args, " "))
		}
	case ForgeMutate:
		if !f.mutate {
			return fmt.Errorf("%w: `gh %s` changes the forge", ErrNoForgeMutate, strings.Join(args, " "))
		}
	case ForgeCustody:
		if !f.custody {
			return fmt.Errorf("%w: `gh %s` places credential material", ErrNoForgeCustody, strings.Join(args, " "))
		}
	default:
		return fmt.Errorf("%w: `gh %s` — add it to the table in capability/forge.go with "+
			"the group it belongs to", ErrForgeUnclassified, strings.Join(args, " "))
	}
	return nil
}

func (f forge) Run(args ...string) ([]byte, error) {
	if err := f.Permits(args...); err != nil {
		return nil, err
	}
	return f.exec("gh", args...)
}

var (
	ErrNoForgeRead       = errors.New("this binding declares neither cloud-read nor read-repo, so it may not read the forge")
	ErrNoForgeMutate     = errors.New("this binding does not declare cloud-mutate, so it may not change the forge")
	ErrNoForgeCustody    = errors.New("this binding does not declare secret-custody, so it may not place forge secrets")
	ErrForgeUnclassified = errors.New("unclassified gh argv, refused rather than assumed safe")
)

type deniedForge struct{}

func (deniedForge) Permits(args ...string) error {
	return fmt.Errorf("%w: `gh %s`", ErrNoForgeRead, strings.Join(args, " "))
}
func (d deniedForge) Run(args ...string) ([]byte, error) { return nil, d.Permits(args...) }

// DeniedForge is the refusing handle, exported so a caller assembling its own Deps
// has somewhere safe to default to.
func DeniedForge() Forge { return deniedForge{} }

// forgeHandle builds the forge handle a binding's grants entitle it to.
//
// READ COMES FROM EITHER `cloud-read` OR `read-repo`, and that is a judgement worth
// naming: `gh api repos/<r>` asks GitHub about the repository this instance lives
// in, which the catalog treats as reading the repo as often as reading a cloud.
// Both grants appear on the four packages this was built for. Requiring a specific
// one would have meant re-declaring working code to satisfy a new handle, which is
// the tail wagging the dog.
//
// MUTATE AND CUSTODY EACH IMPLY READ, for the reason cluster-write implies
// cluster-read: every one of these paths reads back what it changed.
func forgeHandle(b extension.Binding) Forge {
	var read, mutate, custody bool
	for _, g := range b.Grants {
		switch g {
		case extension.CloudRead, extension.ReadRepo:
			read = true
		case extension.CloudMutate:
			mutate = true
		case extension.SecretCustody:
			custody = true
		}
	}
	if !read && !mutate && !custody {
		return deniedForge{}
	}
	return forge{
		exec:    kubectlprobe.Exec,
		read:    read || mutate || custody,
		mutate:  mutate,
		custody: custody,
	}
}

// ForgeActions returns the classified argv keys, for tests and for the docs guard
// to agree with this table rather than transcribe it.
func ForgeActions() (reads, mutations, custody []string) {
	for k := range forgeReads {
		reads = append(reads, k)
	}
	for k := range forgeMutations {
		mutations = append(mutations, k)
	}
	for k := range forgeCustodyOps {
		custody = append(custody, k)
	}
	sort.Strings(reads)
	sort.Strings(mutations)
	sort.Strings(custody)
	return reads, mutations, custody
}
