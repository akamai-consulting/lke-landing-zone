//go:build race

package cli

// raceDetectorLinked reports whether this test binary was built with -race. The
// two build-tagged halves of this constant are the whole mechanism: the Go
// toolchain sets the `race` build tag if and only if -race was passed, and there
// is no runtime API that answers the question.
const raceDetectorLinked = true
