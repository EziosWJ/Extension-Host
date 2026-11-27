package lifecycle

import "errors"

var (
	// ErrIncompatible means the package cannot run on this installation host.
	ErrIncompatible = errors.New("incompatible plugin package")
	// ErrNotFound means the plugin is not installed.
	ErrNotFound = errors.New("plugin is not installed")
	// ErrConflict means a plugin with the same ID is already installed.
	ErrConflict = errors.New("plugin is already installed")
	// ErrIDMismatch means the uploaded package has a different plugin ID.
	ErrIDMismatch = errors.New("package ID does not match target plugin")
	// ErrNotEnabled means the plugin has no persisted enable intent.
	ErrNotEnabled = errors.New("plugin is not enabled")
	// ErrNotRunning means the plugin has no running instance.
	ErrNotRunning = errors.New("plugin is not running")
	// ErrNoLua means the plugin has no Lua entry.
	ErrNoLua = errors.New("plugin has no Lua entry")
	// ErrNoWeb means the plugin has no web entry.
	ErrNoWeb = errors.New("plugin has no web entry")
	// ErrClosed means the host has been closed.
	ErrClosed = errors.New("extension host is closed")
)
