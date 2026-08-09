package cli

import "encoding/base64"

// Helpers the moved tests use, copied across the new package boundary.

// base64Auth is the `username:password` docker-auth blob (mirrors the module's
// base64encode("${username}:${token}")).
func base64Auth(username, token string) string {
	return base64.StdEncoding.EncodeToString([]byte(username + ":" + token))
}
