//go:build !race

package cli

// See racegate_on_test.go — this is the half that compiles when -race is absent.
const raceDetectorLinked = false
