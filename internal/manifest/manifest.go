// Package manifest defines the plugin manifest format and its validation rules.
package manifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	// SchemaVersion is the manifest format version understood by this host.
	SchemaVersion = 1
	// HostAPIVersion is the Host API version implemented by this host.
	HostAPIVersion = 1
)

// ErrHostAPIIncompatible means the manifest's valid range excludes this Host API.
var ErrHostAPIIncompatible = errors.New("plugin requires host API")

var (
	idPattern      = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9_-]{0,62}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9_-]{0,62}[a-z0-9])?)*$`)
	versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)
	methodPattern  = regexp.MustCompile(`^[A-Za-z0-9_]+(\.[A-Za-z0-9_]+)*$`)
)

// Manifest describes a plugin package.
type Manifest struct {
	SchemaVersion  int                    `json:"schemaVersion"`
	ID             string                 `json:"id"`
	Name           string                 `json:"name"`
	Version        string                 `json:"version"`
	MinHostVersion string                 `json:"minHostVersion,omitempty"`
	HostAPI        HostAPI                `json:"hostApi"`
	Entry          Entry                  `json:"entry"`
	Config         map[string]ConfigField `json:"config,omitempty"`
	Native         *Native                `json:"native,omitempty"`
	Description    string                 `json:"description,omitempty"`
	Author         *Author                `json:"author,omitempty"`
	License        string                 `json:"license,omitempty"`
	Homepage       string                 `json:"homepage,omitempty"`
	Repository     string                 `json:"repository,omitempty"`
	Icon           string                 `json:"icon,omitempty"`
	Categories     []string               `json:"categories,omitempty"`
	Keywords       []string               `json:"keywords,omitempty"`
	Requirements   *Requirements          `json:"requirements,omitempty"`
	Locales        map[string]Locale      `json:"locales,omitempty"`
}

// Author identifies the plugin publisher or maintainer.
type Author struct {
	Name  string `json:"name"`
	URL   string `json:"url,omitempty"`
	Email string `json:"email,omitempty"`
}

// Requirements describes environment hints for the installer.
type Requirements struct {
	OS       []string `json:"os,omitempty"`
	Commands []string `json:"commands,omitempty"`
	Systemd  bool     `json:"systemd,omitempty"`

	unknownFields bool
}

// UnmarshalJSON records unrecognized fields for strict validation and keeps
// ordinary manifest parsing compatible with additional fields.
func (r *Requirements) UnmarshalJSON(data []byte) error {
	type fields Requirements
	var decoded fields
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	delete(raw, "os")
	delete(raw, "commands")
	delete(raw, "systemd")
	*r = Requirements(decoded)
	r.unknownFields = len(raw) > 0
	return nil
}

// Locale provides translated plugin and configuration text.
type Locale struct {
	Name        string                  `json:"name,omitempty"`
	Description string                  `json:"description,omitempty"`
	Config      map[string]ConfigLocale `json:"config,omitempty"`
}

// ConfigLocale provides translated text for a configuration field.
type ConfigLocale struct {
	Label       string `json:"label,omitempty"`
	Description string `json:"description,omitempty"`
}

// HostAPI is the inclusive range of Host API versions a plugin supports.
type HostAPI struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

// Entry names the optional entry points, relative to the package root.
type Entry struct {
	Web    string `json:"web,omitempty"`
	Lua    string `json:"lua,omitempty"`
	Native string `json:"native,omitempty"`
}

// Native describes the native executable's platform and optional HTTP surfaces.
type Native struct {
	Platform string   `json:"platform"`
	Exports  []string `json:"exports,omitempty"`
	WebAPI   bool     `json:"webApi,omitempty"`
}

const (
	ConfigFormatInteger           = "integer"
	ConfigFormatWritableDirectory = "writable-directory"
)

// ConfigField describes one configuration value.
type ConfigField struct {
	Type        string          `json:"type"`
	Required    bool            `json:"required,omitempty"`
	Secret      bool            `json:"secret,omitempty"`
	Default     json.RawMessage `json:"default,omitempty"`
	Format      string          `json:"format,omitempty"`
	Min         *float64        `json:"min,omitempty"`
	Max         *float64        `json:"max,omitempty"`
	Label       string          `json:"label,omitempty"`
	Description string          `json:"description,omitempty"`
}

// Parse decodes and validates a manifest.
func Parse(data []byte) (*Manifest, error) {
	m, err := ParseStructure(data)
	if err != nil {
		return nil, err
	}
	if err := m.checkHostAPI(); err != nil {
		return nil, err
	}
	return m, nil
}

// ParseStructure validates a manifest independently of the current host API.
func ParseStructure(data []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("manifest.json: %w", err)
	}
	if err := m.validateStructure(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate checks the manifest against the host's supported schema and API.
func (m *Manifest) Validate() error {
	if err := m.validateStructure(); err != nil {
		return err
	}
	return m.checkHostAPI()
}

func (m *Manifest) validateStructure() error {
	if m.SchemaVersion != SchemaVersion {
		return fmt.Errorf("manifest.json: unsupported schemaVersion %d (host supports %d)", m.SchemaVersion, SchemaVersion)
	}
	if !ValidID(m.ID) {
		return fmt.Errorf("manifest.json: invalid id %q (use dot-separated lowercase segments)", m.ID)
	}
	if strings.TrimSpace(m.Name) == "" {
		return fmt.Errorf("manifest.json: name is required")
	}
	if !ValidPackageVersion(m.Version) {
		return fmt.Errorf("manifest.json: invalid version %q (want semantic version, e.g. 1.2.3)", m.Version)
	}
	if m.HostAPI.Min < 1 || m.HostAPI.Max < m.HostAPI.Min {
		return fmt.Errorf("manifest.json: invalid hostApi range %d-%d", m.HostAPI.Min, m.HostAPI.Max)
	}
	if m.Entry.Web == "" && m.Entry.Lua == "" && m.Entry.Native == "" {
		return fmt.Errorf("manifest.json: at least one of entry.web, entry.lua or entry.native is required")
	}
	for name, entry := range map[string]string{"entry.web": m.Entry.Web, "entry.lua": m.Entry.Lua, "entry.native": m.Entry.Native} {
		if entry == "" {
			continue
		}
		if !ValidPackagePath(entry) {
			return fmt.Errorf("manifest.json: %s %q must be a relative path inside the package", name, entry)
		}
	}
	if m.MinHostVersion != "" && !validVersion(m.MinHostVersion) {
		return fmt.Errorf("manifest.json: invalid minHostVersion %q (want semantic version)", m.MinHostVersion)
	}
	if m.Icon != "" {
		if !ValidPackagePath(m.Icon) {
			return fmt.Errorf("manifest.json: icon %q must be a relative path inside the package", m.Icon)
		}
		switch strings.ToLower(path.Ext(m.Icon)) {
		case ".svg", ".png", ".webp":
		default:
			return fmt.Errorf("manifest.json: icon %q must have an .svg, .png or .webp extension", m.Icon)
		}
	}
	for lang, locale := range m.Locales {
		if strings.TrimSpace(lang) == "" {
			return fmt.Errorf("manifest.json: locales contains an empty language code")
		}
		for key := range locale.Config {
			if strings.TrimSpace(key) == "" {
				return fmt.Errorf("manifest.json: locales[%q].config contains an empty field name", lang)
			}
		}
	}
	if err := m.validateNative(); err != nil {
		return err
	}
	for name, field := range m.Config {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("manifest.json: config contains an empty field name")
		}
		if err := field.validate(name); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manifest) validateNative() error {
	if m.Entry.Native == "" {
		if m.Native != nil {
			return fmt.Errorf("manifest.json: native requires entry.native")
		}
		return nil
	}
	if m.Entry.Lua != "" || !strings.HasPrefix(m.Entry.Native, "native/") {
		return fmt.Errorf("manifest.json: entry.native must be inside native/ and cannot be combined with entry.lua")
	}
	if m.Entry.Web != "" && !strings.HasPrefix(m.Entry.Web, "web/") {
		return fmt.Errorf("manifest.json: an entry.web paired with entry.native must be inside web/")
	}
	if m.MinHostVersion == "" {
		return fmt.Errorf("manifest.json: entry.native requires minHostVersion")
	}
	if m.Native == nil || (m.Native.Platform != "linux-amd64" && m.Native.Platform != "linux-arm64") {
		return fmt.Errorf("manifest.json: native.platform must be linux-amd64 or linux-arm64")
	}
	if m.Native.WebAPI && m.Entry.Web == "" {
		return fmt.Errorf("manifest.json: native.webApi requires entry.web")
	}
	seen := make(map[string]bool)
	for _, method := range m.Native.Exports {
		if len(method) > 128 || !methodPattern.MatchString(method) || seen[method] {
			return fmt.Errorf("manifest.json: invalid or duplicate native export %q", method)
		}
		seen[method] = true
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
	switch f.Format {
	case "":
	case ConfigFormatInteger:
		if f.Type != "number" {
			return fmt.Errorf("manifest.json: config field %q: integer format requires a number", name)
		}
	case ConfigFormatWritableDirectory:
		if f.Type != "string" || f.Secret {
			return fmt.Errorf("manifest.json: config field %q: writable-directory format requires a non-secret string", name)
		}
	default:
		return fmt.Errorf("manifest.json: config field %q: unsupported format %q", name, f.Format)
	}
	if (f.Min != nil || f.Max != nil) && f.Type != "number" {
		return fmt.Errorf("manifest.json: config field %q: min and max require a number", name)
	}
	if f.Min != nil && f.Max != nil && *f.Min > *f.Max {
		return fmt.Errorf("manifest.json: config field %q: min exceeds max", name)
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
	return f.NormalizeValue(value)
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
	normalized, err := NormalizeValue(f.Type, value)
	if err != nil {
		return nil, err
	}
	if path, ok := normalized.(string); ok && f.Format == ConfigFormatWritableDirectory && path != "" {
		if !filepath.IsAbs(path) || strings.ContainsRune(path, 0) {
			return nil, fmt.Errorf("目录无法使用：请填写宿主机绝对路径")
		}
		return filepath.Clean(path), nil
	}
	if number, ok := normalized.(float64); ok {
		if f.Format == ConfigFormatInteger && number != math.Trunc(number) {
			return nil, fmt.Errorf("请输入整数")
		}
		if f.Min != nil && number < *f.Min {
			return nil, fmt.Errorf("数值必须大于或等于 %g", *f.Min)
		}
		if f.Max != nil && number > *f.Max {
			return nil, fmt.Errorf("数值必须小于或等于 %g", *f.Max)
		}
	}
	return normalized, nil
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

// ValidID reports whether id uses the plugin ID syntax.
func ValidID(id string) bool {
	return len(id) > 0 && len(id) <= 128 && idPattern.MatchString(id)
}

// ValidPackageVersion reports whether version uses the plugin package version syntax.
func ValidPackageVersion(version string) bool {
	return versionPattern.MatchString(version)
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
