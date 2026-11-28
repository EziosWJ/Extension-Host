package manifest

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseValid(t *testing.T) {
	data := []byte(`{
		"schemaVersion": 1,
		"id": "com.example.hello",
		"name": "Hello",
		"version": "0.1.0",
		"hostApi": {"min": 1, "max": 1},
		"entry": {"web": "web/index.html", "lua": "lua/init.lua"},
		"config": {"greeting": {"type": "string", "default": "hi"}}
	}`)
	parsed, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if parsed.ID != "com.example.hello" {
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
			"id":            "com.example.test",
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

const marketManifest = `{
  "schemaVersion": 1,
  "id": "de.jianwang.backup-tool",
  "name": "Backup Tool",
  "version": "0.1.0",
  "hostApi": {"min": 1, "max": 1},
  "entry": {"lua": "lua/init.lua"},
  "description": "Back up a database.",
  "author": {"name": "Example Author", "url": "https://example.org", "email": "author@example.org"},
  "license": "MIT",
  "homepage": "https://example.org/backup",
  "repository": "https://example.org/src/backup",
  "icon": "assets/icon.WeBp",
  "categories": ["database", "ops"],
  "keywords": ["db-backup", "postgresql"],
  "requirements": {"os": ["linux"], "commands": ["pg_dump"], "systemd": true},
  "config": {"backupDir": {"type": "string", "label": "Backup Directory", "description": "Path for archives."}},
  "locales": {"zh-CN": {"name": "备份工具", "description": "备份数据库。", "config": {
    "backupDir": {"label": "备份目录", "description": "归档文件存放路径。"}
  }}}
}`

func TestParseMarketMetadata(t *testing.T) {
	m, err := Parse([]byte(marketManifest))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.ValidateStrict(); err != nil {
		t.Fatal(err)
	}
	if m.Description != "Back up a database." || m.Author.Email != "author@example.org" || m.Author.URL != "https://example.org" ||
		m.License != "MIT" || m.Homepage != "https://example.org/backup" || m.Repository != "https://example.org/src/backup" ||
		m.Icon != "assets/icon.WeBp" || m.Categories[0] != "database" || m.Keywords[0] != "db-backup" {
		t.Fatalf("unexpected metadata: %+v", m)
	}
	if !m.Requirements.Systemd || m.Requirements.Commands[0] != "pg_dump" || m.Requirements.OS[0] != "linux" {
		t.Fatalf("unexpected requirements: %+v", m.Requirements)
	}
	field := m.Config["backupDir"]
	locale := m.Locales["zh-CN"]
	translation := locale.Config["backupDir"]
	if field.Label != "Backup Directory" || field.Description != "Path for archives." ||
		locale.Name != "备份工具" || locale.Description != "备份数据库。" ||
		translation.Label != "备份目录" || translation.Description != "归档文件存放路径。" {
		t.Fatalf("unexpected config and localization: %+v, %+v", field, locale)
	}
}

func TestValidStrictID(t *testing.T) {
	maxID := strings.Repeat("a", 62) + "." + strings.Repeat("b", 63) + ".c"
	for _, id := range []string{"de.jianwang.backup-tool", "io.github.jianwang.demo", "com.example2.test", "a.0.c", maxID, "de.jianwang." + strings.Repeat("a", 64)} {
		if !ValidStrictID(id) {
			t.Errorf("rejected %q", id)
		}
	}
	for _, id := range []string{
		"", "de.jianwang", "de.Jianwang.demo", "de.jianwang.backup_tool", "de..demo",
		"de.jianwang.backup--tool", "de.jianwang.-demo", "de.jianwang.demo-", "de.jianwang.示例",
		"com.example", "com.godesk", "com.example.hello", "com.godesk.hello", maxID + "c",
		"de.jianwang." + strings.Repeat("a", 65),
	} {
		if ValidStrictID(id) {
			t.Errorf("accepted %q", id)
		}
	}
}

func TestMinimalManifestValidation(t *testing.T) {
	for _, id := range []string{"de.backup_tool", "de.jianwang.backup_tool", "com.example.backup-tool", "com.godesk.backup-tool"} {
		data := `{"schemaVersion":1,"id":"` + id + `","name":"Backup","version":"1.0.0","hostApi":{"min":1,"max":1},"entry":{"lua":"lua/init.lua"},"config":{"备份目录":{"type":"string"}}}`
		m, err := Parse([]byte(data))
		if err != nil {
			t.Fatalf("Parse %q: %v", id, err)
		}
		if err := m.Validate(); err != nil {
			t.Fatalf("Validate %q: %v", id, err)
		}
		if err := m.ValidateStrict(); err == nil || !strings.Contains(err.Error(), "id ") {
			t.Fatalf("ValidateStrict %q: %v", id, err)
		}
	}
}

func TestParseRejectsInvalidMarketStructure(t *testing.T) {
	for _, tc := range []struct{ name, old, replacement, want string }{
		{"icon traversal", "assets/icon.WeBp", "../icon.png", "icon"},
		{"icon URL", "assets/icon.WeBp", "https://example.org/icon.svg", "icon"},
		{"icon extension", "assets/icon.WeBp", "assets/icon.html", "icon"},
		{"blank language", `"zh-CN"`, `" "`, "language code"},
		{"blank config reference", `"backupDir": {"label"`, `"": {"label"`, "empty field name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(strings.Replace(marketManifest, tc.old, tc.replacement, 1)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestValidateStrict(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		edit       func(*Manifest)
	}{
		{"missing description", "description", func(m *Manifest) { m.Description = " " }},
		{"long description", "description", func(m *Manifest) { m.Description = strings.Repeat("字", 161) }},
		{"newline", "description", func(m *Manifest) { m.Description = "Line\nTwo" }},
		{"carriage return", "description", func(m *Manifest) { m.Description = "Line\rTwo" }},
		{"missing author", "author.name", func(m *Manifest) { m.Author = nil }},
		{"blank author", "author.name", func(m *Manifest) { m.Author.Name = "  " }},
		{"invalid author URL", "author.url", func(m *Manifest) { m.Author.URL = "http://example.org" }},
		{"blank license", "license", func(m *Manifest) { m.License = "\t" }},
		{"plain HTTP", "homepage", func(m *Manifest) { m.Homepage = "http://example.org" }},
		{"missing host", "repository", func(m *Manifest) { m.Repository = "https:///src" }},
		{"empty hostname", "repository", func(m *Manifest) { m.Repository = "https://:443" }},
		{"malformed URL", "homepage", func(m *Manifest) { m.Homepage = "https://bad host" }},
		{"too many categories", "categories", func(m *Manifest) { m.Categories = []string{"database", "ops", "proxy", "container"} }},
		{"duplicate category", "categories", func(m *Manifest) { m.Categories = []string{"ops", "ops"} }},
		{"unknown category", "categories", func(m *Manifest) { m.Categories = []string{"backup"} }},
		{"too many keywords", "keywords", func(m *Manifest) { m.Keywords = strings.Fields("a b c d e f g h i") }},
		{"duplicate keyword", "keywords", func(m *Manifest) { m.Keywords = []string{"db", "db"} }},
		{"invalid keyword", "keywords", func(m *Manifest) { m.Keywords = []string{"db--backup"} }},
		{"empty OS entry", "requirements.os", func(m *Manifest) { m.Requirements.OS = []string{"linux", ""} }},
		{"blank OS entry", "requirements.os", func(m *Manifest) { m.Requirements.OS = []string{" \t"} }},
		{"empty command entry", "requirements.commands", func(m *Manifest) { m.Requirements.Commands = []string{""} }},
		{"blank command entry", "requirements.commands", func(m *Manifest) { m.Requirements.Commands = []string{"pg_dump", " \t"} }},
		{"snake case key", "config key", func(m *Manifest) { m.Config["backup_dir"] = ConfigField{Type: "string"} }},
		{"non-English key", "config key", func(m *Manifest) { m.Config["目录"] = ConfigField{Type: "string"} }},
		{"uppercase key", "config key", func(m *Manifest) { m.Config["BackupDir"] = ConfigField{Type: "string"} }},
		{"unknown config reference", "unknown field", func(m *Manifest) { delete(m.Config, "backupDir") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := Parse([]byte(marketManifest))
			if err != nil {
				t.Fatal(err)
			}
			tc.edit(m)
			if err := m.Validate(); err != nil {
				t.Fatalf("ordinary validation: %v", err)
			}
			if err := m.ValidateStrict(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ValidateStrict = %v, want %q", err, tc.want)
			}
		})
	}
	m, err := Parse([]byte(marketManifest))
	if err != nil {
		t.Fatal(err)
	}
	m.Description = strings.Repeat("字", 160)
	m.Author.URL = ""
	m.Homepage, m.Repository, m.Icon = "", "", ""
	m.Categories, m.Keywords, m.Requirements, m.Locales = nil, nil, nil, nil
	if err := m.ValidateStrict(); err != nil {
		t.Fatalf("strict validation with optional metadata omitted: %v", err)
	}
}

func TestValidateStrictRejectsUnknownRequirements(t *testing.T) {
	data := strings.Replace(marketManifest, `"systemd": true`, `"systemd": true, "grants": ["anything"]`, 1)
	m, err := Parse([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.ValidateStrict(); err == nil || !strings.Contains(err.Error(), "requirements") {
		t.Fatalf("ValidateStrict = %v, want unsupported requirements field", err)
	}
	encoded, err := json.Marshal(m.Requirements)
	if err != nil || strings.Contains(string(encoded), "grants") {
		t.Fatalf("requirements JSON = %s, %v", encoded, err)
	}
}
