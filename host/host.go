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

	"github.com/EziosWJ/Extension-Host/hostsys/services"
	"github.com/EziosWJ/Extension-Host/internal/bundle"
	"github.com/EziosWJ/Extension-Host/internal/gateway"
	"github.com/EziosWJ/Extension-Host/internal/lifecycle"
	"github.com/EziosWJ/Extension-Host/internal/manifest"
	"github.com/EziosWJ/Extension-Host/internal/serviceaccess"
	"github.com/EziosWJ/Extension-Host/internal/store"
)

// Version identifies the Extension Host implementation used by the embedder.
const Version = manifest.Version

// HostAPIVersion is the plugin Host API version implemented by this module.
const HostAPIVersion = manifest.HostAPIVersion

// Package errors.
var (
	ErrNotFound          = lifecycle.ErrNotFound
	ErrConflict          = lifecycle.ErrConflict
	ErrIDMismatch        = lifecycle.ErrIDMismatch
	ErrNotEnabled        = lifecycle.ErrNotEnabled
	ErrNotRunning        = lifecycle.ErrNotRunning
	ErrNoLua             = lifecycle.ErrNoLua
	ErrNoWeb             = lifecycle.ErrNoWeb
	ErrClosed            = lifecycle.ErrClosed
	ErrInvalidPackage    = errors.New("invalid plugin package")
	ErrInvalidHomePlugin = errors.New("首页插件必须已启用且带有网页")
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
	// ServiceBackend overrides the plugin service backend. Defaults to
	// services.OpenPlugin("auto").
	ServiceBackend services.PluginBackend
}

// HTTPOptions configures the extension HTTP handler.
type HTTPOptions struct {
	// RequireSession protects every extension route with the embedder's session middleware.
	RequireSession func(http.Handler) http.Handler
	// AllowedOrigins lists extra WebSocket origins, useful behind a development proxy.
	AllowedOrigins []string
	// DevelopmentRevision enables browser refresh for a plugin when it returns a revision.
	DevelopmentRevision func(pluginID string) string
}

// Plugin is the management view of an installed plugin.
type Plugin struct {
	Description  string                 `json:"description,omitempty"`
	Author       *Author                `json:"author,omitempty"`
	License      string                 `json:"license,omitempty"`
	Homepage     string                 `json:"homepage,omitempty"`
	Repository   string                 `json:"repository,omitempty"`
	Icon         string                 `json:"icon,omitempty"`
	IconPath     string                 `json:"iconPath,omitempty"`
	Categories   []string               `json:"categories,omitempty"`
	Keywords     []string               `json:"keywords,omitempty"`
	Requirements *Requirements          `json:"requirements,omitempty"`
	Locales      map[string]Locale      `json:"locales,omitempty"`
	ID           string                 `json:"id"`
	Name         string                 `json:"name"`
	Version      string                 `json:"version"`
	Enabled      bool                   `json:"enabled"`
	Status       Status                 `json:"status"`
	Error        string                 `json:"error,omitempty"`
	HasWeb       bool                   `json:"hasWeb"`
	HasNative    bool                   `json:"hasNative"`
	HasLua       bool                   `json:"hasLua"`
	WebPath      string                 `json:"webPath,omitempty"`
	APIPath      string                 `json:"apiPath,omitempty"`
	EventsPath   string                 `json:"eventsPath,omitempty"`
	Config       map[string]any         `json:"config"`
	ConfigSchema map[string]ConfigField `json:"configSchema,omitempty"`
	InstalledAt  time.Time              `json:"installedAt"`
	StartedAt    *time.Time             `json:"startedAt,omitempty"`
	LogPath      string                 `json:"logPath"`
	// RedactedConfig identifies saved values hidden after their fields stopped being secret.
	RedactedConfig map[string]bool `json:"redactedConfig,omitempty"`
}

// ConfigField describes one configuration value.
type ConfigField struct {
	Type        string   `json:"type"`
	Required    bool     `json:"required,omitempty"`
	Secret      bool     `json:"secret,omitempty"`
	Default     any      `json:"default,omitempty"`
	Format      string   `json:"format,omitempty"`
	Min         *float64 `json:"min,omitempty"`
	Max         *float64 `json:"max,omitempty"`
	Label       string   `json:"label,omitempty"`
	Description string   `json:"description,omitempty"`
}

// Author identifies the plugin publisher or maintainer.
type Author = manifest.Author

// Requirements describes non-enforcing environment hints.
type Requirements = manifest.Requirements

// Locale provides translated plugin and configuration text.
type Locale = manifest.Locale

