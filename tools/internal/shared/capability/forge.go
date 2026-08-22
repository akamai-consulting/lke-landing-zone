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

	// apply records what one parsed flag means. Only three flags say anything
	// about the method; the rest are consumed so their VALUES cannot be mistaken
	// for one (`-f method=GET` is data, `--jq graphql` is a filter).
	// endpoint is the first POSITIONAL — the API path gh will call. Captured
	// because the method alone does not say what an argv does: see
	// forgeSecretEndpoint.
	var endpoint string

	apply := func(name, val string) {
		switch {
		case name == "method":
			explicit = val // LAST WINS — pflag's rule, and gh's by construction
		case paramFlags[name]:
			params = true
		}
	}

	for i := 0; i < len(rest); i++ {
		a := rest[i]
		switch {
		case a == "--":
			// pflag stops parsing here; everything after is positional.
			for _, p := range rest[i+1:] {
				if p == "graphql" {
					graphql = true
				}
				if endpoint == "" {
					endpoint = p
				}
			}
			i = len(rest)

		case strings.HasPrefix(a, "--"):
			name, val, attached := strings.Cut(a[2:], "=")
			takesValue, known := ghAPIFlags[name]
			if !known {
				return ForgeUnclassified
			}
			if takesValue && !attached {
				if i+1 >= len(rest) {
					return ForgeUnclassified // dangling flag; intent unknown
				}
				i++
				val = rest[i]
			}
			apply(name, val)

		case len(a) > 1 && a[0] == '-':
			// A SHORTHAND CLUSTER, walked the way pflag walks it. See the header.
			cluster := a[1:]
			for len(cluster) > 0 {
				name, known := ghAPIShorthand[cluster[0]]
				if !known {
					return ForgeUnclassified
				}
				cluster = cluster[1:]
				if !ghAPIFlags[name] {
					// A BOOLEAN CAN STILL CARRY `=value`, and pflag checks for it
					// BEFORE it consults NoOptDefVal — so `-i=true` spends the rest
					// of the cluster as a value even though `-i` takes none.
					// Leaving `=true` in the cluster made the next pass look up a
					// shorthand named `=` and refuse a legitimate read: the same
					// classifier-vs-parser divergence this file exists to close,
					// pointed the other way.
					if len(cluster) > 1 && cluster[0] == '=' {
						cluster = ""
					}
					continue // otherwise the next letter is another flag
				}
				var val string
				switch {
				case len(cluster) > 1 && cluster[0] == '=':
					val, cluster = cluster[1:], "" // -X=DELETE
				case len(cluster) > 0:
					val, cluster = cluster, "" // -XDELETE — THE ONE THAT WAS OPEN
				case i+1 < len(rest):
					i++
					val = rest[i] // -X DELETE
				default:
					return ForgeUnclassified // dangling
				}
				apply(name, val)
			}

		case a == "graphql":
			graphql = true
			if endpoint == "" {
				endpoint = a
			}

		default:
			// A positional. The FIRST one is the endpoint; gh takes exactly one.
			if endpoint == "" {
				endpoint = a
			}
		}
	}

	// An explicit method beats every inference, in both directions: `-X GET` with
	// fields is a GET with a body, which gh will send as written.
	if explicit != "" {
		// GRAPHQL IS REFUSED WHATEVER THE METHOD, and it was refused in only two
		// of the three arms. gh always POSTs GraphQL, so `-X POST graphql` is the
		// SAME REQUEST as bare `graphql` — and it graded ForgeMutate while the
		// bare spelling correctly went to ForgeUnclassified. A document that can
		// be a query or a mutation is not classifiable without parsing GraphQL,
		// and that is true no matter how the argv spells the verb.
		if graphql {
			return ForgeUnclassified
		}
		switch u := strings.ToUpper(explicit); {
		case mutatingMethods[u]:
			return mutateOrCustody(endpoint)
		case u == "GET" || u == "HEAD":
			return ForgeRead
		default:
			return ForgeUnclassified
		}
	}
	if graphql {
		return ForgeUnclassified
	}
	if params {
		return mutateOrCustody(endpoint)
	}
	return ForgeRead
}

// mutateOrCustody grades a WRITE by what it writes, not only by its verb.
//
// ─────────────────────────────────────────────────────────────────────────────
// THE METHOD ALONE DEFEATED THE CUSTODY GRANT. `gh secret set` is classified
// ForgeCustody, so a binding without secret-custody is refused — and
// branchpolicy/policy.go:239 says so in as many words, as the reason its
// `cloud-mutate` declaration is safe. But `gh api -X PUT
// repos/o/r/actions/secrets/FOO` writes the same secret through the same
// credential, and by METHOD it is an ordinary mutation. The declaration was
// enforced against one spelling of the operation.
//
// This is the `-XDELETE` defect one layer up: there the classifier disagreed
// with the parser about what the argv SAYS, here it disagrees with GitHub about
// what the argv DOES. Both let a narrower grant perform a wider act.
//
// Reads are untouched. `envreq` lists `repos/o/r/actions/secrets` to discover
// which credentials are configured, and that must stay ForgeRead — knowing a
// secret EXISTS is not holding it. Only a mutating method on a secret path
// becomes custody.
// ─────────────────────────────────────────────────────────────────────────────
func mutateOrCustody(endpoint string) ForgeAction {
	if forgeSecretEndpoint(endpoint) {
		return ForgeCustody
	}
	return ForgeMutate
}

