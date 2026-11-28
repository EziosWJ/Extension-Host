// Package bundle loads, validates and writes plugin packages (directories and ZIP archives).
package bundle

import (
	"archive/zip"
	"bytes"
	"debug/elf"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/EziosWJ/Extension-Host/internal/manifest"
)

const (
	// MaxPackageSize limits a compressed package and the total uncompressed content.
	MaxPackageSize = 256 << 20
	maxFileSize    = 64 << 20
	maxFileCount   = 4096
	maxManifest    = 1 << 20
)

// Bundle is a validated plugin package held in memory.
type Bundle struct {
	Manifest *manifest.Manifest
	Files    map[string][]byte
}

// LoadDir reads and validates a plugin source directory.
// Absolute paths in exclude are skipped, which keeps a pack destination
// inside the source directory out of the package.
func LoadDir(dir string, exclude ...string) (*Bundle, error) {
	return loadDir(dir, false, exclude...)
}

// LoadProject reads only the runtime files in a standard plugin project.
func LoadProject(dir string) (*Bundle, error) {
	pkg, err := loadDir(dir, true)
	if err != nil {
		return nil, err
	}
	for kind, entry := range map[string]string{"web": pkg.Manifest.Entry.Web, "lua": pkg.Manifest.Entry.Lua} {
		if entry != "" && !strings.HasPrefix(entry, kind+"/") {
			return nil, fmt.Errorf("project: entry.%s must be inside %s/", kind, kind)
		}
	}
	return pkg, nil
}

func loadDir(dir string, project bool, exclude ...string) (*Bundle, error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("package directory: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("package directory: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("package directory %s is not a directory", dir)
	}
	skipped := make(map[string]bool, len(exclude))
	for _, name := range exclude {
		if abs, err := filepath.Abs(name); err == nil {
			skipped[abs] = true
		}
	}

	files := make(map[string][]byte)
	var total int64
	err = filepath.WalkDir(root, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if current == root {
			return nil
		}
		rel, err := filepath.Rel(root, current)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		if project {
			top, _, _ := strings.Cut(name, "/")
			if top != "manifest.json" && top != "web" && top != "lua" && top != "native" {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if name == "manifest.json" && !entry.Type().IsRegular() {
				return fmt.Errorf("project: manifest.json must be a regular file")
			}
			if (name == "web" || name == "lua" || name == "native") && !entry.IsDir() {
				return fmt.Errorf("project: %s must be a directory", name)
			}
		}
		if skipped[current] {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			rel, _ := filepath.Rel(root, current)
			return fmt.Errorf("package contains unsupported symlink %q", filepath.ToSlash(rel))
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			rel, _ := filepath.Rel(root, current)
			return fmt.Errorf("package contains unsupported file %q", filepath.ToSlash(rel))
		}
		if len(files) >= maxFileCount {
			return fmt.Errorf("package contains more than %d files", maxFileCount)
		}
		if !manifest.ValidPackagePath(name) {
			return fmt.Errorf("package contains invalid path %q", name)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Size() > maxFileSize {
			return fmt.Errorf("package file %q exceeds %d bytes", name, maxFileSize)
		}
		total += info.Size()
		if total > MaxPackageSize {
			return fmt.Errorf("package exceeds %d bytes uncompressed", MaxPackageSize)
		}
		data, err := os.ReadFile(current)
		if err != nil {
			return err
		}
		files[name] = data
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read package: %w", err)
	}
	return build(files, manifest.Parse)
}

// LoadZip reads and validates a ZIP package.
func LoadZip(data []byte) (*Bundle, error) {
	return loadZip(data, manifest.Parse)
}

// LoadZipStructure validates a ZIP without checking support for its host API.
func LoadZipStructure(data []byte) (*Bundle, error) {
	return loadZip(data, manifest.ParseStructure)
}

func loadZip(data []byte, parseManifest func([]byte) (*manifest.Manifest, error)) (*Bundle, error) {
	if len(data) > MaxPackageSize {
		return nil, fmt.Errorf("package exceeds %d bytes", MaxPackageSize)
	}
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("open package zip: %w", err)
	}
	files := make(map[string][]byte)
	var total int64
	for _, entry := range reader.File {
		name := entry.Name
		if entry.FileInfo().IsDir() {
			name = trimDirSlash(name)
			if !manifest.ValidPackagePath(name) {
				return nil, fmt.Errorf("package contains invalid path %q", entry.Name)
			}
			continue
		}
		if entry.Mode()&fs.ModeSymlink != 0 || !entry.Mode().IsRegular() {
			return nil, fmt.Errorf("package contains unsupported entry %q", entry.Name)
		}
		if !manifest.ValidPackagePath(name) {
			return nil, fmt.Errorf("package contains invalid path %q", entry.Name)
		}
		if _, exists := files[name]; exists {
			return nil, fmt.Errorf("package contains duplicate path %q", name)
		}
		if len(files) >= maxFileCount {
			return nil, fmt.Errorf("package contains more than %d files", maxFileCount)
		}
		if entry.UncompressedSize64 > maxFileSize {
			return nil, fmt.Errorf("package file %q exceeds %d bytes", name, maxFileSize)
		}
		file, err := entry.Open()
		if err != nil {
			return nil, fmt.Errorf("read package file %q: %w", name, err)
		}
		data, err := io.ReadAll(io.LimitReader(file, maxFileSize+1))
		closeErr := file.Close()
		if err != nil {
			return nil, fmt.Errorf("read package file %q: %w", name, err)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("read package file %q: %w", name, closeErr)
		}
		if len(data) > maxFileSize {
			return nil, fmt.Errorf("package file %q exceeds %d bytes", name, maxFileSize)
		}
		total += int64(len(data))
		if total > MaxPackageSize {
			return nil, fmt.Errorf("package exceeds %d bytes uncompressed", MaxPackageSize)
		}
		files[name] = data
	}
	return build(files, parseManifest)
}

// Zip serializes the bundle into a deterministic ZIP archive.
func (b *Bundle) Zip() ([]byte, error) {
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	names := make([]string, 0, len(b.Files))
	for name := range b.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		header.SetMode(b.fileMode(name))
		header.Modified = time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)
		entry, err := writer.CreateHeader(header)
		if err != nil {
			return nil, fmt.Errorf("write package: %w", err)
		}
		if _, err := entry.Write(b.Files[name]); err != nil {
			return nil, fmt.Errorf("write package: %w", err)
		}
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("write package: %w", err)
	}
	return buffer.Bytes(), nil
}

