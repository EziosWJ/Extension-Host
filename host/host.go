// Package host is the public embedding API of the GoDesk Extension Host.
package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
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

// ServiceGrant grants one plugin access to a subset of systemd units.
type ServiceGrant struct {
	// Units lists the service unit names the plugin may access.
	Units []string `json:"units"`
	// Actions lists the allowed operations: "list", "get" and/or "restart".
	Actions []string `json:"actions"`
	// Elevate lists granted actions that run through non-interactive sudo.
	// Only "restart" is used.
	Elevate []string `json:"elevate,omitempty"`
}

// Options configures a Host.
type Options struct {
	// DataDir holds host.db, packages, plugin data and logs.
	DataDir string
	// Logger records host lifecycle events. Defaults to slog.Default.
	Logger *slog.Logger
	// ServiceGrants grants per-plugin service access, keyed by plugin ID.
	ServiceGrants map[string]ServiceGrant
	// ServiceBackend overrides the plugin service backend. Defaults to
	// services.OpenPlugin("auto").
	ServiceBackend services.PluginBackend
}

// LoadServiceGrantsFile reads service grants from a JSON object keyed by
// plugin ID. An empty path returns nil without reading a file.
func LoadServiceGrantsFile(path string) (map[string]ServiceGrant, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("host: read service grants: %w", err)
	}
	grants := make(map[string]ServiceGrant)
	if err := json.Unmarshal(data, &grants); err != nil {
		return nil, fmt.Errorf("host: parse service grants %s: %w", path, err)
	}
	return grants, nil
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
	if err := validateServiceGrants(opts.ServiceGrants); err != nil {
		return nil, err
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
		DataDir:           dataDir,
		Store:             database,
		Logger:            logger,
		ServicesForPlugin: serviceGrantResolver(opts.ServiceGrants, serviceBackend),
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

var serviceGrantActions = map[string]bool{"list": true, "get": true, "restart": true}

func validateServiceGrants(grants map[string]ServiceGrant) error {
	ids := make([]string, 0, len(grants))
	for id := range grants {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		grant := grants[id]
		if id == "" {
			return errors.New("host: service grant has an empty plugin id")
		}
		granted := make(map[string]bool, len(grant.Actions))
		for _, action := range grant.Actions {
			if !serviceGrantActions[action] {
				return fmt.Errorf("host: service grant for plugin %q: invalid action %q (want list, get or restart)", id, action)
			}
			granted[action] = true
		}
		for _, unit := range grant.Units {
			if !services.ValidUnit(unit) {
				return fmt.Errorf("host: service grant for plugin %q: invalid unit %q", id, unit)
			}
		}
		for _, action := range grant.Elevate {
			if !granted[action] {
				return fmt.Errorf("host: service grant for plugin %q: elevate action %q is not granted", id, action)
			}
		}
	}
	return nil
}

func serviceGrantResolver(grants map[string]ServiceGrant, backend services.PluginBackend) func(string, []string) *serviceaccess.Access {
	return func(pluginID string, capabilities []string) *serviceaccess.Access {
		if !declaresCapability(capabilities, "services") {
			return nil
		}
		grant, ok := grants[pluginID]
		if !ok {
			return nil
		}
		units := make(map[string]serviceaccess.Permissions, len(grant.Units))
		for _, unit := range grant.Units {
			permissions := serviceaccess.Permissions{}
			for _, action := range grant.Actions {
				switch action {
				case "list":
					permissions.List = true
				case "get":
					permissions.Get = true
				case "restart":
					permissions.Restart = true
				}
			}
			for _, action := range grant.Elevate {
				if action == "restart" {
					permissions.RestartElevated = true
				}
			}
			units[unit] = permissions
		}
		return &serviceaccess.Access{Backend: backend, Units: units}
	}
}

func declaresCapability(capabilities []string, name string) bool {
	for _, capability := range capabilities {
		if capability == name {
			return true
		}
	}
	return false
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
		HasNative:   view.HasNative,
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
