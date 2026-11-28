package logtail

import (
	"path/filepath"

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
