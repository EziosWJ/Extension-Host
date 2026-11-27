// Package lifecycle owns installed plugins, enable intent and running instances.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/EziosWJ/Extension-Host/internal/bundle"
	"github.com/EziosWJ/Extension-Host/internal/lua"
	"github.com/EziosWJ/Extension-Host/internal/manifest"
	"github.com/EziosWJ/Extension-Host/internal/native"
	"github.com/EziosWJ/Extension-Host/internal/serviceaccess"
	"github.com/EziosWJ/Extension-Host/internal/store"
)

// Status is the current instance state of a plugin.
type Status string

const (
	StatusStopped  Status = "stopped"
	StatusStarting Status = "starting"
	StatusRunning  Status = "running"
	StatusStopping Status = "stopping"
	StatusFailed   Status = "failed"
)

// View is the management-facing snapshot of one plugin.
type View struct {
	ID           string                          `json:"id"`
	Name         string                          `json:"name"`
	Version      string                          `json:"version"`
	Enabled      bool                            `json:"enabled"`
	Status       Status                          `json:"status"`
	Error        string                          `json:"error,omitempty"`
	HasWeb       bool                            `json:"hasWeb"`
	HasNative    bool                            `json:"hasNative"`
	HasLua       bool                            `json:"hasLua"`
	WebPath      string                          `json:"webPath,omitempty"`
	APIPath      string                          `json:"apiPath,omitempty"`
	EventsPath   string                          `json:"eventsPath,omitempty"`
	Config       map[string]any                  `json:"config"`
	ConfigSchema map[string]manifest.ConfigField `json:"configSchema,omitempty"`
	InstalledAt  time.Time                       `json:"installedAt"`
	StartedAt    *time.Time                      `json:"startedAt,omitempty"`
	LogPath      string                          `json:"logPath"`
}

// Options configures a Manager.
type Options struct {
	DataDir  string
	Store    *store.Store
	Logger   *slog.Logger
	Services *serviceaccess.Access
}

// Manager manages plugin installations and instances.
type Manager struct {
	dataDir     string
	packagesDir string
	pluginsDir  string
	logsDir     string
	store       *store.Store
	logger      *slog.Logger
	services    *serviceaccess.Access
	native      *native.Server

	mu     sync.Mutex
	states map[string]*state
	locks  map[string]*sync.Mutex
	closed bool
}

type state struct {
	record    store.Plugin
	manifest  *manifest.Manifest
	logPath   string
	status    Status
	lastError string
	instance  *Instance
	startedAt time.Time
}

// New loads installation records and prepares the manager.
func New(ctx context.Context, opts Options) (*Manager, error) {
	if opts.DataDir == "" {
		return nil, errors.New("data directory is required")
	}
	if opts.Store == nil {
		return nil, errors.New("store is required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	manager := &Manager{
		dataDir:     opts.DataDir,
		packagesDir: filepath.Join(opts.DataDir, "packages"),
		pluginsDir:  filepath.Join(opts.DataDir, "plugins"),
		logsDir:     filepath.Join(opts.DataDir, "logs"),
		store:       opts.Store,
		logger:      opts.Logger,
		services:    opts.Services,
		states:      make(map[string]*state),
		locks:       make(map[string]*sync.Mutex),
	}
	manager.native = native.New(manager.logger, manager.nativeInstance, manager.services)
	for _, dir := range []string{manager.dataDir, manager.packagesDir, manager.pluginsDir, manager.logsDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create host data directory: %w", err)
		}
	}
	records, err := manager.store.ListPlugins(ctx)
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		state := &state{record: record, status: StatusStopped}
		state.logPath = manager.logPath(record.ID, record.Version)
		parsed, err := manifest.Parse(record.Manifest)
		if err == nil {
			err = parsed.CheckCompatibility()
		}
		if err != nil {
			state.status = StatusFailed
			state.lastError = "stored manifest cannot run: " + err.Error()
			manager.logger.Error("invalid installed plugin manifest", "plugin", record.ID, "error", err)
		} else {
			state.manifest = parsed
		}
		manager.states[record.ID] = state
	}
	return manager, nil
}

