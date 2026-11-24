// Package host is the public embedding API of the GoDesk Extension Host.
package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/EziosWJ/Extension-Host/internal/bundle"
	"github.com/EziosWJ/Extension-Host/internal/gateway"
	"github.com/EziosWJ/Extension-Host/internal/lifecycle"
	"github.com/EziosWJ/Extension-Host/internal/manifest"
	"github.com/EziosWJ/Extension-Host/internal/store"
)

// HostAPIVersion is the plugin Host API version implemented by this module.
const HostAPIVersion = manifest.HostAPIVersion

// Package errors.
var (
	ErrNotFound       = lifecycle.ErrNotFound
	ErrConflict       = lifecycle.ErrConflict
	ErrNotEnabled     = lifecycle.ErrNotEnabled
	ErrNotRunning     = lifecycle.ErrNotRunning
	ErrNoLua          = lifecycle.ErrNoLua
	ErrNoWeb          = lifecycle.ErrNoWeb
	ErrClosed         = lifecycle.ErrClosed
	ErrInvalidPackage = errors.New("invalid plugin package")
)

// Status is the current instance state of a plugin.
type Status string

const (
	StatusStopped  Status = Status(lifecycle.StatusStopped)
	StatusStarting Status = Status(lifecycle.StatusStarting)
	StatusRunning  Status = Status(lifecycle.StatusRunning)
	StatusStopping Status = Status(lifecycle.StatusStopping)
	StatusFailed   Status = Status(lifecycle.StatusFailed)
)

// Options configures a Host.
type Options struct {
	// DataDir holds host.db, packages, plugin data and logs.
	DataDir string
	// Logger records host lifecycle events. Defaults to slog.Default.
	Logger *slog.Logger
}

// HTTPOptions configures the extension HTTP handler.
type HTTPOptions struct {
	// RequireSession protects every extension route with the embedder's session middleware.
	RequireSession func(http.Handler) http.Handler
	// AllowedOrigins lists extra WebSocket origins, useful behind a development proxy.
	AllowedOrigins []string
}

// Plugin is the management view of an installed plugin.
type Plugin struct {
	ID           string                 `json:"id"`
	Name         string                 `json:"name"`
	Version      string                 `json:"version"`
	Enabled      bool                   `json:"enabled"`
	Status       Status                 `json:"status"`
	Error        string                 `json:"error,omitempty"`
	HasWeb       bool                   `json:"hasWeb"`
	HasLua       bool                   `json:"hasLua"`
	WebPath      string                 `json:"webPath,omitempty"`
	APIPath      string                 `json:"apiPath,omitempty"`
	EventsPath   string                 `json:"eventsPath,omitempty"`
	Config       map[string]any         `json:"config"`
	ConfigSchema map[string]ConfigField `json:"configSchema,omitempty"`
	InstalledAt  time.Time              `json:"installedAt"`
	StartedAt    *time.Time             `json:"startedAt,omitempty"`
	LogPath      string                 `json:"logPath"`
}

// ConfigField describes one configuration value.
type ConfigField struct {
	Type     string `json:"type"`
	Required bool   `json:"required,omitempty"`
	Secret   bool   `json:"secret,omitempty"`
	Default  any    `json:"default,omitempty"`
}

// UninstallOptions controls uninstall behaviour.
type UninstallOptions struct {
	// PurgeData also deletes the plugin business database and data directory.
	PurgeData bool
}

// Host owns the plugin engine and its storage.
type Host struct {
	manager *lifecycle.Manager
	store   *store.Store
	logger  *slog.Logger

	closeOnce sync.Once
	closeErr  error
}

