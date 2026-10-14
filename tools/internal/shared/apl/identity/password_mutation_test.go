package identity

import (
	crand "crypto/rand"
	mrand "math/rand"
	"strings"
	"testing"
)

// password_mutation_test.go pins the parts of RandomPassword a "looks random"
// assertion cannot see: that the Fisher–Yates shuffle runs at all, that it draws
// from the exact [0,i] range, and that randIndex's non-positive guard holds.
//
// randIndex is not injectable (it reads crypto/rand directly), so the seam used
// here is crypto/rand.Reader itself: swapping it for a fixed-seed math/rand stream
// makes every draw — and therefore the whole permutation — deterministic and
// exactly checkable, without touching production code.

// stubRandReader replaces crypto/rand.Reader with a deterministic stream for the
// rest of the test. Calling it again replays the SAME stream from the start.
func stubRandReader(t *testing.T, seed int64) {
	t.Helper()
	orig := crand.Reader
	crand.Reader = mrand.New(mrand.NewSource(seed))
	t.Cleanup(func() { crand.Reader = orig })
}

// referencePassword is an independent transcription of the algorithm
// RandomPassword documents: seed one character per class, fill from the full
// alphabet, then a textbook Fisher–Yates shuffle over [0,i]. Driven by the same
// randIndex against the same byte stream it must reproduce RandomPassword's output
// byte for byte — so any change to the shuffle's bounds or to the size of the range
// it draws from (i+1) diverges here.
func referencePassword() string {
	classes := []string{pwLower, pwUpper, pwDigit, pwSpecial}
	all := pwLower + pwUpper + pwDigit + pwSpecial

	buf := make([]byte, 0, pwLen)
	for _, class := range classes {
		buf = append(buf, class[randIndex(len(class))])
	}
	for len(buf) < pwLen {
		buf = append(buf, all[randIndex(len(all))])
	}
	for i := len(buf) - 1; i > 0; i-- {
		j := randIndex(i + 1) // inclusive of i: every element can stay put
		buf[i], buf[j] = buf[j], buf[i]
	}
	return string(buf)
}

// TestRandomPassword_ExactPermutation pins the shuffle to the exact permutation
// Fisher–Yates produces on a fixed random stream. A shuffle that skips elements,
// or draws j from the wrong range, silently loses uniformity while still producing
// a "random-looking" password — this is the assertion that sees it.
func TestRandomPassword_ExactPermutation(t *testing.T) {
	const seed = 20260729
	for _, s := range []int64{seed, seed + 1, seed + 2} {
		stubRandReader(t, s)
		got := RandomPassword()
		stubRandReader(t, s) // identical stream, replayed from the start
		want := referencePassword()
		if got != want {
			t.Fatalf("seed %d: RandomPassword = %q, want the exact Fisher–Yates permutation %q", s, got, want)
		}
	}
}

// TestRandomPassword_SeedPhaseIsShuffled asserts the shuffle actually moves the
// four guaranteed characters. Without it they sit at fixed positions 0..3 in class
// order (lower, upper, digit, special) in EVERY draw — a leaked password structure
// that every "contains one of each class" test still passes.
func TestRandomPassword_SeedPhaseIsShuffled(t *testing.T) {
	const draws = 200
	inSeedOrder := 0
	for i := 0; i < draws; i++ {
		pw := RandomPassword()
		if len(pw) != pwLen {
			t.Fatalf("len(pw) = %d, want %d", len(pw), pwLen)
		}
		if strings.IndexByte(pwLower, pw[0]) >= 0 && strings.IndexByte(pwUpper, pw[1]) >= 0 &&
			strings.IndexByte(pwDigit, pw[2]) >= 0 && strings.IndexByte(pwSpecial, pw[3]) >= 0 {
			inSeedOrder++
		}
	}
	// One draw landing back in class order is ordinary chance (~0.3%); all 200 of
	// them means the shuffle never ran.
	if inSeedOrder == draws {
		t.Errorf("all %d draws have the seeded classes at positions 0..3 in order — the shuffle is not running", draws)
	}
}

// TestRandIndex_NonPositiveIsZero pins randIndex's guard. It is load-bearing:
// crypto/rand.Int PANICS on a non-positive max, so a guard that lets n == 0
// through takes the CLI down instead of returning 0.
func TestRandIndex_NonPositiveIsZero(t *testing.T) {
	for _, n := range []int{0, -1, -100} {
		if got := randIndex(n); got != 0 {
			t.Errorf("randIndex(%d) = %d, want 0", n, got)
		}
	}
}

// TestRandIndex_InRange keeps the positive path honest: every draw lands in [0,n).
func TestRandIndex_InRange(t *testing.T) {
	for _, n := range []int{1, 2, 8, 20, 69} {
		for i := 0; i < 100; i++ {
			if got := randIndex(n); got < 0 || got >= n {
				t.Fatalf("randIndex(%d) = %d, want [0,%d)", n, got, n)
			}
		}
	}
}
