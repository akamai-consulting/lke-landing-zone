package main

// yamledit.go is the comment-preserving YAML mutation primitive behind the spec
// WRITE commands (`llz env set`, `llz network add`). They edit the declarative
// source in place — landingzone.yaml / environments/<env>.yaml stay the source of
// truth — using yaml.v3's Node API so an operator's comments survive a `set`.

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	yaml "gopkg.in/yaml.v3"
)

// editSpecFile applies mutate to a spec file, but COMMITS only if the result still
// parses strictly — so a typo'd / unknown-field path can't poison the file and
// break every subsequent spec command. parse is the strict decoder for the file's
// kind (rejects unknown fields). On failure it restores the original bytes and
// returns a clear, reverted error.
func editSpecFile(path string, mutate func(*yaml.Node) error, parse func([]byte) error) error {
	orig, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := editYAMLFile(path, mutate); err != nil {
		return err
	}
	edited, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if perr := parse(edited); perr != nil {
		_ = os.WriteFile(path, orig, 0o644) // roll back — never leave a poisoned file
		return fmt.Errorf("change rejected — %s left unchanged: %s\n  (check the path against `llz env show` / docs/landing-zone-spec.md)",
			filepath.Base(path), cleanFieldErr(perr))
	}
	return nil
}

// cleanFieldErr trims the raw json-unmarshal noise to the actionable bit (the
// "unknown field …" / "cannot unmarshal …" tail).
func cleanFieldErr(err error) string {
	s := err.Error()
	for _, marker := range []string{"unknown field", "cannot unmarshal"} {
		if i := strings.LastIndex(s, marker); i >= 0 {
			return s[i:]
		}
	}
	return s
}

// isPerEnvPath reports whether a spec.<path> belongs in environments/<env>.yaml
// (cluster.* / components.*) vs. instance-wide landingzone.yaml (instance / dns /
// defaults / networks). Drives the routing between `llz env set` and `llz spec set`.
func isPerEnvPath(dotted string) bool {
	head := dotted
	if i := strings.IndexByte(dotted, '.'); i >= 0 {
		head = dotted[:i]
	}
	return head == "cluster" || head == "components"
}

// editYAMLFile loads path as a YAML document, hands the document node to mutate,
// and writes it back with 2-space indent (matching the authored files).
func editYAMLFile(path string, mutate func(doc *yaml.Node) error) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	if len(doc.Content) == 0 {
		return fmt.Errorf("%s is empty", path)
	}
	if err := mutate(&doc); err != nil {
		return err
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return err
	}
	_ = enc.Close()
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

// setSpecPath sets spec.<dotted> = value in a parsed document, creating any
// intermediate mappings. The leaf's YAML type is inferred (bool/int/else string),
// so `cluster.nodePool.count=8` writes an int and `components.harbor.enabled=false`
// a bool. Comments on untouched nodes are preserved.
func setSpecPath(doc *yaml.Node, dotted, value string) error {
	keys := strings.Split(dotted, ".")
	if dotted == "" || keys[0] == "" {
		return fmt.Errorf("empty path")
	}
	cur := childMapping(doc.Content[0], "spec")
	for _, k := range keys[:len(keys)-1] {
		next := childMapping(cur, k)
		if next == nil {
			return fmt.Errorf("path %q crosses a non-mapping at %q", dotted, k)
		}
		cur = next
	}
	setScalarChild(cur, keys[len(keys)-1], value)
	return nil
}

// childMapping returns the mapping value for key under m, creating an empty
// mapping if absent. Returns nil if key exists but isn't a mapping.
func childMapping(m *yaml.Node, key string) *yaml.Node {
	if m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			if m.Content[i+1].Kind != yaml.MappingNode {
				return nil
			}
			return m.Content[i+1]
		}
	}
	v := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, v)
	return v
}

// setScalarChild sets m[key] = value as a typed scalar, replacing an existing
// value node in place (so its key comment survives) or appending a new key.
func setScalarChild(m *yaml.Node, key, value string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			n := m.Content[i+1]
			n.Kind, n.Style, n.Content = yaml.ScalarNode, 0, nil
			n.Tag, n.Value = inferScalarTag(value), value
			return
		}
	}
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: inferScalarTag(value), Value: value})
}

// inferScalarTag picks the YAML scalar type for a raw CLI value: true/false →
// bool, a bare integer → int, everything else (CIDRs, versions, regions) → string.
func inferScalarTag(v string) string {
	switch {
	case v == "true" || v == "false":
		return "!!bool"
	case isInt(v):
		return "!!int"
	default:
		return "!!str"
	}
}

func isInt(v string) bool {
	_, err := strconv.Atoi(v)
	return err == nil
}

// parseAssignments splits "a.b=c" CLI args into (path, value) pairs.
func parseAssignments(args []string) ([][2]string, error) {
	out := make([][2]string, 0, len(args))
	for _, a := range args {
		i := strings.IndexByte(a, '=')
		if i <= 0 {
			return nil, fmt.Errorf("invalid assignment %q — want path=value (e.g. cluster.nodePool.count=8)", a)
		}
		out = append(out, [2]string{strings.TrimSpace(a[:i]), strings.TrimSpace(a[i+1:])})
	}
	return out, nil
}
