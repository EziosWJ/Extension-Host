package buildinfo

import "testing"

func TestFormat(t *testing.T) {
	original := struct{ version, commit, date string }{Version, Commit, Date}
	t.Cleanup(func() {
		Version, Commit, Date = original.version, original.commit, original.date
	})
	for _, tc := range []struct {
		name                  string
		version, commit, date string
		want                  string
	}{
		{"version only", "v0.1.0", "", "", "v0.1.0"},
		{"full", "v0.1.0", "abc1234", "2026-11-28T09:30:00Z", "v0.1.0 (abc1234, 2026-11-28T09:30:00Z)"},
		{"commit only", "v0.1.0", "abc1234", "", "v0.1.0 (abc1234)"},
		{"date only", "v0.1.0", "", "2026-11-28T09:30:00Z", "v0.1.0 (2026-11-28T09:30:00Z)"},
		{"blank fields", " v0.1.0 ", "  ", " ", "v0.1.0"},
		{"empty version", "", "abc1234", "", "dev (abc1234)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			Version, Commit, Date = tc.version, tc.commit, tc.date
			if got := Format(); got != tc.want {
				t.Fatalf("Format() = %q, want %q", got, tc.want)
			}
		})
	}
}
