package containers

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/docker/docker/api/types/container"
)

type fakeLister struct {
	summaries []container.Summary
	err       error
}

func (f *fakeLister) ContainerList(_ context.Context, _ container.ListOptions) ([]container.Summary, error) {
	return f.summaries, f.err
}

func (f *fakeLister) Close() error { return nil }

func TestDockerManagerList(t *testing.T) {
	tests := []struct {
		name      string
		summaries []container.Summary
		listErr   error
		connErr   error
		want      []Container
		wantErr   bool
	}{
		{
			name: "empty",
			want: []Container{},
		},
		{
			name: "strips leading slash and sorts by name",
			summaries: []container.Summary{
				{Names: []string{"/web"}, Image: "nginx:latest", State: "running", Status: "Up 2 hours"},
				{Names: []string{"/db", "/db-alias"}, Image: "postgres:16", State: "exited", Status: "Exited (0) 1 day ago"},
				{Names: nil, Image: "busybox", State: "created", Status: "Created"},
			},
			want: []Container{
				{Name: "", Image: "busybox", State: "created", Status: "Created"},
				{Name: "db", Image: "postgres:16", State: "exited", Status: "Exited (0) 1 day ago"},
				{Name: "web", Image: "nginx:latest", State: "running", Status: "Up 2 hours"},
			},
		},
		{
			name:    "daemon unreachable is a wrapped, non-fatal error",
			listErr: errors.New("cannot connect to the docker daemon"),
			wantErr: true,
		},
		{
			name:    "client creation failure is wrapped",
			connErr: errors.New("bad DOCKER_HOST"),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &DockerManager{
				connect: func() (containerLister, error) {
					if tt.connErr != nil {
						return nil, tt.connErr
					}
					return &fakeLister{summaries: tt.summaries, err: tt.listErr}, nil
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
