// Package serviceaccess validates service requests from plugin Host APIs.
package serviceaccess

import (
	"context"
	"fmt"

	"github.com/EziosWJ/Extension-Host/hostsys/services"
)

const (
	DefaultLogLines = 200
	MaxLogLines     = services.MaxLogLines
)

// Access is the shared service backend with validation for plugin-supplied unit names.
type Access struct {
	Backend services.PluginBackend
}

func (a *Access) List(ctx context.Context) ([]services.Service, error) {
	return a.Backend.List(ctx)
}

func (a *Access) Detail(ctx context.Context, unit string) (services.ServiceDetail, error) {
	if err := validateUnit(unit); err != nil {
		return services.ServiceDetail{}, err
	}
	return a.Backend.Detail(ctx, unit)
}

func (a *Access) Start(ctx context.Context, unit string, sudo bool) error {
	if err := validateUnit(unit); err != nil {
		return err
	}
	return a.Backend.Start(ctx, unit, sudo)
}

func (a *Access) Stop(ctx context.Context, unit string, sudo bool) error {
	if err := validateUnit(unit); err != nil {
		return err
	}
	return a.Backend.Stop(ctx, unit, sudo)
}

func (a *Access) Restart(ctx context.Context, unit string, sudo bool) error {
	if err := validateUnit(unit); err != nil {
		return err
	}
	return a.Backend.Restart(ctx, unit, sudo)
}

func (a *Access) Logs(ctx context.Context, unit string, lines int) ([]string, error) {
	if err := validateUnit(unit); err != nil {
		return nil, err
	}
	return a.Backend.Logs(ctx, unit, lines)
}

func validateUnit(unit string) error {
	if services.ValidUnit(unit) {
		return nil
	}
	return &services.Error{Code: "invalid_unit", Message: fmt.Sprintf("invalid unit %q", unit)}
}