// Extract writes the package files below dst.
func (b *Bundle) Extract(dst string) error {
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return fmt.Errorf("create package directory: %w", err)
	}
	for name, data := range b.Files {
		target := filepath.Join(dst, filepath.FromSlash(name))
		relative, err := filepath.Rel(dst, target)
		if err != nil || !filepath.IsLocal(relative) {
			return fmt.Errorf("package path %q escapes the package root", name)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("create package directory: %w", err)
		}
		if err := os.WriteFile(target, data, b.fileMode(name)); err != nil {
			return fmt.Errorf("write package file %q: %w", name, err)
		}
	}
	return nil
}

func (b *Bundle) fileMode(name string) fs.FileMode {
	if name == b.Manifest.Entry.Native {
		return 0o755
	}
	return 0o644
}

// Filename is the standard distribution name for the package.
func (b *Bundle) Filename() string {
	name := b.Manifest.ID + "-" + b.Manifest.Version
	if b.Manifest.Native != nil {
		name += "-" + b.Manifest.Native.Platform
	}
	return name + ".zip"
}

func build(files map[string][]byte, parseManifest func([]byte) (*manifest.Manifest, error)) (*Bundle, error) {
	for name := range files {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if _, exists := files[parent]; exists {
				return nil, fmt.Errorf("package path %q is both a file and a directory", parent)
			}
		}
	}
	data, ok := files["manifest.json"]
	if !ok {
		return nil, fmt.Errorf("package: manifest.json is missing from the package root")
	}
	if len(data) > maxManifest {
		return nil, fmt.Errorf("package: manifest.json exceeds %d bytes", maxManifest)
	}
	parsed, err := parseManifest(data)
	if err != nil {
		return nil, fmt.Errorf("package: %w", err)
	}
	for name, entry := range map[string]string{"entry.web": parsed.Entry.Web, "entry.lua": parsed.Entry.Lua, "entry.native": parsed.Entry.Native} {
		if entry == "" {
			continue
		}
		if _, ok := files[entry]; !ok {
			return nil, fmt.Errorf("package: %s %q does not exist", name, entry)
		}
	}
	if parsed.Native != nil {
		if err := validateExecutable(files[parsed.Entry.Native], parsed.Native.Platform); err != nil {
			return nil, fmt.Errorf("package: entry.native %q: %w", parsed.Entry.Native, err)
		}
	}
	return &Bundle{Manifest: parsed, Files: files}, nil
}

func validateExecutable(data []byte, platform string) error {
	executable, err := elf.NewFile(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("invalid ELF executable: %w", err)
	}
	machine := elf.EM_X86_64
	if platform == "linux-arm64" {
		machine = elf.EM_AARCH64
	}
	if executable.Machine != machine || executable.Class != elf.ELFCLASS64 || executable.Data != elf.ELFDATA2LSB {
		return fmt.Errorf("ELF architecture does not match %s", platform)
	}
	if executable.Entry == 0 || (executable.Type != elf.ET_EXEC && executable.Type != elf.ET_DYN) ||
		(executable.OSABI != elf.ELFOSABI_NONE && executable.OSABI != elf.ELFOSABI_LINUX) {
		return fmt.Errorf("expected a Linux executable")
	}
	for _, program := range executable.Progs {
		if program.Type == elf.PT_LOAD && program.Flags&elf.PF_X != 0 &&
			executable.Entry >= program.Vaddr && executable.Entry-program.Vaddr < program.Filesz {
			return nil
		}
	}
	return fmt.Errorf("ELF entry point is outside executable segments")
}

func trimDirSlash(name string) string {
	for len(name) > 0 && name[len(name)-1] == '/' {
		name = name[:len(name)-1]
	}
	return name
}
