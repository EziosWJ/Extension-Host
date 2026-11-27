package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/EziosWJ/Extension-Host/internal/bundle"
)

// Upgrade replaces an installed package while retaining its configuration and enable intent.
func (m *Manager) Upgrade(ctx context.Context, id string, pkg *bundle.Bundle) (View, error) {
	lock := m.lockFor(id)
	lock.Lock()
	defer lock.Unlock()

	current, err := m.state(id)
	if err != nil {
		return View{}, err
	}
	if pkg.Manifest.ID != id {
		return View{}, fmt.Errorf("%w: got %q, expected %q", ErrIDMismatch, pkg.Manifest.ID, id)
	}
	if err := checkCompatibility(pkg.Manifest); err != nil {
		return View{}, err
	}

	directory, err := os.MkdirTemp(m.packagesDir, ".upgrade-")
	if err != nil {
		return View{}, fmt.Errorf("create upgrade package directory: %w", err)
	}
	published := false
	defer func() {
		if !published {
			m.removePackageDirectory(directory)
		}
	}()
	if err := pkg.Extract(directory); err != nil {
		return View{}, err
	}
	if err := ctx.Err(); err != nil {
		return View{}, err
	}

	// The per-plugin lock protects the current record and instance from lifecycle changes.
	record := current.record
	oldDirectory := m.packageDir(record)
	record.PackageDirectory = filepath.Base(directory)
	record.Name = pkg.Manifest.Name
	record.Version = pkg.Manifest.Version
	record.Manifest = pkg.Files["manifest.json"]
	wasRunning := current.instance != nil
	restore := func(cause error) error {
		if !wasRunning {
			return cause
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := m.startLocked(cleanup, current); err != nil {
			return errors.Join(cause, fmt.Errorf("restart previous package: %w", err))
		}
		return cause
	}
	if wasRunning {
		if err := m.stopLocked(ctx, current); err != nil {
			return View{}, restore(err)
		}
	}
	// Publish the staged directory by committing its reference and metadata together.
	if err := m.store.SetPackage(ctx, record); err != nil {
		return View{}, restore(fmt.Errorf("publish upgrade: %w", err))
	}

	next := &state{
		record:   record,
		manifest: pkg.Manifest,
		logPath:  m.logPath(id, record.Version),
		status:   StatusStopped,
	}
	m.mu.Lock()
	m.states[id] = next
	m.mu.Unlock()
	published = true
	m.removePackageDirectory(oldDirectory)

	if record.Enabled {
		// startLocked records a startup failure in the view and leaves the enable intent set.
		_ = m.startLocked(ctx, next)
	}
	view, _ := m.Get(id)
	m.logger.Info("plugin upgraded", "plugin", id, "version", record.Version)
	return view, nil
}
