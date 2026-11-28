// Package buildinfo holds version details injected at build time with
// -ldflags "-X github.com/EziosWJ/Extension-Host/internal/buildinfo.Version=...".
package buildinfo

import "strings"

var (
	Version = "v0.1.0"
	Commit  = ""
	Date    = ""
)

// Format returns the version with the injected commit and build date, for
// example "v0.1.0 (abc1234, 2026-11-28T09:30:00Z)" or just "v0.1.0".
func Format() string {
	version := strings.TrimSpace(Version)
	if version == "" {
		version = "dev"
	}
	parts := make([]string, 0, 2)
	if commit := strings.TrimSpace(Commit); commit != "" {
		parts = append(parts, commit)
	}
	if date := strings.TrimSpace(Date); date != "" {
		parts = append(parts, date)
	}
	if len(parts) == 0 {
		return version
	}
	return version + " (" + strings.Join(parts, ", ") + ")"
}
