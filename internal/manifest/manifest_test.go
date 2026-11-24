package manifest

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseValid(t *testing.T) {
	data := []byte(`{
		"schemaVersion": 1,
		"id": "com.godesk.hello",
		"name": "Hello",
		"version": "0.1.0",
		"hostApi": {"min": 1, "max": 1},
		"entry": {"web": "web/index.html", "lua": "lua/init.lua"},
		"config": {"greeting": {"type": "string", "default": "hi"}},
		"capabilities": ["storage"]
	}`)
	parsed, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if parsed.ID != "com.godesk.hello" {
		t.Fatalf("ID = %q", parsed.ID)
	}
	defaults, err := parsed.Defaults()
	if err != nil {
		t.Fatalf("Defaults: %v", err)
	}
	if defaults["greeting"] != "hi" {
		t.Fatalf("default = %#v", defaults["greeting"])
	}
}

func TestParseRejectsInvalidManifests(t *testing.T) {
	valid := func(mutate func(map[string]any)) []byte {
		value := map[string]any{
			"schemaVersion": 1,
			"id":            "com.godesk.test",
			"name":          "Test",
			"version":       "1.0.0",
			"hostApi":       map[string]any{"min": 1, "max": 1},
			"entry":         map[string]any{"lua": "lua/init.lua"},
		}
		mutate(value)
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}

	tests := []struct {
		name    string
		data    []byte
		wantErr string
	}{
		{"bad schema", valid(func(m map[string]any) { m["schemaVersion"] = 2 }), "schemaVersion"},
		{"bad id", valid(func(m map[string]any) { m["id"] = "../escape" }), "invalid id"},
		{"dot id", valid(func(m map[string]any) { m["id"] = "com..test" }), "invalid id"},
		{"bad version", valid(func(m map[string]any) { m["version"] = "1.0" }), "invalid version"},
		{"bad api range", valid(func(m map[string]any) { m["hostApi"] = map[string]any{"min": 2, "max": 1} }), "hostApi"},
		{"incompatible api", valid(func(m map[string]any) { m["hostApi"] = map[string]any{"min": 2, "max": 3} }), "host provides 1"},
		{"no entry", valid(func(m map[string]any) { m["entry"] = map[string]any{} }), "at least one"},
		{"absolute entry", valid(func(m map[string]any) { m["entry"] = map[string]any{"lua": "/etc/passwd"} }), "entry.lua"},
		{"traversal entry", valid(func(m map[string]any) { m["entry"] = map[string]any{"lua": "../init.lua"} }), "entry.lua"},
		{"bad config type", valid(func(m map[string]any) {
			m["config"] = map[string]any{"x": map[string]any{"type": "object"}}
		}), "unsupported type"},
		{"secret non-string", valid(func(m map[string]any) {
			m["config"] = map[string]any{"x": map[string]any{"type": "number", "secret": true}}
		}), "secret is only"},
		{"wrong default", valid(func(m map[string]any) {
			m["config"] = map[string]any{"x": map[string]any{"type": "string", "default": 3}}
		}), "want string"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Parse(test.data)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error %q does not contain %q", err, test.wantErr)
			}
		})
	}
}

func TestMergeConfig(t *testing.T) {
	m := &Manifest{
		Config: map[string]ConfigField{
			"greeting": {Type: "string"},
			"token":    {Type: "string", Secret: true},
			"enabled":  {Type: "boolean"},
			"required": {Type: "string", Required: true},
		},
	}
	merged, err := m.MergeConfig(map[string]any{"greeting": "hi", "required": "yes"}, map[string]any{
		"greeting": "hello",
		"enabled":  true,
	})
	if err != nil {
		t.Fatalf("MergeConfig: %v", err)
	}
	if merged["greeting"] != "hello" || merged["enabled"] != true || merged["required"] != "yes" {
		t.Fatalf("merged = %#v", merged)
	}
	if _, err := m.MergeConfig(merged, map[string]any{"unknown": "x"}); err == nil {
		t.Fatal("unknown field accepted")
	}
	if _, err := m.MergeConfig(merged, map[string]any{"enabled": "yes"}); err == nil {
		t.Fatal("wrong type accepted")
	}
	if _, err := m.MergeConfig(map[string]any{}, map[string]any{}); err == nil {
		t.Fatal("missing required field accepted")
	}
	redacted := m.Redacted(map[string]any{"greeting": "hi", "token": "secret"})
	if _, ok := redacted["token"]; ok {
		t.Fatal("secret was not redacted")
	}
}
