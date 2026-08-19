package assertplatform

import (
	"os"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

// TestMain zeroes the probe retry delay for the whole package.
//
// THIS LINE IS NOT OPTIONAL and its absence does not fail, it just makes the
// suite crawl: internal/kubectlprobe retries an unanswerable kubectl call three
// times with a 3s gap, so every stubbed-error test pays six real seconds.
// internal/converge lost this line in extraction and its suite went from 4s to
// 568s before tripping CI's 300s timeout. Any package that moves code touching
// kubectlprobe needs it.
func TestMain(m *testing.M) {
	kubectlprobe.Delay = 0
	// Same reason, second seam: the k8sVersion exemption retries a bounded number
	// of times with a real pause between attempts, so every stubbed-error test in
	// that lane would otherwise pay it.
	k8sVersionSleep = func(time.Duration) {}
	os.Exit(m.Run())
}
