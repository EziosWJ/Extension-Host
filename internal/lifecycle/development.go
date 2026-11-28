package lifecycle

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/EziosWJ/Extension-Host/internal/bundle"
	"github.com/EziosWJ/Extension-Host/internal/logtail"
)

// ReloadDevelopment publishes an updated source snapshot and keeps the plugin's data.
func (m *Manager) ReloadDevelopment(ctx context.Context, pkg *bundle.Bundle, webOnly bool) error {
	if err := checkCompatibility(pkg.Manifest); err != nil {
		return err
	}
	id := pkg.Manifest.ID
	lock := m.lockFor(id)
	lock.Lock()
	defer lock.Unlock()

	current, err := m.state(id)
	if err != nil {
		return err
	}
	m.mu.Lock()
	record := current.record
	previous := current.instance
	m.mu.Unlock()

	config, err := pkg.Manifest.Defaults()
	if err != nil {
		return err
	}
	for name, value := range record.Config {
		if field, ok := pkg.Manifest.Config[name]; ok {
			if normalized, err := field.NormalizeValue(value); err == nil {
				config[name] = normalized
			}
		}
	}

	directory, err := os.MkdirTemp(m.packagesDir, ".dev-")
	if err != nil {
		return fmt.Errorf("create development snapshot: %w", err)
	}
	published := false
	defer func() {
		if !published {
			m.removePackageDirectory(directory)
		}
	}()
	if err := pkg.Extract(directory); err != nil {
		return err
	}

	oldDirectory := m.packageDir(record)
	webOnly = webOnly && previous != nil && bytes.Equal(record.Manifest, pkg.Files["manifest.json"])
	record.PackageDirectory = filepath.Base(directory)
	record.Name = pkg.Manifest.Name
	record.Version = pkg.Manifest.Version
	record.Manifest = pkg.Files["manifest.json"]

	if webOnly {
		if err := m.store.SetDevelopmentPackage(ctx, record); err != nil {
			return err
		}
		m.mu.Lock()
		current.record = record
		m.mu.Unlock()
		published = true
		if oldDirectory != previous.packageDir {
			m.removePackageDirectory(oldDirectory)
		}
		return nil
	}

	record.Config = config
	record.SensitiveKeys = sensitiveConfigKeys(record, pkg.Manifest)
	record.Enabled = true
	next := &state{
		record:   record,
		manifest: pkg.Manifest,
		logPath:  logtail.Path(m.logsDir, id, record.Version),
		status:   StatusStopped,
	}
	restore := func(cause error) error {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if next.instance != nil {
			cause = errors.Join(cause, m.stopLocked(cleanup, next))
		}
		if previous != nil {
			if err := m.startLocked(cleanup, current); err != nil {
				cause = errors.Join(cause, fmt.Errorf("restart last working version: %w", err))
			}
		}
		return cause
	}

	if previous != nil {
		if err := m.stopLocked(ctx, current); err != nil {
			return restore(err)
		}
	}
	if err := m.startLocked(ctx, next); err != nil {
		return restore(err)
	}
	if next.instance == nil {
		return restore(fmt.Errorf("plugin %s exited while starting", id))
	}
	if err := m.store.SetDevelopmentPackage(ctx, record); err != nil {
		return restore(err)
	}
	m.mu.Lock()
	m.states[id] = next
	m.mu.Unlock()
	published = true
	m.removePackageDirectory(oldDirectory)
	return nil
}

func (m *Manager) removePackageDirectory(directory string) {
	if err := os.RemoveAll(directory); err != nil {
		m.logger.Warn("remove package directory", "path", directory, "error", err)
	}
}
