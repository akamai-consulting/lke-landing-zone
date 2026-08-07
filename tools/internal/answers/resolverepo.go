package answers

// resolverepo.go — which repo IS this instance?
//
// Three of its four branches read the copier answers, which is why it lives here
// rather than in tokens.go where it was: an explicit --repo wins, then
// .copier-answers.yml, then (admin only) the template's own example repo, then a
// clear error. Four callers in package main asked this question and it was
// answered inside a 700-line file about GitHub tokens.

import (
	"fmt"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/templateid"
)

func ResolveInstanceRepo(flagVal string, admin bool) (string, error) {
	if flagVal != "" {
		return flagVal, nil
	}
	if a, _ := Read("."); a != nil && a.InstanceRepo != "" {
		return a.InstanceRepo, nil
	}
	if admin {
		return templateid.ExampleRepo(), nil
	}
	return "", fmt.Errorf("could not determine instance repo — pass --repo <owner>/<name>")
}
