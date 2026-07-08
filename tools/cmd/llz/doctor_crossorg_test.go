package main

import (
	"reflect"
	"testing"
)

func TestUsesOrg(t *testing.T) {
	cases := map[string]string{
		"akamai-consulting/lke-landing-zone/.github/workflows/llz-terraform.yml@v0.0.24": "akamai-consulting",
		"akamai-consulting/lke-landing-zone/instance-template/.github/actions/x@v1":      "akamai-consulting",
		"actions/checkout@v7":           "actions",
		"./.github/workflows/local.yml": "",
		"docker://alpine:3":             "",
		"":                              "",
		"noslash@ref":                   "",
	}
	for in, want := range cases {
		if got := usesOrg(in); got != want {
			t.Errorf("usesOrg(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCrossOrgSecretInheritFindings(t *testing.T) {
	// Cross-org call + secrets: inherit → flagged.
	crossOrg := `
jobs:
  call:
    uses: akamai-consulting/lke-landing-zone/.github/workflows/llz-terraform.yml@v0.0.24
    secrets: inherit
`
	got, err := crossOrgSecretInheritFindings(crossOrg, "akamai", "terraform.yml")
	if err != nil {
		t.Fatal(err)
	}
	want := []crossOrgReuseFinding{{File: "terraform.yml", Job: "call", UsesOrg: "akamai-consulting"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("cross-org: got %+v, want %+v", got, want)
	}

	// Negative cases — none should be flagged.
	negatives := map[string]struct{ content, owner string }{
		"same-org": {`
jobs:
  call:
    uses: akamai/lke-landing-zone/.github/workflows/llz-terraform.yml@v0.0.24
    secrets: inherit
`, "akamai"},
		"same-org-case-insensitive": {`
jobs:
  call:
    uses: Akamai-Consulting/lke-landing-zone/.github/workflows/x.yml@v1
    secrets: inherit
`, "akamai-consulting"},
		"explicit-secrets-not-inherit": {`
jobs:
  call:
    uses: akamai-consulting/lke-landing-zone/.github/workflows/x.yml@v1
    secrets:
      TOKEN: ${{ secrets.TOKEN }}
`, "akamai"},
		"no-secrets": {`
jobs:
  call:
    uses: akamai-consulting/lke-landing-zone/.github/workflows/x.yml@v1
`, "akamai"},
		"unrendered-template-token": {`
jobs:
  call:
    uses: <@ upstream_org @>/lke-landing-zone/.github/workflows/x.yml@<@ llz_version @>
    secrets: inherit
`, "akamai"},
		"local-steps-job": {`
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
`, "akamai"},
	}
	for name, c := range negatives {
		got, err := crossOrgSecretInheritFindings(c.content, c.owner, "wf.yml")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(got) != 0 {
			t.Errorf("%s: expected no findings, got %+v", name, got)
		}
	}
}
