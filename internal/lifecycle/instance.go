package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/EziosWJ/Extension-Host/internal/lua"
	"github.com/EziosWJ/Extension-Host/internal/manifest"
	"github.com/EziosWJ/Extension-Host/internal/native"
	"github.com/EziosWJ/Extension-Host/internal/serviceaccess"
	"github.com/EziosWJ/Extension-Host/internal/store"
)

const eventBuffer = 32

// Instance is one running plugin in this host process.
type Instance struct {
	id          string
	version     string
	packageDir  string
	luaEntry    string
	hasLua      bool
	nativeEntry string
	nativeSpec  *manifest.Native
	nativeHost  *native.Server
	process     *native.Process
	dataDir     string
	logPath     string
	config      func() map[string]any
	services    *serviceaccess.Access

	logger   *slog.Logger
	logFile  *os.File
	business *store.Business
	runtime  *lua.Runtime

	mu            sync.Mutex
	subscriptions map[*Subscription]chan []byte
	done          chan struct{}
	closed        bool
}

type instanceOptions struct {
	id         string
	version    string
	packageDir string
	manifest   *manifest.Manifest
	pluginDir  string
	logPath    string
	config     func() map[string]any
	services   *serviceaccess.Access
	nativeHost *native.Server
}

func newInstance(opts instanceOptions) *Instance {
	instance := &Instance{
		id:            opts.id,
		version:       opts.version,
		packageDir:    opts.packageDir,
		luaEntry:      opts.manifest.Entry.Lua,
		hasLua:        opts.manifest.Entry.Lua != "",
		nativeEntry:   opts.manifest.Entry.Native,
		nativeSpec:    opts.manifest.Native,
		nativeHost:    opts.nativeHost,
		dataDir:       filepath.Join(opts.pluginDir, "data"),
		logPath:       opts.logPath,
		config:        opts.config,
		services:      opts.services,
		subscriptions: make(map[*Subscription]chan []byte),
		done:          make(chan struct{}),
	}
	return instance
}

// Start opens the plugin log and starts its backend.
func (i *Instance) Start(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(i.logPath), 0o700); err != nil {
		return fmt.Errorf("create log directory: %w", err)
	}
	file, err := os.OpenFile(i.logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open plugin log: %w", err)
	}
	i.logFile = file
	i.logger = slog.New(slog.NewTextHandler(file, &slog.HandlerOptions{Level: slog.LevelInfo})).
		With("plugin", i.id, "version", i.version)

	if !i.hasLua && i.nativeSpec == nil {
		return nil
	}
	if err := os.MkdirAll(i.dataDir, 0o700); err != nil {
		i.logger.Error("create plugin data directory", "error", err)
		i.closeLog()
		return fmt.Errorf("create plugin data directory: %w", err)
	}
	if i.nativeSpec != nil {
		process, err := i.nativeHost.Start(ctx, native.Options{
			ID: i.id, Executable: filepath.Join(i.packageDir, filepath.FromSlash(i.nativeEntry)),
			PackageDir: i.packageDir, DataDir: i.dataDir, Config: i.config,
			Log: file, Exports: i.nativeSpec.Exports, WebAPI: i.nativeSpec.WebAPI,
		})
		if err != nil {
			i.logger.Error("start native plugin", "error", err)
			i.closeLog()
			return err
		}
		i.process = process
		i.logger.Info("native plugin instance started")
		return nil
	}
	business, err := store.OpenBusiness(ctx, filepath.Join(filepath.Dir(i.dataDir), "business.db"))
	if err != nil {
		i.logger.Error("open plugin database", "error", err)
		i.closeLog()
		return err
	}
	i.business = business
	i.runtime = lua.New(lua.Options{
		EntryPath:   filepath.Join(i.packageDir, filepath.FromSlash(i.luaEntry)),
		PackageRoot: i.packageDir,
		DataDir:     i.dataDir,
		Logger:      i.logger,
		Config:      i.config,
		Storage:     business,
		Emit:        i.Emit,
		Services:    i.services,
	})
	if err := i.runtime.Start(ctx); err != nil {
		if !errors.Is(err, lua.ErrExited) {
			i.logger.Error("start plugin runtime", "error", err)
		}
		_ = business.Close()
		i.business = nil
		i.runtime = nil
		i.closeLog()
		return err
	}
	i.logger.Info("plugin instance started")
	return nil
}

