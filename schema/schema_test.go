// Package schema ships the JSON Schema for Fleetplane's config.yaml
// (editor autocomplete + hover docs). These tests keep the schema honest:
// it must compile, accept the shipped example config, and reject the same
// mistakes the strict Go decoder rejects.
package schema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

const schemaFile = "config.schema.json"

func compileSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	raw, err := os.ReadFile(schemaFile)
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(schemaFile, doc); err != nil {
		t.Fatalf("add schema resource: %v", err)
	}
	sch, err := c.Compile(schemaFile)
	if err != nil {
		t.Fatalf("compile schema: %v", err)
	}
	return sch
}

// yamlToInstance decodes YAML into a JSON-shaped instance (string keys,
// json.Number numbers) as the jsonschema validator expects.
func yamlToInstance(t *testing.T, src []byte) any {
	t.Helper()
	var doc any
	if err := yaml.Unmarshal(src, &doc); err != nil {
		t.Fatalf("yaml decode: %v", err)
	}
	buf, err := json.Marshal(normalizeKeys(doc))
	if err != nil {
		t.Fatalf("json re-encode: %v", err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("json re-decode: %v", err)
	}
	return inst
}

// normalizeKeys forces every mapping key to a string so the YAML document
// round-trips through encoding/json regardless of how goccy typed the keys.
func normalizeKeys(v any) any {
	switch x := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, val := range x {
			m[k] = normalizeKeys(val)
		}
		return m
	case map[any]any:
		m := make(map[string]any, len(x))
		for k, val := range x {
			m[fmt.Sprint(k)] = normalizeKeys(val)
		}
		return m
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = normalizeKeys(val)
		}
		return out
	default:
		return v
	}
}

func TestSchemaCompiles(t *testing.T) {
	compileSchema(t)
}

func TestExampleConfigValidates(t *testing.T) {
	sch := compileSchema(t)
	raw, err := os.ReadFile("../examples/config.yaml")
	if err != nil {
		t.Fatalf("read example config: %v", err)
	}
	// Strip the $schema modeline (a YAML comment, but the deliverable is
	// "the config itself validates", so drop it explicitly).
	src := string(raw)
	if first, rest, ok := strings.Cut(src, "\n"); ok && strings.HasPrefix(first, "# yaml-language-server:") {
		src = rest
	}
	inst := yamlToInstance(t, []byte(src))
	if err := sch.Validate(inst); err != nil {
		t.Fatalf("examples/config.yaml does not validate against the schema:\n%v", err)
	}
}

func TestUnknownTopLevelKeyFails(t *testing.T) {
	sch := compileSchema(t)
	inst := yamlToInstance(t, []byte(`
storage:
  path: /var/lib/fleetplane/fleetplane.db
serverr:
  addr: ":8080"
`))
	if err := sch.Validate(inst); err == nil {
		t.Fatal("config with unknown top-level key 'serverr' validated; want failure (strict decoding, ADR-007)")
	}
}

func TestForeignDriverSettingsKeyFails(t *testing.T) {
	sch := compileSchema(t)
	inst := yamlToInstance(t, []byte(`
storage:
  path: /var/lib/fleetplane/fleetplane.db
providers:
  main:
    driver: hetzner
    settings:
      token: secret://env/HETZNER_TOKEN
      subnetId: subnet-0abc123 # aws-only key under driver: hetzner
`))
	if err := sch.Validate(inst); err == nil {
		t.Fatal("driver: hetzner with aws-only settings key 'subnetId' validated; want failure")
	}
}
