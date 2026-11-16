package deployment

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hasankaradmir/karga/internal/adapters/fakeruntime"
	"github.com/hasankaradmir/karga/internal/domain/resource"
	"github.com/hasankaradmir/karga/internal/ports"
)

func init() { pollInterval = time.Millisecond }

func manifest(port int) []resource.Resource {
	return []resource.Resource{
		{Kind: resource.KindWorkload, Metadata: resource.Metadata{Name: "redis"}, Spec: &resource.WorkloadSpec{
			Image: "redis:7.4", PullPolicy: "if-not-present", RestartPolicy: "always",
			Command:  []string{"redis-server"},
			Networks: []string{"net"},
			Ports:    []resource.Port{{ContainerPort: 6379, HostPort: port, HostIP: "127.0.0.1", Protocol: "tcp"}},
			Mounts: []resource.Mount{
				{Volume: "data", MountPath: "/data"},
				{Config: "conf", MountPath: "/etc/redis"},
			},
			HealthCheck: &resource.HealthCheck{Type: "exec", Command: []string{"redis-cli", "ping"}, Interval: "10s", Timeout: "3s", Retries: 3},
			Resources:   &resource.Resources{Memory: "512Mi", CPU: "500m"},
		}},
		cfgRes("conf", map[string]string{"redis.conf": "x"}),
		{Kind: resource.KindVolume, Metadata: resource.Metadata{Name: "data"}, Spec: &resource.VolumeSpec{Driver: "local", Retention: "retain"}},
		{Kind: resource.KindVolume, Metadata: resource.Metadata{Name: "cache"}, Spec: &resource.VolumeSpec{Driver: "local", Retention: "delete"}},
		{Kind: resource.KindNetwork, Metadata: resource.Metadata{Name: "net"}, Spec: &resource.NetworkSpec{Driver: "bridge"}},
	}
}

func request(t *testing.T, port int) Request {
	return Request{Home: t.TempDir(), Release: "cache", Revision: 1, Supported: []string{"docker"}, Resources: manifest(port), Timeout: time.Second}
}

func TestApply(t *testing.T) {
	rt := fakeruntime.New()
	req := request(t, freePort(t))
	if err := Apply(context.Background(), rt, req); err != nil {
		t.Fatal(err)
	}

	// Image pulled before anything is created; dependencies before the workload.
	idx := func(call string) int { return slices.Index(rt.Calls, call) }
	sequence := []string{"Pull:redis:7.4", "CreateNetwork:cache-net", "CreateVolume:cache-data", "CreateWorkload:cache-redis", "StartWorkload:cache-redis"}
	for i := 1; i < len(sequence); i++ {
		if idx(sequence[i-1]) < 0 || idx(sequence[i-1]) > idx(sequence[i]) {
			t.Fatalf("%s must come before %s: %v", sequence[i-1], sequence[i], rt.Calls)
		}
	}

	w := rt.Workloads["cache-redis"].Request
	if w.Labels[ports.LabelRelease] != "cache" || w.Labels[ports.LabelName] != "redis" || w.Labels[ports.LabelRevision] != "1" {
		t.Fatalf("labels: %v", w.Labels)
	}
	if !slices.Equal(w.Networks, []string{"cache-net"}) || !slices.Equal(w.Aliases, []string{"redis"}) {
		t.Fatalf("networks/aliases: %v %v", w.Networks, w.Aliases)
	}
	if w.Ports[0].HostIP != "127.0.0.1" || w.MemoryBytes != 512<<20 || w.MilliCPU != 500 {
		t.Fatalf("ports/resources: %+v", w)
	}
	if w.HealthCheck.Interval != 10*time.Second || w.HealthCheck.Retries != 3 {
		t.Fatalf("healthcheck: %+v", w.HealthCheck)
	}
	if w.Mounts[0] != (ports.Mount{Type: ports.MountVolume, Source: "cache-data", Target: "/data"}) {
		t.Fatalf("volume mount: %+v", w.Mounts[0])
	}
	if m := w.Mounts[1]; m.Type != ports.MountBind || !m.ReadOnly || !strings.HasSuffix(m.Source, "releases/cache/1/configs/conf") {
		t.Fatalf("config mount: %+v", m)
	}
	if rt.Volumes["cache-data"].Labels[ports.LabelRetention] != "retain" {
		t.Fatalf("volume labels: %v", rt.Volumes["cache-data"].Labels)
	}
}

func TestApplyImageFailureCreatesNothing(t *testing.T) {
	rt := fakeruntime.New()
	req := request(t, freePort(t))
	req.Resources[0].Spec.(*resource.WorkloadSpec).Image = "missing/redis:0"
	err := Apply(context.Background(), rt, req)
	if !errors.Is(err, ports.ErrImageNotFound) {
		t.Fatalf("want ErrImageNotFound, got %v", err)
	}
	if len(rt.Networks)+len(rt.Volumes)+len(rt.Workloads) != 0 {
		t.Fatalf("objects created despite image failure: %v", rt.Calls)
	}
}

func TestApplyFailures(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*fakeruntime.Runtime)
		want  string
	}{
		{"unhealthy", func(r *fakeruntime.Runtime) { r.Health = ports.HealthUnhealthy }, "unhealthy"},
		{"never healthy", func(r *fakeruntime.Runtime) { r.Health = ports.HealthStarting }, "not ready after"},
		{"start fails", func(r *fakeruntime.Runtime) { r.Fail["StartWorkload:cache-redis"] = errors.New("boom") }, `Workload "redis": boom`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := fakeruntime.New()
			tt.setup(rt)
			req := request(t, freePort(t))
			req.Timeout = 50 * time.Millisecond
			err := Apply(context.Background(), rt, req)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("want %q, got %v", tt.want, err)
			}
		})
	}
}

func TestApplyExistingWorkloadConflicts(t *testing.T) {
	rt := fakeruntime.New()
	req := request(t, freePort(t))
	if err := Apply(context.Background(), rt, req); err != nil {
		t.Fatal(err)
	}
	if err := Apply(context.Background(), rt, req); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("second apply: want ErrConflict, got %v", err)
	}
}

func TestDelete(t *testing.T) {
	ctx := context.Background()
	for _, deleteVolumes := range []bool{false, true} {
		rt := fakeruntime.New()
		req := request(t, freePort(t))
		if err := Apply(ctx, rt, req); err != nil {
			t.Fatal(err)
		}
		if err := Delete(ctx, rt, req.Home, req.Release, deleteVolumes); err != nil {
			t.Fatal(err)
		}
		if len(rt.Workloads) != 0 || len(rt.Networks) != 0 {
			t.Fatalf("workloads/networks left: %v %v", rt.Workloads, rt.Networks)
		}
		_, dataKept := rt.Volumes["cache-data"]
		_, cacheKept := rt.Volumes["cache-cache"]
		if dataKept == deleteVolumes || cacheKept {
			t.Fatalf("deleteVolumes=%v: data kept=%v, cache (retention delete) kept=%v", deleteVolumes, dataKept, cacheKept)
		}
		if !slices.Contains(rt.Calls, "StopWorkload:cache-redis") {
			t.Fatal("workload not stopped gracefully before delete")
		}
	}
}
