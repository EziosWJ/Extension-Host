package manifest

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

const kebabSegment = `[a-z0-9]+(?:-[a-z0-9]+)*`

var (
	strictIDPattern  = regexp.MustCompile(`^` + kebabSegment + `(?:\.` + kebabSegment + `){2,}$`)
	keywordPattern   = regexp.MustCompile(`^` + kebabSegment + `$`)
	configKeyPattern = regexp.MustCompile(`^[a-z][a-zA-Z0-9]*$`)
)

// ValidStrictID reports whether id uses the marketplace syntax and namespace.
func ValidStrictID(id string) bool {
	return ValidID(id) && strictIDPattern.MatchString(id) &&
		!strings.HasPrefix(id, "com.example.") && !strings.HasPrefix(id, "com.godesk.")
}

// ValidateStrict checks a manifest for marketplace publication.
func (m *Manifest) ValidateStrict() error {
	if err := m.Validate(); err != nil {
		return err
	}
	if !ValidStrictID(m.ID) {
		return fmt.Errorf("manifest.json: id %q must use a publishable reverse-DNS name with at least three lowercase kebab-case segments", m.ID)
	}
	if strings.TrimSpace(m.Description) == "" || strings.ContainsAny(m.Description, "\r\n") || utf8.RuneCountInString(m.Description) > 160 {
		return fmt.Errorf("manifest.json: description must be a non-empty single line of at most 160 characters")
	}
	if m.Author == nil || strings.TrimSpace(m.Author.Name) == "" {
		return fmt.Errorf("manifest.json: author.name is required")
	}
	if strings.TrimSpace(m.License) == "" {
		return fmt.Errorf("manifest.json: license is required")
	}
	for _, field := range []struct{ name, value string }{
		{"homepage", m.Homepage}, {"repository", m.Repository}, {"author.url", m.Author.URL},
	} {
		if field.value == "" {
			continue
		}
		u, err := url.Parse(field.value)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" {
			return fmt.Errorf("manifest.json: %s must be a valid HTTPS URL with a host", field.name)
		}
	}
	categories := []string{"database", "proxy", "container", "ops", "monitoring", "networking", "security", "utility"}
	if err := validateList("categories", m.Categories, 3, func(value string) bool { return slices.Contains(categories, value) }); err != nil {
		return err
	}
	if err := validateList("keywords", m.Keywords, 8, keywordPattern.MatchString); err != nil {
		return err
	}
	if requirements := m.Requirements; requirements != nil {
		if requirements.unknownFields {
			return fmt.Errorf("manifest.json: requirements only supports os, commands and systemd")
		}
		for _, field := range []struct {
			name   string
			values []string
		}{
			{"requirements.os", requirements.OS},
			{"requirements.commands", requirements.Commands},
		} {
			for _, value := range field.values {
				if strings.TrimSpace(value) == "" {
					return fmt.Errorf("manifest.json: %s must not contain blank entries", field.name)
				}
			}
		}
	}
	for key := range m.Config {
		if !configKeyPattern.MatchString(key) {
			return fmt.Errorf("manifest.json: config key %q must use English camelCase", key)
		}
	}
	for lang, locale := range m.Locales {
		for key := range locale.Config {
			if _, exists := m.Config[key]; !exists {
				return fmt.Errorf("manifest.json: locales[%q].config references unknown field %q", lang, key)
			}
		}
	}
	return nil
}

func validateList(name string, values []string, limit int, valid func(string) bool) error {
	if len(values) > limit {
		return fmt.Errorf("manifest.json: %s supports at most %d entries", name, limit)
	}
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if !valid(value) || seen[value] {
			return fmt.Errorf("manifest.json: invalid or duplicate %s entry %q", name, value)
		}
		seen[value] = true
	}
	return nil
}
