package tfenc

// hydrate.go — the local half: turn an instance checkout's `.llz` credential cache
// into the environment a Terraform command actually needs. See the package
// comment for why that gap existed.
//
// THE RULE THAT MAKES THIS SAFE TO DO AUTOMATICALLY: an entry already present in
// the process environment is NEVER overwritten, so an operator who exported
// something deliberately keeps it, and in CI — where the workflow sets these and
// no `.llz` exists — nothing is contributed at all.
//
// It does NOT follow that "the worst case is it changes nothing": INTRODUCING an
// absent variable is a real change, and `derived` below is a mapping rather than a
// copy — the cache holds LINODE_API_TOKEN while the provider reads LINODE_TOKEN —
// so a process carrying one and deliberately not the other would acquire a
// credential it did not have. Adding a name to that table is a security decision.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cli"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/instanceresolve"
)

// SecretsFile and VarsFile are the `.llz` cache, relative to the instance root.
const (
	SecretsFile = ".llz/secrets.env"
	VarsFile    = ".llz/vars.env"
)

// Var is one environment entry the Terraform stack reads.
type Var struct {
	// Name is what the consumer reads — OpenTofu, the S3 backend's AWS SDK, or
	// the Linode provider.
	Name string
	// Value is the resolved value.
	Value string
	// From names the `.llz` key it was derived from. Reporting the SOURCE is what
	// makes a missing value actionable: "set TF_STATE_ACCESS_KEY" is something the
	// operator can do, where "set AWS_ACCESS_KEY_ID" sends them looking for an AWS
	// account they do not have.
	From string
	// Secret is true for values that must never be printed.
	Secret bool
}

// Missing is one source key that was absent, and what it would have provided.
type Missing struct {
	// Key is the `.llz` / environment name to set.
	Key string
	// Provides is the variable that stayed unset because Key was absent.
	Provides string
}

// Local is the environment a hand-run OpenTofu needs inside an instance
// checkout, plus what could not be assembled and why.
type Local struct {
	// Root is the instance checkout Vars were assembled from, "" when the
	// caller is not standing in one.
	Root string
	// Vars are the additions, in a stable order.
	Vars []Var
	// Present names the variables already set in the process environment and
	// therefore left untouched.
	Present []string
	// Missing names the source keys that were absent.
	Missing []Missing
	// Overrode names the ambient variables replaced by this instance's own value.
	// Reported, never silent: replacing something the operator exported is exactly
	// the kind of thing they must be told about, even when it is right.
	Overrode []Override
}

// Override is one ambient variable replaced by this instance's own credential.
type Override struct {
	// Name is the generic variable that was already set (e.g. AWS_ACCESS_KEY_ID).
	Name string
	// From names the instance value that replaced it (e.g. TF_STATE_ACCESS_KEY).
	From string
}

// derived maps each variable the Terraform stack reads to the `.llz` key carrying
// it, and is the whole contract: backend credentials, endpoint, bucket, and the
// provider token. TF_ENCRYPTION is absent because it is BUILT, not copied.
// Anything else a root needs is a value `llz render` puts in <env>.tfvars.
var derived = []Var{
	// The S3 backend reaches Linode Object Storage through the AWS SDK, which
	// reads its own names — see the roots' backend.tf, which documents the same
	// three-way mapping from the CI side.
	{Name: "AWS_ACCESS_KEY_ID", From: "TF_STATE_ACCESS_KEY", Secret: true},
	{Name: "AWS_SECRET_ACCESS_KEY", From: "TF_STATE_SECRET_KEY", Secret: true},
	{Name: "AWS_ENDPOINT_URL_S3", From: "TF_STATE_ENDPOINT"},
	// Not consumed by tofu directly: `-backend-config=bucket=$TF_STATE_BUCKET` at
	// init time. Carried here so one hydration covers the whole init.
	{Name: "TF_STATE_BUCKET", From: "TF_STATE_BUCKET"},
	// The Linode provider reads LINODE_TOKEN; the cache and every workflow spell
	// the credential LINODE_API_TOKEN.
	{Name: "LINODE_TOKEN", From: "LINODE_API_TOKEN", Secret: true},
}