// List returns all installed plugins ordered by name.
func (m *Manager) List() []View {
	m.mu.Lock()
	defer m.mu.Unlock()
	views := make([]View, 0, len(m.states))
	for _, state := range m.states {
		views = append(views, m.viewLocked(state))
	}
	sort.Slice(views, func(a, b int) bool {
		if views[a].Name == views[b].Name {
			return views[a].ID < views[b].ID
		}
		return views[a].Name < views[b].Name
	})
	return views
}

// Get returns one plugin view.
func (m *Manager) Get(id string) (View, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.states[id]
	if state == nil {
		return View{}, false
	}
	return m.viewLocked(state), true
}

// LogPath returns the active package version's log path.
func (m *Manager) LogPath(id string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if state := m.states[id]; state != nil {
		return state.logPath, true
	}
	return "", false
}

// Install publishes a validated package and records it as disabled.
func (m *Manager) Install(ctx context.Context, pkg *bundle.Bundle) (View, error) {
	id := pkg.Manifest.ID
	lock := m.lockFor(id)
	lock.Lock()
	defer lock.Unlock()

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return View{}, ErrClosed
	}
	if _, exists := m.states[id]; exists {
		m.mu.Unlock()
		return View{}, fmt.Errorf("%w: %s", ErrConflict, id)
	}
	m.mu.Unlock()

	if err := checkCompatibility(pkg.Manifest); err != nil {
		return View{}, err
	}
	defaults, err := pkg.Manifest.Defaults()
	if err != nil {
		return View{}, err
	}
	target := filepath.Join(m.packagesDir, id)
	if _, err := os.Stat(target); err == nil {
		// Remove a package directory left behind by an interrupted install.
		if err := os.RemoveAll(target); err != nil {
			return View{}, fmt.Errorf("remove stale package directory: %w", err)
		}
	}
	temporary, err := os.MkdirTemp(m.packagesDir, ".install-")
	if err != nil {
		return View{}, fmt.Errorf("create package directory: %w", err)
	}
	defer os.RemoveAll(temporary)
	if err := pkg.Extract(temporary); err != nil {
		return View{}, err
	}
	if err := os.Rename(temporary, target); err != nil {
		return View{}, fmt.Errorf("publish package: %w", err)
	}

	record := store.Plugin{
		ID:          id,
		Name:        pkg.Manifest.Name,
		Version:     pkg.Manifest.Version,
		Manifest:    pkg.Files["manifest.json"],
		Config:      defaults,
		Enabled:     false,
		InstalledAt: time.Now().UTC(),
	}
	if err := m.store.InsertPlugin(ctx, record); err != nil {
		_ = os.RemoveAll(target)
		return View{}, err
	}

	state := &state{
		record:   record,
		manifest: pkg.Manifest,
		logPath:  m.logPath(record.ID, record.Version),
		status:   StatusStopped,
	}
	m.mu.Lock()
	m.states[id] = state
	view := m.viewLocked(state)
	m.mu.Unlock()
	m.logger.Info("plugin installed", "plugin", id, "version", record.Version)
	return view, nil
}

// Enable persists the enable intent and starts the plugin.
func (m *Manager) Enable(ctx context.Context, id string) error {
	lock := m.lockFor(id)
	lock.Lock()
	defer lock.Unlock()

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrClosed
	}
	state := m.states[id]
	if state == nil {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	enabled := state.record.Enabled
	m.mu.Unlock()

	if !enabled {
		if err := m.store.SetEnabled(ctx, id, true); err != nil {
			return err
		}
		m.mu.Lock()
		state.record.Enabled = true
		m.mu.Unlock()
	}
	return m.startLocked(ctx, state)
}

// Start starts the instance of an enabled plugin.
func (m *Manager) Start(ctx context.Context, id string) error {
	lock := m.lockFor(id)
	lock.Lock()
	defer lock.Unlock()

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrClosed
	}
	state := m.states[id]
	if state == nil {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if !state.record.Enabled {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrNotEnabled, id)
	}
	m.mu.Unlock()
	return m.startLocked(ctx, state)
}

