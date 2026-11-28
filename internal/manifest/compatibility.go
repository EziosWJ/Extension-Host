package manifest

import (
	"cmp"
	"fmt"
	"runtime"
	"strings"
)

// Version identifies this Extension Host implementation, including source builds.
const Version = "0.3.0"

// CheckCompatibility checks requirements against the actual installation host.
func (m *Manifest) CheckCompatibility() error {
	if err := m.checkHostAPI(); err != nil {
		return err
	}
	if m.MinHostVersion != "" && compareVersions(Version, m.MinHostVersion) < 0 {
		return fmt.Errorf("requires Extension Host >= %s (current: %s)", m.MinHostVersion, Version)
	}
	platform := runtime.GOOS + "-" + runtime.GOARCH
	if m.Native != nil && m.Native.Platform != platform {
		return fmt.Errorf("native plugin targets %s (current: %s)", m.Native.Platform, platform)
	}
	return nil
}

func (m *Manifest) checkHostAPI() error {
	if HostAPIVersion < m.HostAPI.Min || HostAPIVersion > m.HostAPI.Max {
		return fmt.Errorf("manifest.json: %w %d-%d, host provides %d", ErrHostAPIIncompatible, m.HostAPI.Min, m.HostAPI.Max, HostAPIVersion)
	}
	return nil
}

func versionParts(version string) ([]string, string) {
	version, _, _ = strings.Cut(version, "+")
	core, pre, _ := strings.Cut(version, "-")
	return strings.Split(core, "."), pre
}

func validVersion(version string) bool {
	if !versionPattern.MatchString(version) {
		return false
	}
	base, build, hasBuild := strings.Cut(version, "+")
	_, pre, hasPre := strings.Cut(base, "-")
	if hasBuild {
		for _, part := range strings.Split(build, ".") {
			if part == "" {
				return false
			}
		}
	}
	if hasPre {
		for _, part := range strings.Split(pre, ".") {
			if part == "" || (len(part) > 1 && part[0] == '0' && numeric(part)) {
				return false
			}
		}
	}
	return true
}

func numeric(value string) bool {
	return strings.Trim(value, "0123456789") == ""
}

func compareNumeric(a, b string) int {
	if order := cmp.Compare(len(a), len(b)); order != 0 {
		return order
	}
	return cmp.Compare(a, b)
}

func compareVersions(a, b string) int {
	aCore, aPre := versionParts(a)
	bCore, bPre := versionParts(b)
	for i := range aCore {
		if order := compareNumeric(aCore[i], bCore[i]); order != 0 {
			return order
		}
	}
	if aPre == "" || bPre == "" {
		return cmp.Compare(bPre, aPre)
	}
	aIDs, bIDs := strings.Split(aPre, "."), strings.Split(bPre, ".")
	for i := range min(len(aIDs), len(bIDs)) {
		a, b := aIDs[i], bIDs[i]
		switch {
		case a == b:
			continue
		case numeric(a) && numeric(b):
			return compareNumeric(a, b)
		case numeric(a):
			return -1
		case numeric(b):
			return 1
		default:
			return cmp.Compare(a, b)
		}
	}
	return cmp.Compare(len(aIDs), len(bIDs))
}
