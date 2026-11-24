// Package manifest defines the plugin manifest format and its validation rules.
package manifest

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

const (
	// SchemaVersion is the manifest format version understood by this host.
	SchemaVersion = 1
	// HostAPIVersion is the Host API version implemented by this host.
	HostAPIVersion = 1
)

var (
	idPattern      = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9_-]{0,62}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9_-]{0,62}[a-z0-9])?)*$`)
	versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)
)

// Manifest describes a plugin package.
type Manifest struct {
	SchemaVersion int                    `json:"schemaVersion"`
	ID            string                 `json:"id"`
	Name          string                 `json:"name"`
	Version       string                 `json:"version"`
	HostAPI       HostAPI                `json:"hostApi"`
	Entry         Entry                  `json:"entry"`
	Config        map[string]ConfigField `json:"config,omitempty"`
	Capabilities  []string               `json:"capabilities,omitempty"`
}

// HostAPI is the inclusive range of Host API versions a plugin supports.
type HostAPI struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

// Entry names the optional web and Lua entry points, relative to the package root.
type Entry struct {
	Web string `json:"web,omitempty"`
	Lua string `json:"lua,omitempty"`
}

// ConfigField describes one configuration value.
type ConfigField struct {
	Type     string          `json:"type"`
	Required bool            `json:"required,omitempty"`
	Secret   bool            `json:"secret,omitempty"`
	Default  json.RawMessage `json:"default,omitempty"`
}

// Parse decodes and validates a manifest.
func Parse(data []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("manifest.json: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate checks the manifest against the host's supported schema and API.
func (m *Manifest) Validate() error {
	if m.SchemaVersion != SchemaVersion {
		return fmt.Errorf("manifest.json: unsupported schemaVersion %d (host supports %d)", m.SchemaVersion, SchemaVersion)
	}
	if !validID(m.ID) {
		return fmt.Errorf("manifest.json: invalid id %q (use dot-separated lowercase segments)", m.ID)
	}
	if strings.TrimSpace(m.Name) == "" {
		return fmt.Errorf("manifest.json: name is required")
	}
	if !versionPattern.MatchString(m.Version) {
		return fmt.Errorf("manifest.json: invalid version %q (want semantic version, e.g. 1.2.3)", m.Version)
	}
	if m.HostAPI.Min < 1 || m.HostAPI.Max < m.HostAPI.Min {
		return fmt.Errorf("manifest.json: invalid hostApi range %d-%d", m.HostAPI.Min, m.HostAPI.Max)
	}
	if HostAPIVersion < m.HostAPI.Min || HostAPIVersion > m.HostAPI.Max {
		return fmt.Errorf("manifest.json: plugin requires host API %d-%d, host provides %d", m.HostAPI.Min, m.HostAPI.Max, HostAPIVersion)
	}
	if m.Entry.Web == "" && m.Entry.Lua == "" {
		return fmt.Errorf("manifest.json: at least one of entry.web or entry.lua is required")
	}
	for name, entry := range map[string]string{"entry.web": m.Entry.Web, "entry.lua": m.Entry.Lua} {
		if entry == "" {
			continue
		}
		if !ValidPackagePath(entry) {
			return fmt.Errorf("manifest.json: %s %q must be a relative path inside the package", name, entry)
		}
	}
	for name, field := range m.Config {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("manifest.json: config contains an empty field name")
		}
		if err := field.validate(name); err != nil {
			return err
		}
	}
	for _, capability := range m.Capabilities {
		if strings.TrimSpace(capability) == "" {
			return fmt.Errorf("manifest.json: capabilities contains an empty value")
		}
	}
	return nil
}

func (f ConfigField) validate(name string) error {
	switch f.Type {
	case "string":
		if f.Secret {
			break
		}
	case "number", "boolean":
		if f.Secret {
			return fmt.Errorf("manifest.json: config field %q: secret is only supported for strings", name)
		}
	default:
		return fmt.Errorf("manifest.json: config field %q: unsupported type %q", name, f.Type)
	}
	if _, err := f.DefaultValue(); err != nil {
		return fmt.Errorf("manifest.json: config field %q: %w", name, err)
	}
	return nil
}

// DefaultValue returns the declared default, or (nil, nil) when absent.
func (f ConfigField) DefaultValue() (any, error) {
	if len(f.Default) == 0 || string(f.Default) == "null" {
		return nil, nil
	}
	var value any
	if err := json.Unmarshal(f.Default, &value); err != nil {
		return nil, fmt.Errorf("invalid default: %w", err)
	}
	return NormalizeValue(f.Type, value)
}

// Defaults returns the configuration defaults declared by the manifest.
func (m *Manifest) Defaults() (map[string]any, error) {
	values := make(map[string]any, len(m.Config))
	for name, field := range m.Config {
		value, err := field.DefaultValue()
		if err != nil {
			return nil, fmt.Errorf("config field %q: %w", name, err)
		}
		if value != nil {
			values[name] = value
		}
	}
	return values, nil
}

// NormalizeValue checks that value matches the declared type and returns its canonical form.
func (f ConfigField) NormalizeValue(value any) (any, error) {
	return NormalizeValue(f.Type, value)
}

// NormalizeValue checks a plain JSON value against a configuration type.
func NormalizeValue(typ string, value any) (any, error) {
	switch typ {
	case "string":
		s, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("want string, got %T", value)
		}
		return s, nil
	case "number":
		switch n := value.(type) {
		case float64:
			return n, nil
		case float32:
			return float64(n), nil
		case int:
			return float64(n), nil
		case int64:
			return float64(n), nil
		case json.Number:
			f, err := n.Float64()
			if err != nil {
				return nil, fmt.Errorf("invalid number %q", n)
			}
			return f, nil
		default:
			return nil, fmt.Errorf("want number, got %T", value)
		}
	case "boolean":
		b, ok := value.(bool)
		if !ok {
			return nil, fmt.Errorf("want boolean, got %T", value)
		}
		return b, nil
	default:
		return nil, fmt.Errorf("unsupported type %q", typ)
	}
}

// Redacted returns a copy of values with secret fields removed.
func (m *Manifest) Redacted(values map[string]any) map[string]any {
	result := make(map[string]any, len(values))
	for name, value := range values {
		if field, ok := m.Config[name]; ok && field.Secret {
			continue
		}
		result[name] = value
	}
	return result
}

// SecretKeys returns the configuration field names marked as secret.
func (m *Manifest) SecretKeys() []string {
	var keys []string
	for name, field := range m.Config {
		if field.Secret {
			keys = append(keys, name)
		}
	}
	return keys
}

// MergeConfig validates values against the schema, merges them into current and
// verifies that required fields remain present.
func (m *Manifest) MergeConfig(current, values map[string]any) (map[string]any, error) {
	merged := make(map[string]any, len(current)+len(values))
	for name, value := range current {
		merged[name] = value
	}
	for name, value := range values {
		field, ok := m.Config[name]
		if !ok {
			return nil, fmt.Errorf("unknown config field %q", name)
		}
		if value == nil {
			delete(merged, name)
			continue
		}
		normalized, err := field.NormalizeValue(value)
		if err != nil {
			return nil, fmt.Errorf("config field %q: %w", name, err)
		}
		merged[name] = normalized
	}
	for name, field := range m.Config {
		if !field.Required {
			continue
		}
		value, ok := merged[name]
		if !ok {
			return nil, fmt.Errorf("config field %q is required", name)
		}
		if s, isString := value.(string); isString && strings.TrimSpace(s) == "" {
			return nil, fmt.Errorf("config field %q is required", name)
		}
	}
	return merged, nil
}

func validID(id string) bool {
	return len(id) > 0 && len(id) <= 128 && idPattern.MatchString(id)
}

// ValidPackagePath reports whether name is a clean relative path inside a package.
func ValidPackagePath(name string) bool {
	if name == "" || strings.ContainsAny(name, "\\\x00") || strings.HasPrefix(name, "/") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}