// forgeSecretEndpoint reports whether a `gh api` path addresses GitHub-held
// credential material.
//
// MATCHED AGAINST THE REAL ENDPOINT SHAPES, not any path containing the word.
// The first cut accepted a `secrets` segment anywhere, and the Contents API
// embeds an arbitrary REPOSITORY PATH in its URL — so
// `repos/o/r/contents/kubernetes/secrets/x.yaml` graded as custody and an
// ordinary content write was refused for want of a grant it never needed. A
// GitOps repo with a `secrets/` directory is not an exotic input; it is most of
// them.
//
// Every place GitHub actually keeps one has `secrets` directly after an API
// FAMILY — actions, codespaces, dependabot — or after `environments/<name>`.
// That covers repo, org and user scope in one rule rather than six literals.
//
// Anything under `contents` is excluded outright: past that segment the URL is
// the caller's file tree and no segment in it is an API family name.
func forgeSecretEndpoint(endpoint string) bool {
	p := strings.TrimSpace(endpoint)
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	segs := strings.Split(strings.Trim(p, "/"), "/")
	for i, seg := range segs {
		if seg == "contents" {
			return false // the rest is a repository path, not API structure
		}
		if seg != "secrets" || i == 0 {
			continue
		}
		switch segs[i-1] {
		case "actions", "codespaces", "dependabot":
			return true
		}
		if i >= 2 && segs[i-2] == "environments" {
			return true
		}
	}
	return false
}

// ghAPIFlags is every flag `gh api` accepts, keyed by LONG name, valued by
// whether it consumes a value. It is a closed set on purpose: an argv containing
// a flag that is not here is ForgeUnclassified, which every grant refuses.
//
// FAILING CLOSED ON AN UNKNOWN FLAG IS THE POINT, not a limitation to apologise
// for. The alternative — skip what we do not recognise and keep classifying — is
// how `-ftitle=x` came to read as a GET: an unrecognised token was treated as
// harmless when it was the token that made the request a POST. If gh grows a
// flag, or one of these is misspelt, the argv is refused with a message naming
// it, and the first caller to hit it makes the decision. That is the rule this
// package already applies to an unclassified kubectl verb and to
// `gh api graphql`.
var ghAPIFlags = map[string]bool{
	// take a value
	"method": true, "field": true, "raw-field": true, "input": true,
	"header": true, "jq": true, "template": true, "preview": true,
	"cache": true, "hostname": true,
	// boolean
	"include": false, "paginate": false, "silent": false,
	"slurp": false, "verbose": false,
}

// ghAPIShorthand maps `gh api`'s single letters onto the long names above, so
// the two spellings cannot disagree about what a flag is or whether it takes a
// value — the disagreement that let `-XDELETE` through while `-X DELETE` was
// caught.
var ghAPIShorthand = map[byte]string{
	'X': "method", 'F': "field", 'f': "raw-field", 'H': "header",
	'q': "jq", 't': "template", 'p': "preview", 'i': "include",
}

// paramFlags are the flags that ADD PARAMETERS, and therefore flip gh's default
// method from GET to POST. From gh's own manual: "The default HTTP request
// method is GET normally and POST if any parameters were added."
var paramFlags = map[string]bool{
	"field": true, "raw-field": true, "input": true,
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
		// AN `api` SECRET WRITE NEEDS BOTH GRANTS, and grading it custody alone
		// swapped one hole for its mirror image. Before, a cloud-mutate binding
		// without custody could PUT a secret because the write graded as an
		// ordinary mutation. Grading it custody fixed that and opened the other
		// side: the bindings holding secret-custody WITHOUT cloud-mutate — the
		// db-admin seeder, objenc's seed-ssec-key, two openbao lanes — gained a
		// `gh api` write they had always been refused.
		//
		// Both directions are wrong for the same reason: writing a secret through
		// the raw API is a mutation AND a placement of credential material, so it
		// is not either grant's to authorise alone.
		//
		// Scoped to `api` deliberately. `gh secret set` has been custody-only
		// since it was classified, and several bindings are declared against that
		// contract; requiring cloud-mutate there too may well be right, but it
		// changes what those declarations mean and belongs in a change that says
		// so. The raw-API spelling is new to this classifier and has no such
		// history to preserve.
		if len(args) > 0 && args[0] == "api" && !f.mutate {
			return fmt.Errorf("%w: `gh %s` writes credential material through the raw API, "+
				"which needs cloud-mutate as well as secret-custody", ErrNoForgeMutate, strings.Join(args, " "))
		}
	default:
		// THE REMEDY DIFFERS BY WHICH TABLE FELL SHORT, and the generic one sent
		// readers to the command table for an argv whose COMMAND is fine. `gh api`
		// is classified by method rather than by name, so it has its own four ways
		// of being unreadable and its own two tables to fix.
		remedy := "add it to the table in capability/forge.go with the group it belongs to"
		if len(args) > 0 && args[0] == "api" {
			remedy = "an `api` argv is unclassified when the method it will send cannot be " +
				"established — a flag missing from ghAPIFlags/ghAPIShorthand in " +
				"capability/forge.go, a method that is neither a read nor a write, a " +
				"dangling -X/--method, or `graphql`, whose document decides and reading " +
				"it means parsing GraphQL"
		}
		return fmt.Errorf("%w: `gh %s` — %s", ErrForgeUnclassified, strings.Join(args, " "), remedy)
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

// hasAnyPrefix reports whether s starts with any of prefixes. Shared with
// writer.go's flag allowlisting.
func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}