// Stop releases the runtime and resources associated with this instance.
func (i *Instance) Stop(ctx context.Context) error {
	i.CloseSubscriptions()
	var errs []error
	if i.process != nil {
		errs = append(errs, i.process.Stop(ctx))
	}
	if i.runtime != nil {
		if err := i.runtime.Stop(ctx); err != nil {
			i.logger.Error("stop plugin runtime", "error", err)
			errs = append(errs, err)
		}
	}
	if i.business != nil {
		if err := i.business.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close plugin database: %w", err))
		}
		i.business = nil
	}
	if i.logger != nil {
		i.logger.Info("plugin instance stopped")
	}
	i.closeLog()
	return errors.Join(errs...)
}

func (i *Instance) closeLog() {
	if i.logFile != nil {
		_ = i.logFile.Close()
		i.logFile = nil
	}
}

// HasLua reports whether the plugin exposes a Lua entry.
func (i *Instance) HasLua() bool { return i.hasLua }

// Native returns the native runtime, when the plugin has one.
func (i *Instance) Native() *native.Process { return i.process }

func (i *Instance) runtimeDone() <-chan struct{} {
	if i.process != nil {
		return i.process.Done()
	}
	if i.runtime != nil {
		return i.runtime.Done()
	}
	return nil
}

// Handle dispatches an HTTP request to the plugin Lua runtime.
func (i *Instance) Handle(ctx context.Context, request lua.Request) (lua.Response, error) {
	if i.runtime == nil {
		return lua.Response{}, ErrNoLua
	}
	response, err := i.runtime.Handle(ctx, request)
	if err != nil && !errors.Is(err, lua.ErrStopped) {
		i.logger.Error("plugin request failed", "method", request.Method, "path", request.Path, "error", err)
	}
	return response, err
}

// Subscription receives plugin events as JSON payloads.
type Subscription struct {
	// C carries encoded {"event": ..., "data": ...} messages.
	C <-chan []byte
	// Done is closed when the plugin instance stops.
	Done <-chan struct{}

	instance *Instance
	channel  chan []byte
}

// Close releases the subscription.
func (s *Subscription) Close() {
	if s.instance != nil {
		s.instance.unsubscribe(s)
		s.instance = nil
	}
}

// Subscribe adds a browser event subscription.
func (i *Instance) Subscribe() (*Subscription, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed {
		return nil, ErrNotRunning
	}
	channel := make(chan []byte, eventBuffer)
	subscription := &Subscription{C: channel, Done: i.done, instance: i, channel: channel}
	i.subscriptions[subscription] = channel
	return subscription, nil
}

func (i *Instance) unsubscribe(subscription *Subscription) {
	i.mu.Lock()
	defer i.mu.Unlock()
	delete(i.subscriptions, subscription)
}

// Emit publishes one event to all current browser subscribers.
func (i *Instance) Emit(event string, data any) error {
	payload, err := json.Marshal(struct {
		Event string `json:"event"`
		Data  any    `json:"data"`
	}{Event: event, Data: data})
	if err != nil {
		return fmt.Errorf("encode event: %w", err)
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed {
		return nil
	}
	for _, channel := range i.subscriptions {
		select {
		case channel <- payload:
		default:
			i.logger.Warn("dropping plugin event for slow subscriber", "event", event)
		}
	}
	return nil
}

// CloseSubscriptions releases every browser subscription.
func (i *Instance) CloseSubscriptions() {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed {
		return
	}
	i.closed = true
	i.subscriptions = make(map[*Subscription]chan []byte)
	close(i.done)
}

// LogPath returns the plugin log file path.
func (i *Instance) LogPath() string { return i.logPath }
