package identity

// password.go — the random temporary-password generator for onboarding. The
// default flow mints a one-time password the user must change at first login, so
// it need only be strong + satisfy a typical Keycloak password policy (one of
// each class), not memorable.

import (
	"crypto/rand"
	"math/big"
)

const (
	pwLower   = "abcdefghijkmnopqrstuvwxyz" // no l
	pwUpper   = "ABCDEFGHJKLMNPQRSTUVWXYZ"  // no I, O
	pwDigit   = "23456789"                  // no 0, 1
	pwSpecial = "!@#$%^&*-_=+"              // special set
	pwLen     = 20
)

// RandomPassword returns a cryptographically-random password of pwLen runes that
// contains at least one lower, upper, digit, and special character (so it clears
// a default Keycloak password policy). It seeds the four guaranteed classes, fills
// the rest from the full alphabet, then shuffles so the guaranteed characters are
// not in fixed positions.
func RandomPassword() string {
	classes := []string{pwLower, pwUpper, pwDigit, pwSpecial}
	all := pwLower + pwUpper + pwDigit + pwSpecial

	buf := make([]byte, 0, pwLen)
	for _, class := range classes { // one guaranteed char per class
		buf = append(buf, class[randIndex(len(class))])
	}
	for len(buf) < pwLen { // fill the remainder from everything
		buf = append(buf, all[randIndex(len(all))])
	}

	// Fisher–Yates shuffle so the guaranteed characters aren't a fixed prefix.
	for i := len(buf) - 1; i > 0; i-- {
		j := randIndex(i + 1)
		buf[i], buf[j] = buf[j], buf[i]
	}
	return string(buf)
}

// randIndex returns a uniform random int in [0,n) using crypto/rand.
func randIndex(n int) int {
	if n <= 0 {
		return 0
	}
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		// crypto/rand should never fail; fall back to 0 rather than panic in a CLI.
		return 0
	}
	return int(v.Int64())
}
