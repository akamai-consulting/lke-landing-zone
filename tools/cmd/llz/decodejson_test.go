package main

// A COPY of the response decoder that moved to internal/keycloak with the client.
// The configure verb's mutation test still builds responses this way.

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// decodeJSON reads a JSON array/object body, requiring a 2xx status.
func decodeJSON(resp *http.Response, v any) error {
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, readSnippet(resp.Body))
	}
	return json.NewDecoder(resp.Body).Decode(v)
}