// Stop stops the running instance and keeps the enable intent.
func (m *Manager) Stop(ctx context.Context, id string) error {
	lock := m.lockFor(id)
	lock.Lock()
	defer lock.Unlock()

	state, err := m.state(id)
	if err != nil {
		return err
	}
	return m.stopLocked(ctx, state)
}

// Disable clears the enable intent and stops the instance.
func (m *Manager) Disable(ctx context.Context, id string) error {
	lock := m.lockFor(id)
	lock.Lock()
	defer lock.Unlock()

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrClosed
	}
	state := m.states[id]
	if state == nil {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	enabled := state.record.Enabled
	m.mu.Unlock()

	if enabled {
		if err := m.store.SetEnabled(ctx, id, false); err != nil {
			return err
		}
		m.mu.Lock()
		state.record.Enabled = false
		m.mu.Unlock()
	}
	return m.stopLocked(ctx, state)
}

// Uninstall stops and removes a plugin. Business data is kept unless purgeData is true.
func (m *Manager) Uninstall(ctx context.Context, id string, purgeData bool) error {
	lock := m.lockFor(id)
	lock.Lock()
	defer lock.Unlock()

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrClosed
	}
	state := m.states[id]
	m.mu.Unlock()
	if state == nil {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if state.record.Enabled {
		if err := m.store.SetEnabled(ctx, id, false); err != nil {
			return err
		}
		m.mu.Lock()
		state.record.Enabled = false
		m.mu.Unlock()
	}
	if err := m.stopLocked(ctx, state); err != nil {
		m.logger.Error("plugin stop failed during uninstall", "plugin", id, "error", err)
	}
	if err := m.store.DeletePlugin(ctx, id); err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.states, id)
	m.mu.Unlock()

	var errs []error
	packageDir := m.packageDir(state.record)
	if err := os.RemoveAll(packageDir); err != nil {
		errs = append(errs, fmt.Errorf("remove plugin package: %w", err))
	}
	if purgeData {
		if err := os.RemoveAll(filepath.Join(m.pluginsDir, id)); err != nil {
			errs = append(errs, fmt.Errorf("remove plugin data: %w", err))
		}
	}
	m.logger.Info("plugin uninstalled", "plugin", id, "purged", purgeData)
	return errors.Join(errs...)
}

// Restore starts every plugin with a persisted enable intent.
func (m *Manager) Restore(ctx context.Context) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrClosed
	}
	type restoreItem struct {
		id    string
		state *state
	}
	var items []restoreItem
	for id, state := range m.states {
		if state.record.Enabled {
			items = append(items, restoreItem{id: id, state: state})
		}
	}
	m.mu.Unlock()
	sort.Slice(items, func(a, b int) bool { return items[a].id < items[b].id })

	var errs []error
	for _, item := range items {
		lock := m.lockFor(item.id)
		lock.Lock()
		m.mu.Lock()
		current := m.states[item.id]
		m.mu.Unlock()
		if current == nil {
			lock.Unlock()
			continue
		}
		if err := m.startLocked(ctx, current); err != nil {
			errs = append(errs, err)
		}
		lock.Unlock()
	}
	return errors.Join(errs...)
}

// Close stops all instances and rejects further lifecycle operations.
func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	ids := make([]string, 0, len(m.states))
	for id := range m.states {
		ids = append(ids, id)
	}
	m.mu.Unlock()

	var errs []error
	for _, id := range ids {
		lock := m.lockFor(id)
		lock.Lock()
		m.mu.Lock()
		state := m.states[id]
		m.mu.Unlock()
		if state != nil {
			if err := m.stopLocked(ctx, state); err != nil {
				errs = append(errs, fmt.Errorf("stop plugin %s: %w", id, err))
			}
		}
		lock.Unlock()
	}
	return errors.Join(append(errs, m.native.Close())...)
}

