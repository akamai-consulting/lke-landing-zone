package manifestguard

// ci_placeholder_guard.go implements `llz ci placeholder-guard` — reject
// unsubstituted `placeholder.example.com` hostnames in the rendered manifests.
//
// Anything Argo CD reconciles into a cluster must carry real addresses, never the
// template's example placeholders: a placeholder host that survives rendering
// becomes an Ingress/ExternalSecret/endpoint pointing at a domain nobody owns.
//
// This was the last inline-shell guard in the Makefile's LINT_K8S group — nine
// lines of recipe bash hand-rolling the two things the guard framework already
// owns: the empty-corpus assertion (requireCorpus, written for exactly this
// failure mode) and the manifest walk (walkManifests). Its own Makefile comment
// recorded why the corpus check was added — a missing/empty RENDER_DIR made
// `grep -r` exit non-zero, which the recipe read as "no placeholders found", the
// same clean pass as a fully-rendered tree with none.
//
// Matching is on raw file content rather than parsed YAML: a placeholder can
// appear anywhere (a URL in a ConfigMap value, an annotation, a comment that gets
// templated into a host), and the shell version this replaces matched raw text
// too. Keeping that behavior means the conversion cannot quietly narrow what the
// guard catches.

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/guardkit"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/guardwalk"
)

// placeholderHost is the example hostname the template ships. A rendered
// manifest carrying it means a value was never substituted.
const placeholderHost = "placeholder.example.com"

// phFinding is one line of one file carrying the placeholder host.
type phFinding struct {
	file string
	line int
	text string
}

func RunPlaceholderGuard(renderDir string) error {
	dirs := []string{renderDir}
	findings, examined, err := collectPlaceholderFindings(dirs)
	if err != nil {
		return err
	}
	if err := guardkit.RequireCorpus("placeholder-guard", examined, dirs); err != nil {
		return err
	}
	if len(findings) == 0 {
		fmt.Printf("placeholder-guard: no %s addresses in %d rendered manifest file(s).\n", placeholderHost, examined)
		return nil
	}
	for _, f := range findings {
		fmt.Printf("::error file=%s,line=%d::unsubstituted %s in a rendered manifest: %s\n",
			f.file, f.line, placeholderHost, f.text)
	}
	return fmt.Errorf("placeholder-guard: %d unsubstituted %s reference(s) in the rendered manifests — "+
		"a placeholder host that reaches a cluster points at a domain nobody owns",
		len(findings), placeholderHost)
}

// collectPlaceholderFindings walks the dirs and reports every line carrying the
// placeholder host, with its 1-indexed line number (so the ::error annotation
// lands on the offending line in the PR diff — something the `grep -rn` this
// replaced printed but could not annotate).
func collectPlaceholderFindings(dirs []string) ([]phFinding, int, error) {
	var findings []phFinding
	examined, err := guardwalk.Walk(dirs, func(path string, raw []byte) error {
		sc := bufio.NewScanner(bytes.NewReader(raw))
		// Rendered charts concatenate every template, so a single file can be
		// large; the default 64KB token limit would error on one long line.
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for n := 1; sc.Scan(); n++ {
			line := sc.Text()
			if strings.Contains(line, placeholderHost) {
				findings = append(findings, phFinding{file: path, line: n, text: strings.TrimSpace(line)})
			}
		}
		return sc.Err()
	})
	if err != nil {
		return nil, examined, err
	}
	return findings, examined, nil
}
