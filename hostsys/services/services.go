// Package services manages systemd services for the native GoDesk app and for
// extension-host plugins. Native and plugin callers share the same discovery,
// parsing and command execution code but use different privilege policies.
package services

import (
	"context"
	"fmt"
	"strings"
)

// State is the normalized runtime state shown by the desktop.
type State string

const (
	StateRunning   State = "running"
	StateCompleted State = "completed"
	StateStopped   State = "stopped"
	StateFailed    State = "failed"
	StateUnknown   State = "unknown"
)

// BootState is the normalized boot-time enablement shown by the desktop.
type BootState string

const (
	BootEnabled       BootState = "enabled"
	BootDisabled      BootState = "disabled"
	BootStatic        BootState = "static"
	BootMasked        BootState = "masked"
	BootRuntime       BootState = "runtime"
	BootNotApplicable BootState = "not_applicable"
)

// Action is a service operation exposed by a backend.
type Action string

const (
	ActionStart   Action = "start"
	ActionStop    Action = "stop"
	ActionRestart Action = "restart"
	ActionReload  Action = "reload"
	ActionEnable  Action = "enable"
	ActionDisable Action = "disable"
)

var actionOrder = []Action{ActionStart, ActionStop, ActionRestart, ActionReload, ActionEnable, ActionDisable}

// Capabilities describes what a backend supports as a whole.
type Capabilities struct {
	Start         bool `json:"start"`
	Stop          bool `json:"stop"`
	Restart       bool `json:"restart"`
	Reload        bool `json:"reload"`
	DaemonReload  bool `json:"daemonReload"`
	Enable        bool `json:"enable"`
	Disable       bool `json:"disable"`
	Logs          bool `json:"logs"`
	StateReliable bool `json:"stateReliable"`
}

// Service is one managed service unit.
type Service struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	State       State     `json:"state"`
	StateDetail string    `json:"stateDetail"`
	BootState   BootState `json:"bootState"`
	Actions     []Action  `json:"actions"`
}

// ServiceDetail is the extended per-unit view shown in the service detail pane.
type ServiceDetail struct {
	Name                 string    `json:"name"`
	State                State     `json:"state"`
	StateDetail          string    `json:"stateDetail"`
	BootState            BootState `json:"bootState"`
	MainPID              *int      `json:"mainPid"`
	MemoryBytes          *int64    `json:"memoryBytes"`
	CPUUsageNs           *int64    `json:"cpuUsageNs"`
	ActiveEnterTimestamp string    `json:"activeEnterTimestamp"`
	FragmentPath         string    `json:"fragmentPath"`
}

// Backend is the service manager interface for native GoDesk callers.
type Backend interface {
	Name() string
	Capabilities() Capabilities
	List(ctx context.Context) ([]Service, error)
	Detail(ctx context.Context, unit string) (ServiceDetail, error)
	Start(ctx context.Context, unit string) error
	Stop(ctx context.Context, unit string) error
	Restart(ctx context.Context, unit string) error
	Reload(ctx context.Context, unit string) error
	DaemonReload(ctx context.Context) error
	Enable(ctx context.Context, unit string) error
	Disable(ctx context.Context, unit string) error
	Logs(ctx context.Context, unit string, lines int) ([]string, error)
	// StreamLogs follows journal output, emitting lines without their newline.
	// initialLines is 0 through MaxLogLines inclusive. Zero follows new entries only.
	// emit must be non-nil and is called synchronously. StreamLogs propagates
	// emit errors and returns ctx.Err() when the context ends.
	StreamLogs(ctx context.Context, unit string, initialLines int, emit func(line string) error) error
}

// MaxLogLines is the maximum number of historical journal lines in one request.
const MaxLogLines = 2000

// Error carries a machine-readable code and a user-facing message.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

// ValidAction reports whether action is a known service action.
func ValidAction(action Action) bool {
	switch action {
	case ActionStart, ActionStop, ActionRestart, ActionReload, ActionEnable, ActionDisable:
		return true
	default:
		return false
	}
}

// ValidUnit reports whether name is a syntactically valid systemd service unit
// name. It accepts the characters used by unit names, including \xNN escapes,
// and rejects names that could be interpreted as command options.
func ValidUnit(name string) bool {
	if name == "" || len(name) > 255 || strings.HasPrefix(name, "-") {
		return false
	}
	if !strings.HasSuffix(name, ".service") {
		return false
	}
	for i := 0; i < len(name); {
		switch c := name[i]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			i++
		case c == ':' || c == '_' || c == '.' || c == '@' || c == '-':
			i++
		case c == '\\' && i+3 < len(name) && name[i+1] == 'x' && isHex(name[i+2]) && isHex(name[i+3]):
			i += 4
		default:
			return false
		}
	}
	return true
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func validateUnit(unit string) error {
	if ValidUnit(unit) {
		return nil
	}
	return &Error{Code: "invalid_unit", Message: fmt.Sprintf("invalid unit %q", unit)}
}

func actionsFor(state State, boot BootState, capabilities Capabilities) []Action {
	actions := make([]Action, 0, len(actionOrder))
	add := func(action Action, supported bool) {
		if supported {
			actions = append(actions, action)
		}
	}
	switch state {
	case StateRunning, StateCompleted:
		add(ActionStop, capabilities.Stop)
		add(ActionRestart, capabilities.Restart)
	case StateStopped:
		add(ActionStart, capabilities.Start)
	case StateFailed:
		add(ActionStart, capabilities.Start)
		add(ActionRestart, capabilities.Restart)
	case StateUnknown:
		add(ActionStart, capabilities.Start)
		add(ActionStop, capabilities.Stop)
		add(ActionRestart, capabilities.Restart)
	}
	if boot == BootMasked {
		kept := actions[:0]
		for _, action := range actions {
			if action == ActionStop {
				kept = append(kept, action)
			}
		}
		actions = kept
	}
	add(ActionReload, state == StateRunning && capabilities.Reload)
	switch boot {
	case BootEnabled:
		add(ActionDisable, capabilities.Disable)
	case BootDisabled:
		add(ActionEnable, capabilities.Enable)
	}
	ordered := make([]Action, 0, len(actions))
	for _, action := range actionOrder {
		for _, candidate := range actions {
			if candidate == action {
				ordered = append(ordered, action)
				break
			}
		}
	}
	return ordered
}

// ContainsAction reports whether actions includes action.
func ContainsAction(actions []Action, action Action) bool {
	for _, candidate := range actions {
		if candidate == action {
			return true
		}
	}
	return false
}