// UpdateConfig validates and merges configuration values.
func (m *Manager) UpdateConfig(ctx context.Context, id string, values map[string]any) (View, error) {
	lock := m.lockFor(id)
	lock.Lock()
	defer lock.Unlock()

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return View{}, ErrClosed
	}
	state := m.states[id]
	m.mu.Unlock()
	if state == nil {
		return View{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if state.manifest == nil {
		return View{}, fmt.Errorf("plugin %s has an invalid manifest", id)
	}
	m.mu.Lock()
	current := copyMap(state.record.Config)
	m.mu.Unlock()
	merged, err := state.manifest.MergeConfig(current, values)
	if err != nil {
		return View{}, err
	}
	for name := range values {
		path, _ := merged[name].(string)
		if state.manifest.Config[name].Format == manifest.ConfigFormatWritableDirectory && path != "" {
			if err := verifyWritableDirectory(path); err != nil {
				return View{}, fmt.Errorf("%s：目录无法使用：%w", name, err)
			}
		}
	}
	if err := m.store.SetConfig(ctx, id, merged); err != nil {
		return View{}, err
	}
	m.mu.Lock()
	state.record.Config = merged
	view := m.viewLocked(state)
	m.mu.Unlock()
	return view, nil
}

func verifyWritableDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(path, ".extension-host-write-*")
	if err != nil {
		return err
	}
	_, writeErr := file.WriteString("write test")
	return errors.Join(writeErr, file.Close(), os.Remove(file.Name()))
}

// RunningInstance returns the live instance for a plugin.
func (m *Manager) RunningInstance(id string) (*Instance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.states[id]
	if state == nil {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if state.instance == nil {
		return nil, fmt.Errorf("%w: %s", ErrNotRunning, id)
	}
	return state.instance, nil
}

func (m *Manager) nativeInstance(id string) *native.Process {
	instance, err := m.RunningInstance(id)
	if err != nil {
		return nil
	}
	return instance.Native()
}

func checkCompatibility(metadata *manifest.Manifest) error {
	if err := metadata.CheckCompatibility(); err != nil {
		return fmt.Errorf("%w: %v", ErrIncompatible, err)
	}
	return nil
}

// WebInfo returns the directory and index file for a plugin web entry.
func (m *Manager) WebInfo(id string) (string, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.states[id]
	if state == nil {
		return "", "", fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if state.instance == nil {
		return "", "", fmt.Errorf("%w: %s", ErrNotRunning, id)
	}
	entry := state.manifest.Entry.Web
	if entry == "" {
		return "", "", fmt.Errorf("%w: %s", ErrNoWeb, id)
	}
	return filepath.Join(m.packageDir(state.record), filepath.FromSlash(filepath.Dir(entry))), filepath.Base(entry), nil
}

func (m *Manager) state(id string) (*state, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrClosed
	}
	state := m.states[id]
	if state == nil {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return state, nil
}

func (m *Manager) startLocked(ctx context.Context, state *state) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrClosed
	}
	if state.instance != nil {
		m.mu.Unlock()
		return nil
	}
	if state.manifest == nil {
		message := state.lastError
		if message == "" {
			message = "plugin manifest is invalid"
		}
		state.status = StatusFailed
		m.mu.Unlock()
		return errors.New(message)
	}
	state.status = StatusStarting
	state.lastError = ""
	record := state.record
	parsed := state.manifest
	logPath := state.logPath
	m.mu.Unlock()

	instance := newInstance(instanceOptions{
		id:         record.ID,
		version:    record.Version,
		packageDir: m.packageDir(record),
		manifest:   parsed,
		pluginDir:  filepath.Join(m.pluginsDir, record.ID),
		logPath:    logPath,
		config:     m.configGetter(state),
		services:   m.services,
		nativeHost: m.native,
	})
	if err := instance.Start(ctx); err != nil {
		m.mu.Lock()
		if errors.Is(err, lua.ErrExited) {
			state.status = StatusStopped
			m.mu.Unlock()
			return nil
		}
		state.status = StatusFailed
		state.lastError = err.Error()
		m.mu.Unlock()
		m.logger.Error("plugin start failed", "plugin", record.ID, "version", record.Version, "error", err)
		return fmt.Errorf("start plugin %s: %w", record.ID, err)
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		_ = instance.Stop(ctx)
		return ErrClosed
	}
	state.instance = instance
	state.status = StatusRunning
	state.startedAt = time.Now().UTC()
	m.mu.Unlock()
	m.logger.Info("plugin started", "plugin", record.ID, "version", record.Version)
	if done := instance.runtimeDone(); done != nil {
		go m.watchRuntime(record.ID, state, instance, done)
	}
	return nil
}

