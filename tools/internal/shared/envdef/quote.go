package envdef

// quote is one line, copied rather than shared. It wraps a YAML scalar in double
// quotes; a package boundary for that would cost more to read than it saves.
func quote(s string) string { return `"` + s + `"` }
