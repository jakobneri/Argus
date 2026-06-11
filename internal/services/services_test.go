package services

import (
	"context"
	"errors"
	"reflect"
	"testing"

	sddbus "github.com/coreos/go-systemd/v22/dbus"
)

type fakeLister struct {
	units []sddbus.UnitStatus
	err   error
}

func (f *fakeLister) ListUnitsContext(_ context.Context) ([]sddbus.UnitStatus, error) {
	return f.units, f.err
}

func (f *fakeLister) Close() {}

func TestSystemdManagerList(t *testing.T) {
	tests := []struct {
		name    string
		units   []sddbus.UnitStatus
		listErr error
		connErr error
		want    []Service
		wantErr bool
	}{
		{
			name:  "empty",
			units: nil,
			want:  []Service{},
		},
		{
			name: "filters non-service units and sorts by name",
			units: []sddbus.UnitStatus{
				{Name: "ssh.service", Description: "OpenSSH server", LoadState: "loaded", ActiveState: "active", SubState: "running"},
				{Name: "tmp.mount", Description: "Temporary Directory", LoadState: "loaded", ActiveState: "active", SubState: "mounted"},
				{Name: "docker.socket", Description: "Docker Socket", LoadState: "loaded", ActiveState: "active", SubState: "listening"},
				{Name: "cron.service", Description: "Cron daemon", LoadState: "loaded", ActiveState: "failed", SubState: "failed"},
			},
			want: []Service{
				{Name: "cron.service", Description: "Cron daemon", LoadState: "loaded", ActiveState: "failed", SubState: "failed"},
				{Name: "ssh.service", Description: "OpenSSH server", LoadState: "loaded", ActiveState: "active", SubState: "running"},
			},
		},
		{
			name:    "list error is wrapped",
			listErr: errors.New("dbus broke"),
			wantErr: true,
		},
		{
			name:    "connect error is wrapped",
			connErr: errors.New("no systemd"),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &SystemdManager{
				connect: func(_ context.Context) (unitLister, error) {
					if tt.connErr != nil {
						return nil, tt.connErr
					}
					return &fakeLister{units: tt.units, err: tt.listErr}, nil
				},
			}
			got, err := m.List(context.Background())
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected an error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("List = %+v, want %+v", got, tt.want)
			}
		})
	}
}