func (m *Manager) watchRuntime(id string, state *state, instance *Instance, done <-chan struct{}) {
	<-done
	lock := m.lockFor(id)
	lock.Lock()
	defer lock.Unlock()

	m.mu.Lock()
	current := state.instance == instance
	m.mu.Unlock()
	if !current {
		return
	}
	_ = m.stopLocked(context.Background(), state)
	if process := instance.Native(); process != nil {
		err := process.ExitError()
		m.mu.Lock()
		state.status = StatusFailed
		state.lastError = err.Error()
		m.mu.Unlock()
		m.logger.Error("native plugin exited", "plugin", id, "error", err)
	}
}

func (m *Manager) stopLocked(ctx context.Context, state *state) error {
	m.mu.Lock()
	instance := state.instance
	if instance == nil {
		state.status = StatusStopped
		state.lastError = ""
		state.startedAt = time.Time{}
		m.mu.Unlock()
		return nil
	}
	state.instance = nil
	state.status = StatusStopping
	id := state.record.ID
	m.mu.Unlock()

	err := instance.Stop(ctx)
	if instance.packageDir != m.packageDir(state.record) {
		m.removePackageDirectory(instance.packageDir)
	}
	m.mu.Lock()
	state.status = StatusStopped
	state.lastError = ""
	state.startedAt = time.Time{}
	m.mu.Unlock()
	if err != nil {
		m.logger.Error("plugin stop failed", "plugin", id, "error", err)
	}
	return err
}

func (m *Manager) configGetter(state *state) func() map[string]any {
	return func() map[string]any {
		m.mu.Lock()
		defer m.mu.Unlock()
		return copyMap(state.record.Config)
	}
}

func (m *Manager) packageDir(record store.Plugin) string {
	if record.PackageDirectory != "" {
		return filepath.Join(m.packagesDir, record.PackageDirectory)
	}
	return filepath.Join(m.packagesDir, record.ID)
}

func (m *Manager) viewLocked(state *state) View {
	view := View{
		ID:          state.record.ID,
		Name:        state.record.Name,
		Version:     state.record.Version,
		Enabled:     state.record.Enabled,
		Status:      state.status,
		Error:       state.lastError,
		Config:      map[string]any{},
		InstalledAt: state.record.InstalledAt,
		LogPath:     state.logPath,
	}
	if !state.startedAt.IsZero() {
		started := state.startedAt
		view.StartedAt = &started
	}
	if state.manifest != nil {
		view.HasWeb = state.manifest.Entry.Web != ""
		view.HasLua = state.manifest.Entry.Lua != ""
		view.HasNative = state.manifest.Native != nil
		view.ConfigSchema = state.manifest.Config
		for name, field := range state.manifest.Config {
			if value, ok := state.record.Config[name]; ok && !field.Secret {
				view.Config[name] = value
			}
		}
		if view.HasWeb {
			view.WebPath = "/extensions/" + state.record.ID + "/web/"
		}
		if view.HasLua || (view.HasNative && state.manifest.Native.WebAPI) {
			view.APIPath = "/extensions/" + state.record.ID + "/api/"
		}
		if view.HasLua {
			view.EventsPath = "/extensions/" + state.record.ID + "/events"
		}
	}
	return view
}

func (m *Manager) lockFor(id string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	lock := m.locks[id]
	if lock == nil {
		lock = &sync.Mutex{}
		m.locks[id] = lock
	}
	return lock
}

func (m *Manager) logPath(id, version string) string {
	return filepath.Join(m.logsDir, id+"-"+version+".log")
}

func copyMap(values map[string]any) map[string]any {
	result := make(map[string]any, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}
