package logtail

import (
	"path/filepath"
	"strings"

	"github.com/EziosWJ/Extension-Host/internal/manifest"
)

// PackageDir returns the directory reserved for logs with separate coordinates.
func PackageDir(dir string) string {
	return filepath.Join(dir, "by-plugin")
}

// Path returns the log path for validated package coordinates, or an empty path.
func Path(dir, id, version string) string {
	if !manifest.ValidID(id) || !filepath.IsLocal(id) || !manifest.ValidPackageVersion(version) {
		return ""
	}
	return filepath.Join(PackageDir(dir), id, version+".log")
}

// LegacyIdentity identifies a flat log filename only when it has one valid split.
// A second split could belong to a previously installed package.
func LegacyIdentity(name string) (id, version string) {
	stem, ok := strings.CutSuffix(name, ".log")
	if !ok {
		return "", ""
	}
	for i := 1; i < len(stem) && i <= 128; i++ {
		if stem[i] != '-' || !manifest.ValidID(stem[:i]) || !manifest.ValidPackageVersion(stem[i+1:]) {
			continue
		}
		if id != "" {
			return "", ""
		}
		id, version = stem[:i], stem[i+1:]
	}
	return id, version
}
