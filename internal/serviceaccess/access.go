// Package serviceaccess applies a plugin's grants to the systemd backend.
package serviceaccess

import (
	"context"
	"fmt"

	"github.com/EziosWJ/Extension-Host/hostsys/services"
)

// Access resolves the service operations available to one plugin.
type Access struct {
	Backend services.PluginBackend
	Units   map[string]Permissions
}

// Permissions lists the operations granted for one unit.
type Permissions struct {
	List            bool
	Get             bool
	Restart         bool
	RestartElevated bool
}

// Error describes an invalid request or a missing grant.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

func (a *Access) List(ctx context.Context) ([]services.Service, error) {
	const operation = "host.services.list"
	if a == nil {
		return nil, noCapability(operation)
	}
	granted := false
	for _, permissions := range a.Units {
		if permissions.List {
			granted = true
			break
		}
	}
	if !granted {
		return nil, &Error{"forbidden", operation + " is not authorized: no service grant is configured"}
	}
	items, err := a.Backend.List(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]services.Service, 0, len(items))
	for _, item := range items {
		if a.Units[item.Name].List {
			result = append(result, item)
		}
	}
	return result, nil
}

func (a *Access) Detail(ctx context.Context, unit string) (services.ServiceDetail, error) {
	const operation = "host.services.get"
	if err := a.validateUnit(operation, unit); err != nil {
		return services.ServiceDetail{}, err
	}
	if !a.Units[unit].Get {
		return services.ServiceDetail{}, denied(operation, unit)
	}
	return a.Backend.Detail(ctx, unit)
}

func (a *Access) Restart(ctx context.Context, unit string) error {
	const operation = "host.services.restart"
	if err := a.validateUnit(operation, unit); err != nil {
		return err
	}
	permissions := a.Units[unit]
	if !permissions.Restart {
		return denied(operation, unit)
	}
	return a.Backend.Restart(ctx, unit, permissions.RestartElevated)
}

func (a *Access) validateUnit(operation, unit string) error {
	if a == nil {
		return noCapability(operation)
	}
	if !services.ValidUnit(unit) {
		return &Error{"invalid_unit", fmt.Sprintf("%s: invalid unit %q", operation, unit)}
	}
	return nil
}

func noCapability(operation string) error {
	return &Error{"forbidden", operation + " is not authorized: no capability grant is configured"}
}

func denied(operation, unit string) error {
	return &Error{"forbidden", fmt.Sprintf("%s is not authorized for unit %s", operation, unit)}
}
