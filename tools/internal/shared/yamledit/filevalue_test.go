package yamledit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestFileValueNodeSetsAStructuredSubtree(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "openbao.yaml")
	if err := os.WriteFile(f, []byte("mounts:\n  - path: pki-x\n    type: pki\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte("apiVersion: v\nspec:\n  teams: [] # keep me\n"), &doc); err != nil {
		t.Fatal(err)
	}
	node, ok, err := FileValueNode("@" + f)
	if err != nil || !ok {
		t.Fatalf("FileValueNode: ok=%v err=%v", ok, err)
	}
	if err := SetSpecPathNode(&doc, "openbao", node); err != nil {
		t.Fatal(err)
	}
	out, _ := yaml.Marshal(&doc)
	for _, want := range []string{"openbao:", "path: pki-x", "type: pki", "# keep me"} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("rendered spec lacks %q:\n%s", want, out)
		}
	}
	// Setting again REPLACES rather than appending a duplicate key.
	if err := SetSpecPathNode(&doc, "openbao", node); err != nil {
		t.Fatal(err)
	}
	out, _ = yaml.Marshal(&doc)
	if n := strings.Count(string(out), "openbao:"); n != 1 {
		t.Fatalf("openbao key appears %d times", n)
	}
}

func TestFileValueNodeScalarsAndErrors(t *testing.T) {
	if _, ok, err := FileValueNode("plain"); ok || err != nil {
		t.Fatalf("a plain value is not a file: ok=%v err=%v", ok, err)
	}
	if _, ok, err := FileValueNode("@/nonexistent/x.yaml"); !ok || err == nil {
		t.Fatalf("a missing file must be an error: ok=%v err=%v", ok, err)
	}
	empty := filepath.Join(t.TempDir(), "e.yaml")
	_ = os.WriteFile(empty, nil, 0o600)
	if _, _, err := FileValueNode("@" + empty); err == nil {
		t.Fatal("an empty file must be an error")
	}
}

func TestSetSpecPathNodeErrorsAndNesting(t *testing.T) {
	parse := func(src string) *yaml.Node {
		var doc yaml.Node
		if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
			t.Fatal(err)
		}
		return &doc
	}
	val := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	if err := SetSpecPathNode(parse("spec: {}\n"), "", val); err == nil {
		t.Error("empty path must be an error")
	}
	if err := SetSpecPathNode(parse("spec: {}\n"), ".x", val); err == nil {
		t.Error("a path with an empty first key must be an error")
	}
	if err := SetSpecPathNode(parse("spec: {}\n"), "openbao", nil); err == nil {
		t.Error("a nil value must be an error")
	}
	if err := SetSpecPathNode(parse("spec:\n  dns: scalar\n"), "dns.x", val); err == nil || !strings.Contains(err.Error(), "non-mapping") {
		t.Errorf("crossing a scalar must be an error, got %v", err)
	}
	// A nested path creates the intermediate mappings.
	doc := parse("spec: {}\n")
	if err := SetSpecPathNode(doc, "a.b", val); err != nil {
		t.Fatal(err)
	}
	if out, _ := yaml.Marshal(doc); !strings.Contains(string(out), "a:") || !strings.Contains(string(out), "b: {}") {
		t.Fatalf("nested set not rendered:\n%s", out)
	}
}

func TestFileValueNodeRejectsMalformedYAML(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(bad, []byte("a: [unclosed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := FileValueNode("@" + bad); !ok || err == nil || !strings.Contains(err.Error(), "parse") {
		t.Fatalf("malformed YAML must be a parse error: ok=%v err=%v", ok, err)
	}
}