// ConfigLocale provides translated text for a configuration field.
type ConfigLocale = manifest.ConfigLocale

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
	serviceBackend := opts.ServiceBackend
	if serviceBackend == nil {
		serviceBackend, err = services.OpenPlugin("auto")
		if err != nil {
			return nil, fmt.Errorf("host: open service backend: %w", err)
		}
	}
	database, err := store.Open(ctx, filepath.Join(dataDir, "host.db"))
	if err != nil {
		return nil, err
	}
	manager, err := lifecycle.New(ctx, lifecycle.Options{
		DataDir:  dataDir,
		Store:    database,
		Logger:   logger,
		Services: &serviceaccess.Access{Backend: serviceBackend},
	})
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
		RequireSession:      opts.RequireSession,
		AllowedOrigins:      opts.AllowedOrigins,
		DevelopmentRevision: opts.DevelopmentRevision,
	})
}

// Get returns one installed plugin.
func (h *Host) Get(id string) (Plugin, bool) {
	view, ok := h.manager.Get(id)
	return pluginFromView(view), ok
}

// HomePlugin returns the ID selected for the standalone home page.
func (h *Host) HomePlugin(ctx context.Context) (string, error) {
	return h.store.HomePlugin(ctx)
}

// SetHomePlugin selects an enabled web plugin or clears the selection.
func (h *Host) SetHomePlugin(ctx context.Context, id string) error {
	if id != "" {
		plugin, ok := h.Get(id)
		if !ok || !plugin.Enabled || !plugin.HasWeb {
			return ErrInvalidHomePlugin
		}
	}
	return h.store.SetHomePlugin(ctx, id)
}

// ReloadDevelopment replaces an installed development plugin using the same data directory.
func (h *Host) ReloadDevelopment(ctx context.Context, pkg *bundle.Bundle, webOnly bool) error {
	return h.manager.ReloadDevelopment(ctx, pkg, webOnly)
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
	pkg, err := loadPackage(zip)
	if err != nil {
		return Plugin{}, err
	}
	view, err := h.manager.Install(ctx, pkg)
	return pluginFromView(view), err
}

// Upgrade replaces the package of an installed plugin and restarts it if enabled.
// A startup failure after replacement is reported in the returned plugin status.
func (h *Host) Upgrade(ctx context.Context, id string, zip io.Reader) (Plugin, error) {
	pkg, err := loadPackage(zip)
	if err != nil {
		return Plugin{}, err
	}
	view, err := h.manager.Upgrade(ctx, id, pkg)
	return pluginFromView(view), err
}

func loadPackage(zip io.Reader) (*bundle.Bundle, error) {
	data, err := io.ReadAll(io.LimitReader(zip, bundle.MaxPackageSize+1))
	if err != nil {
		return nil, fmt.Errorf("read plugin package: %w", err)
	}
	if len(data) > bundle.MaxPackageSize {
		return nil, fmt.Errorf("%w: package exceeds %d bytes", ErrInvalidPackage, bundle.MaxPackageSize)
	}
	pkg, err := bundle.LoadZip(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidPackage, err)
	}
	return pkg, nil
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

// Uninstall removes the plugin package and configuration.
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
		Description:    view.Description,
		Author:         view.Author,
		License:        view.License,
		Homepage:       view.Homepage,
		Repository:     view.Repository,
		Icon:           view.Icon,
		IconPath:       view.IconPath,
		Categories:     view.Categories,
		Keywords:       view.Keywords,
		Requirements:   view.Requirements,
		Locales:        view.Locales,
		ID:             view.ID,
		Name:           view.Name,
		Version:        view.Version,
		Enabled:        view.Enabled,
		Status:         Status(view.Status),
		Error:          view.Error,
		HasWeb:         view.HasWeb,
		HasLua:         view.HasLua,
		HasNative:      view.HasNative,
		WebPath:        view.WebPath,
		APIPath:        view.APIPath,
		EventsPath:     view.EventsPath,
		Config:         view.Config,
		RedactedConfig: view.RedactedConfig,
		InstalledAt:    view.InstalledAt,
		StartedAt:      view.StartedAt,
		LogPath:        view.LogPath,
	}
	if len(view.ConfigSchema) > 0 {
		plugin.ConfigSchema = make(map[string]ConfigField, len(view.ConfigSchema))
		for name, field := range view.ConfigSchema {
			defaultValue, _ := field.DefaultValue()
			plugin.ConfigSchema[name] = ConfigField{
				Type:        field.Type,
				Required:    field.Required,
				Secret:      field.Secret,
				Default:     defaultValue,
				Format:      field.Format,
				Min:         copyBound(field.Min),
				Max:         copyBound(field.Max),
				Label:       field.Label,
				Description: field.Description,
			}
		}
	}
	return plugin
}

func copyBound(bound *float64) *float64 {
	if bound == nil {
		return nil
	}
	return new(*bound)
}
