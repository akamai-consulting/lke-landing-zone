package envreq

// Followed validGlyph here — it renders a token-validity verdict as a table glyph,
// which is part of the readiness report this package owns.

import (
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/tokenprobe"
)

// validGlyph maps a validity status to a marker. NOTE the `case vWarn` arm and
// the `default` arm return the SAME value, so no test can distinguish them — the
// arm is redundant. What is assertable, and what matters, is that vInvalid is the
// only status rendered as a failure.
func TestValidGlyph(t *testing.T) {
	invalid := validGlyph(tokenprobe.VInvalid)
	if !strings.Contains(invalid, "✗") {
		t.Errorf("vInvalid must render a cross, got %q", invalid)
	}
	for _, s := range []tokenprobe.ValidityStatus{tokenprobe.VWarn, tokenprobe.VUnreachable} {
		got := validGlyph(s)
		if !strings.Contains(got, "⚠") {
			t.Errorf("status %v must render a warning, got %q", s, got)
		}
		if got == invalid {
			t.Errorf("status %v must not render identically to vInvalid", s)
		}
	}
}
