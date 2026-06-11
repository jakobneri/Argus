// Package services provides read-only access to systemd service units.
// External systems sit behind the Manager interface so they can be mocked
// in tests.
package services

import (
	"context"
	"fmt"
	"sort"
	"strings"

	sddbus "github.com/coreos/go-systemd/v22/dbus"
)

// Service describes one systemd service unit.
type Service struct {
	Name        string
	Description string
	LoadState   string
	ActiveState string
	SubState    string
}

// Manager lists systemd service units. Implementations must be read-only.
type Manager interface {
	List(ctx context.Context) ([]Service, error)
}

// unitLister is the slice of the go-systemd dbus connection that the manager
// needs; it exists so tests can substitute a fake backend.
type unitLister interface {
	ListUnitsContext(ctx context.Context) ([]sddbus.UnitStatus, error)
	Close()
}

// SystemdManager implements Manager against the systemd D-Bus API
// (coreos/go-systemd, pure Go). A fresh connection is opened per List call,
// which keeps the manager stateless across daemon reloads and is cheap at
// M1 polling rates.
type SystemdManager struct {
	connect func(ctx context.Context) (unitLister, error)
}

// NewSystemdManager returns a Manager backed by the local systemd instance.
func NewSystemdManager() *SystemdManager {
	return &SystemdManager{
		connect: func(ctx context.Context) (unitLister, error) {
			conn, err := sddbus.NewWithContext(ctx)
			if err != nil {
				return nil, fmt.Errorf("connect to systemd dbus: %w", err)
			}
			return conn, nil
		},
	}
}

// List returns all loaded *.service units, sorted by name.
func (m *SystemdManager) List(ctx context.Context) ([]Service, error) {
	conn, err := m.connect(ctx)
	if err != nil {
		return nil, fmt.Errorf("systemd: %w", err)
	}
	defer conn.Close()

	units, err := conn.ListUnitsContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("systemd: list units: %w", err)
	}
	return servicesFromUnits(units), nil
}

// servicesFromUnits converts raw unit statuses to Services, keeping only
// *.service units and sorting them by name.
func servicesFromUnits(units []sddbus.UnitStatus) []Service {
	out := make([]Service, 0, len(units))
	for _, u := range units {
		if !strings.HasSuffix(u.Name, ".service") {
			continue
		}
		out = append(out, Service{
			Name:        u.Name,
			Description: u.Description,
			LoadState:   u.LoadState,
			ActiveState: u.ActiveState,
			SubState:    u.SubState,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