// Hydrate assembles the local Terraform environment from the instance checkout
// enclosing startDir.
//
// Returns a zero Local with no error when startDir is not inside an instance —
// that is the ordinary CI case and every other case where there is simply
// nothing to add, and it must not be an error, because Hydrate runs on the path
// of every Terraform shell-out.
//
// The error return is reserved for material that is PRESENT AND WRONG: a cached
// passphrase that would produce an invalid or injectable encryption document.
// That is worth reporting loudly precisely because the operator believes it is
// configured.
func Hydrate(startDir string) (Local, error) {
	root := instanceRoot(startDir)
	if root == "" {
		return Local{}, nil
	}
	l := Local{Root: root}

	secrets := cli.ReadEnvFile(filepath.Join(root, SecretsFile))
	vars := cli.ReadEnvFile(filepath.Join(root, VarsFile))
	// Precedence: the process environment wins over the cache, always. The cache
	// is a convenience for a shell that has nothing set; it must never quietly
	// replace a value the operator or a workflow chose.
	lookup := func(key string) string {
		if v := os.Getenv(key); v != "" {
			return v
		}
		if v := secrets[key]; v != "" {
			return v
		}
		return vars[key]
	}

	if os.Getenv(EnvVar) != "" {
		l.Present = append(l.Present, EnvVar)
	} else if passphrase := lookup(PassphraseEnv); passphrase == "" {
		l.Missing = append(l.Missing, Missing{Key: PassphraseEnv, Provides: EnvVar})
	} else {
		doc, err := Build(Config{
			Passphrase:    passphrase,
			KeyName:       lookup(KeyNameEnv),
			PassphraseOld: lookup(PassphraseOldEnv),
			KeyNameOld:    lookup(KeyNameOldEnv),
		})
		if err != nil {
			return l, fmt.Errorf("building %s from %s: %w", EnvVar, filepath.Join(root, SecretsFile), err)
		}
		l.Vars = append(l.Vars, Var{Name: EnvVar, Value: doc, From: PassphraseEnv, Secret: true})
	}

	for _, d := range derived {
		v := lookup(d.From)
		// A MAPPED NAME YIELDS TO THIS INSTANCE'S OWN CREDENTIAL, and that is the one
		// place the never-overwrite rule is wrong.
		//
		// AWS_ACCESS_KEY_ID is a GENERIC, AMBIENT name. Anyone with an AWS account has
		// it exported, and its presence says nothing about which credential belongs to
		// THIS instance's Linode Object Storage state bucket. Leaving it alone handed
		// the operator's AWS key to Linode and produced
		//
		//	Error: Failed to get existing workspaces: ... api error InvalidAccessKeyId:
		//	The AWS Access Key Id you provided does not exist in our records
		//
		// with nothing connecting it to the conflict — on a machine that merely has
		// AWS credentials, which is most of them. Every documented `llz tofu` flow
		// fails there, including the provider-lock remedy this release added.
		//
		// The LLZ-SPELLED name still wins: `lookup` prefers an exported
		// TF_STATE_ACCESS_KEY over the cache, so an operator who deliberately pointed
		// this instance somewhere else keeps that. What is overridden is only the
		// ambient generic name, only when the instance actually has its own value for
		// it, and only where the two spellings DIFFER — TF_STATE_BUCKET, which the
		// operator sets under its own name, is untouched.
		//
		// A NO-OP IN CI by construction, not by a mode flag: the workflow sets
		// AWS_ACCESS_KEY_ID from TF_STATE_ACCESS_KEY, so the two agree and there is
		// nothing to override — and no `.llz` cache exists there for `lookup` to read.
		if existing := os.Getenv(d.Name); existing != "" {
			if d.Name == d.From || v == "" || v == existing {
				l.Present = append(l.Present, d.Name)
				continue
			}
			d.Value = v
			l.Vars = append(l.Vars, d)
			l.Overrode = append(l.Overrode, Override{Name: d.Name, From: d.From})
			continue
		}
		if v == "" {
			l.Missing = append(l.Missing, Missing{Key: d.From, Provides: d.Name})
			continue
		}
		d.Value = v
		l.Vars = append(l.Vars, d)
	}
	sort.Strings(l.Present)
	return l, nil
}

