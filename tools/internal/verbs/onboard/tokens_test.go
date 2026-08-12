package onboard

import (
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/answers"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/templateid"
)

// TestNothingToProvisionNote gates the exit line of the most-run path through
// `llz tokens`: the idempotent re-run, where the command provisions nothing and
// what it PRINTS is the entire deliverable.
//
// It is a real gate rather than a spelling check because the branch it guards
// returns before pushToRepo. An operator who rotates a credential by editing
// .llz/secrets.env lands here with everything envreq.Satisfied (presence, not
// value) and their edit unpushed — so a message that does not name
// `llz secrets push` sends them away believing the repo has what they just
// changed. Each assertion below corresponds to a way the old string actually
// went wrong: no route out, no statement of what the check does not cover, and
// an env name hardcoded to the template's own lane.
func TestNothingToProvisionNote(t *testing.T) {
	got := NothingToProvisionNote("prod")

	// The route out. This is the whole point of the message: `llz secrets push`
	// is a sibling verb, and nothing else in a `llz tokens` run mentions it.
	if !strings.Contains(got, "llz secrets push prod --yes") {
		t.Errorf("re-run message never names the command that pushes a hand-edited credential:\n%s", got)
	}
	// PRESENCE-not-VALUE has to be said, not implied. "Everything is set" is true
	// and still leaves the operator with the wrong conclusion.
	if !strings.Contains(got, ".llz/secrets.env") {
		t.Errorf("re-run message claims everything is set without saying what is NOT checked:\n%s", got)
	}
	// The deployment the operator named, not `e2e`. The old constant reported on
	// the template's throwaway lane for every adopter who has no such env — the
	// misdirection DefaultDoctorEnv() removes one verb over.
	if !strings.Contains(got, "infra-prod") {
		t.Errorf("re-run message does not name the deployment it reported on:\n%s", got)
	}
	if strings.Contains(got, "e2e") {
		t.Errorf("re-run message names `e2e` for an --env prod run:\n%s", got)
	}
}

func TestRegionFromCluster(t *testing.T) {
	cases := map[string]string{
		"us-ord-1":     "us-ord",
		"us-ord-10":    "us-ord",
		"us-iad-18":    "us-iad",
		"eu-central-1": "eu-central",
		"single":       "single", // no hyphen → returned unchanged
	}
	for in, want := range cases {
		if got := regionFromCluster(in); got != want {
			t.Errorf("regionFromCluster(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRepoSlug(t *testing.T) {
	cases := map[string]string{
		"akamai-consulting/lke-landing-zone-example": "lke-landing-zone-example",
		"Org/My-Repo": "my-repo",
		"bare":        "bare",
	}
	for in, want := range cases {
		if got := repoSlug(in); got != want {
			t.Errorf("repoSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveInstanceRepo(t *testing.T) {
	// Explicit flag always wins.
	if r, err := answers.ResolveInstanceRepo("owner/explicit", false); err != nil || r != "owner/explicit" {
		t.Fatalf("flag: got (%q,%v), want owner/explicit", r, err)
	}
	// Admin with no flag and no answers file falls back to the example repo.
	if r, err := answers.ResolveInstanceRepo("", true); err != nil || r != templateid.DefaultOrg+"/"+templateid.Name+"-example" {
		t.Fatalf("admin default: got (%q,%v)", r, err)
	}
	// Non-admin with no flag and (presumably) no .copier-answers.yml here errors.
	if _, err := answers.ResolveInstanceRepo("", false); err == nil {
		t.Errorf("expected error when no repo can be determined")
	}
}
