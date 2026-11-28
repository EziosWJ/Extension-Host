package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/EziosWJ/Extension-Host/internal/logtail"
)

func showLogs(ctx context.Context, args []string, workDir string, out io.Writer) error {
	var dataDir string
	linesValue, followValue := "100", "false"
	args = slices.Clone(args)
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-f", "--f", "-follow", "--follow":
			args[i] = "--follow=true"
		case "-n", "--n", "-lines", "--lines", "-data-dir", "--data-dir":
			i++ // Leave a value consumed by parseFlags intact.
		}
	}
	positional, err := parseFlags(args, []stringFlag{
		{names: []string{"data-dir"}, value: &dataDir},
		{names: []string{"n", "lines"}, value: &linesValue},
		{names: []string{"f", "follow"}, value: &followValue},
	})
	if err != nil {
		return err
	}
	if len(positional) > 1 {
		return errors.New("logs accepts at most one plugin-id")
	}
	lines, err := strconv.Atoi(linesValue)
	if err != nil || lines < 0 {
		return fmt.Errorf("invalid --lines %q: expected a positive integer or 0 for all lines", linesValue)
	}
	follow, err := strconv.ParseBool(followValue)
	if err != nil {
		return fmt.Errorf("invalid --follow %q: expected true or false", followValue)
	}

	manifestPath := filepath.Join(workDir, "manifest.json")
	if dataDir == "" {
		dataDir = "ext-data"
		if manifestInfo, err := os.Stat(manifestPath); err == nil && manifestInfo.Mode().IsRegular() {
			if logsInfo, err := os.Stat(filepath.Join(workDir, ".ext", "logs")); err == nil && logsInfo.IsDir() {
				dataDir = ".ext"
			}
		}
	}
	if !filepath.IsAbs(dataDir) {
		dataDir = filepath.Join(workDir, dataDir)
	}

	var pluginID string
	if len(positional) == 1 {
		pluginID = positional[0]
		if pluginID == "" {
			return errors.New("plugin-id must not be empty")
		}
	} else {
		pluginID, err = logManifestID(manifestPath)
		if err != nil {
			return err
		}
	}
	logDir := filepath.Join(dataDir, "logs")
	path, err := selectLog(logDir, pluginID)
	if err != nil {
		return err
	}

	var cursor logCursor
	if err := cursor.write(path, lines, out); err != nil || !follow {
		return err
	}
	var nestedPath string
	if filepath.Dir(path) == logDir {
		id, version := logtail.LegacyIdentity(filepath.Base(path))
		nestedPath = logtail.Path(logDir, id, version)
	}
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			next := path
			if nestedPath != "" {
				info, err := os.Lstat(nestedPath)
				switch {
				case err == nil && info.Mode().IsRegular():
					next = nestedPath
				case err != nil && !errors.Is(err, os.ErrNotExist):
					return fmt.Errorf("inspect nested plugin log: %w", err)
				}
			}
			// Drain the legacy file before reading the replacement.
			if err := cursor.write(path, lines, out); err != nil {
				return err
			}
			if next == path {
				continue
			}
			if err := cursor.write(next, lines, out); err != nil {
				return err
			}
			if cursor.path == next {
				path, nestedPath = next, ""
			}
		}
	}
}

func logManifestID(path string) (string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read manifest.json: %w (or specify plugin-id)", err)
	}
	var metadata struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		return "", fmt.Errorf("read manifest.json: %w (or specify plugin-id)", err)
	}
	if metadata.ID == "" {
		return "", errors.New("manifest.json has no id; specify plugin-id")
	}
	return metadata.ID, nil
}

func selectLog(dir, pluginID string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("list plugin logs: %w", err)
	}
	type logFile struct {
		path  string
		entry os.DirEntry
	}
	files := make(map[string]logFile)
	for _, entry := range entries {
		name := entry.Name()
		if entry.Type().IsRegular() {
			id, version := logtail.LegacyIdentity(name)
			if path := logtail.Path(dir, id, version); path != "" && (pluginID == "" || id == pluginID) {
				files[path] = logFile{filepath.Join(dir, name), entry}
			}
		}
	}
	packageDir := logtail.PackageDir(dir)
	pluginDirs, err := os.ReadDir(packageDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("list plugin log directories: %w", err)
	}
	for _, entry := range pluginDirs {
		id := entry.Name()
		if !entry.IsDir() || (pluginID != "" && id != pluginID) {
			continue
		}
		versionFiles, err := os.ReadDir(filepath.Join(packageDir, id))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("list logs for plugin %q: %w", id, err)
		}
		for _, versionFile := range versionFiles {
			version, ok := strings.CutSuffix(versionFile.Name(), ".log")
			if path := logtail.Path(dir, id, version); ok && path != "" && versionFile.Type().IsRegular() {
				files[path] = logFile{path, versionFile}
			}
		}
	}
	var latest os.FileInfo
	var latestPath string
	for _, file := range files {
		info, err := file.entry.Info()
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("inspect plugin log: %w", err)
		}
		if latest != nil && pluginID == "" {
			return "", fmt.Errorf("multiple log files in %s; specify plugin-id", dir)
		}
		if latest == nil || info.ModTime().After(latest.ModTime()) ||
			(info.ModTime().Equal(latest.ModTime()) && file.path < latestPath) {
			latest = info
			latestPath = file.path
		}
	}
	if latest != nil {
		return latestPath, nil
	}
	if pluginID != "" {
		return "", fmt.Errorf("no log files found for plugin %q in %s", pluginID, dir)
	}
	return "", fmt.Errorf("no log files found in %s", dir)
}

type logCursor struct {
	path   string
	info   os.FileInfo
	suffix string
}

func (cursor *logCursor) write(path string, lines int, out io.Writer) error {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) && cursor.info != nil {
		return nil // The active path can temporarily disappear during rotation.
	}
	if err != nil {
		return fmt.Errorf("open plugin log: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat plugin log: %w", err)
	}
	size := info.Size()
	var start int64
	switch {
	case cursor.info == nil:
		start, err = logtail.Start(file, size, lines)
	case path == cursor.path && os.SameFile(cursor.info, info) && size >= cursor.info.Size():
		// Check bytes before the old end in case the file was truncated and
		// grew back past that offset between polls.
		var previous string
		previous, err = logSuffix(file, cursor.info.Size())
		if previous == cursor.suffix {
			start = cursor.info.Size()
			if err == nil && start == size {
				return nil
			}
		}
	}
	if err != nil {
		return fmt.Errorf("read plugin log: %w", err)
	}

	suffix, err := logSuffix(file, size)
	if err != nil {
		return fmt.Errorf("read plugin log: %w", err)
	}
	if _, err := io.Copy(out, io.NewSectionReader(file, start, size-start)); err != nil {
		return fmt.Errorf("copy plugin log: %w", err)
	}
	cursor.path, cursor.info, cursor.suffix = path, info, suffix
	return nil
}

func logSuffix(file *os.File, end int64) (string, error) {
	var buffer [64]byte
	size := min(end, int64(len(buffer)))
	n, err := file.ReadAt(buffer[:size], end-size)
	if errors.Is(err, io.EOF) {
		err = nil // A concurrent truncation can shorten the sampled range.
	}
	return string(buffer[:n]), err
}