// Open creates or opens the host data directory.
func Open(ctx context.Context, opts Options) (*Host, error) {
	if opts.DataDir == "" {
		return nil, errors.New("host: data directory is required")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	dataDir, err := filepath.Abs(opts.DataDir)
	if err != nil {
		return nil, fmt.Errorf("host: resolve data directory: %w", err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("host: create data directory: %w", err)
	}
	database, err := store.Open(ctx, filepath.Join(dataDir, "host.db"))
	if err != nil {
		return nil, err
	}
	manager, err := lifecycle.New(ctx, lifecycle.Options{DataDir: dataDir, Store: database, Logger: logger})
	if err != nil {
		_ = database.Close()
		return nil, err
	}
	return &Host{manager: manager, store: database, logger: logger}, nil
}

// Restore starts every plugin that was enabled before the last shutdown.
func (h *Host) Restore(ctx context.Context) error {
	return h.manager.Restore(ctx)
}

// Handler returns the extension HTTP routes.
func (h *Host) Handler(opts HTTPOptions) http.Handler {
	return gateway.New(h.manager, h.logger, gateway.Options{
		RequireSession: opts.RequireSession,
		AllowedOrigins: opts.AllowedOrigins,
	})
}

// List returns all installed plugins.
func (h *Host) List(ctx context.Context) ([]Plugin, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	views := h.manager.List()
	plugins := make([]Plugin, 0, len(views))
	for _, view := range views {
		plugins = append(plugins, pluginFromView(view))
	}
	return plugins, nil
}

// Install publishes and records a ZIP plugin package. The plugin starts disabled.
func (h *Host) Install(ctx context.Context, zip io.Reader) (Plugin, error) {
	data, err := io.ReadAll(io.LimitReader(zip, bundle.MaxPackageSize+1))
	if err != nil {
		return Plugin{}, fmt.Errorf("read plugin package: %w", err)
	}
	if len(data) > bundle.MaxPackageSize {
		return Plugin{}, fmt.Errorf("%w: package exceeds %d bytes", ErrInvalidPackage, bundle.MaxPackageSize)
	}
	pkg, err := bundle.LoadZip(data)
	if err != nil {
		return Plugin{}, fmt.Errorf("%w: %v", ErrInvalidPackage, err)
	}
	view, err := h.manager.Install(ctx, pkg)
	if err != nil {
		return Plugin{}, err
	}
	return pluginFromView(view), nil
}

// Enable persists the enable intent and starts the plugin.
func (h *Host) Enable(ctx context.Context, id string) error {
	return h.manager.Enable(ctx, id)
}

// Start starts an already enabled plugin.
func (h *Host) Start(ctx context.Context, id string) error {
	return h.manager.Start(ctx, id)
}

// Stop stops the instance and keeps the enable intent.
func (h *Host) Stop(ctx context.Context, id string) error {
	return h.manager.Stop(ctx, id)
}

// Disable clears the enable intent and stops the plugin.
func (h *Host) Disable(ctx context.Context, id string) error {
	return h.manager.Disable(ctx, id)
}

// Uninstall removes the plugin package, configuration and grants.
func (h *Host) Uninstall(ctx context.Context, id string, opts UninstallOptions) error {
	return h.manager.Uninstall(ctx, id, opts.PurgeData)
}

// Close stops all instances and closes the host database.
func (h *Host) Close(ctx context.Context) error {
	h.closeOnce.Do(func() {
		managerErr := h.manager.Close(ctx)
		storeErr := h.store.Close()
		h.closeErr = errors.Join(managerErr, storeErr)
	})
	return h.closeErr
}

func pluginFromView(view lifecycle.View) Plugin {
	plugin := Plugin{
		ID:          view.ID,
		Name:        view.Name,
		Version:     view.Version,
		Enabled:     view.Enabled,
		Status:      Status(view.Status),
		Error:       view.Error,
		HasWeb:      view.HasWeb,
		HasLua:      view.HasLua,
		WebPath:     view.WebPath,
		APIPath:     view.APIPath,
		EventsPath:  view.EventsPath,
		Config:      view.Config,
		InstalledAt: view.InstalledAt,
		StartedAt:   view.StartedAt,
		LogPath:     view.LogPath,
	}
	if len(view.ConfigSchema) > 0 {
		plugin.ConfigSchema = make(map[string]ConfigField, len(view.ConfigSchema))
		for name, field := range view.ConfigSchema {
			defaultValue, _ := field.DefaultValue()
			plugin.ConfigSchema[name] = ConfigField{
				Type:     field.Type,
				Required: field.Required,
				Secret:   field.Secret,
				Default:  defaultValue,
			}
		}
	}
	return plugin
}