// instanceRoot walks up from dir to the first instance checkout, INCLUDING dir
// itself.
//
// Not instanceresolve.EnclosingInstanceRoot: that one starts at the CWD's PARENT
// (it answers "am I below a root?", for the operator who forgot to `cd` up) and
// takes no argument, so it can neither see a root it is standing in nor be
// driven by a test. The markers are the shared part and they come from there.
func instanceRoot(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	for {
		if instanceresolve.IsInstanceRoot(abs) {
			return abs
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return ""
		}
		abs = parent
	}
}

// ResolvedNote is the one-line report that hydration contributed something.
//
// It lives here because two printers need it — tofudriver after resolving `local`
// itself, tfbin from the chokepoint — and separate copies drift: "resolved 2
// variables" against "resolved 2 Terraform variables", depending on which code
// path happened to run.
func ResolvedNote(n int) string { return resolvedNote(n, false) }

// ResolvedNoteFor is ResolvedNote for a resolution that REPLACED something.
//
// The parenthetical is not decoration: "anything already exported was left alone"
// is the promise this command makes, and printing it on a run that overrode an
// ambient AWS key would be false in the one place the operator most needs it
// true. The lines naming each override follow it, and a blanket claim above them
// reads as the tool contradicting itself.
func ResolvedNoteFor(l Local) string { return resolvedNote(len(l.Vars), len(l.Overrode) > 0) }

func resolvedNote(n int, overrode bool) string {
	noun := "variables"
	if n == 1 {
		noun = "variable"
	}
	tail := " (anything already exported was left alone)"
	if overrode {
		tail = " (see below for the ambient values this instance's own took precedence over)"
	}
	return fmt.Sprintf("resolved %d Terraform %s from %s%s", n, noun, SecretsFile, tail)
}

// Environ renders the additions as KEY=value, for appending to os.Environ().
func (l Local) Environ() []string {
	out := make([]string, 0, len(l.Vars))
	for _, v := range l.Vars {
		out = append(out, v.Name+"="+v.Value)
	}
	return out
}

// Exports renders the additions as shell `export` statements for
// `eval "$(llz tofu --export)"`.
//
// Single quotes, with the shell's only escape for them, because TF_ENCRYPTION is
// a MULTI-LINE HCL document: a double-quoted heredoc would expand `$` and the
// document contains none today but is not guaranteed not to, and an unquoted
// value is not even close to legal. Single-quoting is the one form where the
// content is verbatim.
func (l Local) Exports() string {
	var b strings.Builder
	for _, v := range l.Vars {
		// shellQuote, not a second copy of the escape: this package exists because
		// one rule had two implementations, and quoting was quietly the same
		// mistake one layer down — exportfile.go quotes the PATH, this quotes the
		// VALUE, and they had no reason to drift except that nothing stopped them.
		fmt.Fprintf(&b, "export %s=%s\n", v.Name, shellQuote(v.Value))
	}
	return b.String()
}

// Value resolves one variable the way the hydrated command will see it: the
// process environment first, then what Hydrate assembled. Callers that need to
// USE a value (the bucket name, to build a -backend-config flag) rather than
// merely pass it along go through here, so they cannot read a stale os.Getenv
// for something the cache supplied.
func (l Local) Value(name string) string {
	// VARS FIRST, because Environ() APPENDS them to os.Environ() and the last
	// assignment is the one the child process sees. Reading the environment first
	// was equivalent while hydration could only add; now that a mapped name can
	// override an ambient one, it would report a value the command is not going to
	// use — and its caller derives an `init` backend config from it.
	for _, v := range l.Vars {
		if v.Name == name {
			return v.Value
		}
	}
	return os.Getenv(name)
}

// Has reports whether name will be set for the hydrated command — either
// already in the environment, or supplied by Hydrate.
func (l Local) Has(name string) bool {
	for _, p := range l.Present {
		if p == name {
			return true
		}
	}
	for _, v := range l.Vars {
		if v.Name == name {
			return true
		}
	}
	return false
}
