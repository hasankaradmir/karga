package deployment

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/hasankaradmir/karga/internal/adapters/fakeruntime"
	"github.com/hasankaradmir/karga/internal/domain/resource"
	"github.com/hasankaradmir/karga/internal/ports"
)

func webWithPort(port int) resource.Resource {
	return resource.Resource{Kind: resource.KindWorkload, Metadata: resource.Metadata{Name: "web"}, Spec: &resource.WorkloadSpec{
		Image: "x", Ports: []resource.Port{{ContainerPort: 80, HostPort: port, HostIP: "127.0.0.1", Protocol: "tcp"}},
		Resources:   &resource.Resources{Memory: "64Mi", CPU: "1"},
		HealthCheck: &resource.HealthCheck{Type: "exec", Command: []string{"true"}},
	}}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

func preflight(t *testing.T, rt *fakeruntime.Runtime, release string, rs ...resource.Resource) error {
	t.Helper()
	info, err := rt.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return Preflight(context.Background(), rt, info, release, []string{"docker"}, rs)
}

func TestPreflightOK(t *testing.T) {
	rs := []resource.Resource{res(resource.KindNetwork, "n"), webWithPort(freePort(t))}
	rs[0].Spec = &resource.NetworkSpec{Driver: "bridge"}
	if err := preflight(t, fakeruntime.New(), "web", rs...); err != nil {
		t.Fatal(err)
	}
}

func TestPreflightRuntimeAndCapabilities(t *testing.T) {
	rt := fakeruntime.New()
	rt.Capabilities = []string{ports.CapNetworkDriver("bridge")}
	info, _ := rt.Info(context.Background())
	vol := resource.Resource{Kind: resource.KindVolume, Metadata: resource.Metadata{Name: "d"},
		Spec: &resource.VolumeSpec{Driver: "nfs", MountOptions: []string{"nocopy"}}}
	err := Preflight(context.Background(), rt, info, "web", []string{"podman"}, []resource.Resource{vol, webWithPort(0)})
	for _, want := range []string{`does not support runtime "docker"`, "volume driver nfs", "mountOptions", "memory limit", "cpu limit", "exec healthcheck"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
}

func TestPreflightHostPorts(t *testing.T) {
	ctx := context.Background()
	port := freePort(t)
	rt := fakeruntime.New("img")
	other := ports.WorkloadRequest{
		Name: "other-web", Image: "img", Labels: map[string]string{ports.LabelRelease: "other"},
		Ports: []ports.PortBinding{{ContainerPort: 80, HostPort: port, HostIP: "0.0.0.0", Protocol: "tcp"}},
	}
	if err := rt.CreateWorkload(ctx, other); err != nil {
		t.Fatal(err)
	}
	if err := rt.StartWorkload(ctx, other.Name); err != nil {
		t.Fatal(err)
	}

	err := preflight(t, rt, "web", webWithPort(port))
	if !errors.Is(err, ports.ErrPortInUse) || !strings.Contains(err.Error(), "used by release other") {
		t.Fatalf("other release: %v", err)
	}
	// The release that owns the port may reuse it.
	if err := preflight(t, rt, "other", webWithPort(port)); err != nil {
		t.Fatalf("same release: %v", err)
	}

	// A host process holding a port.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	busy := l.Addr().(*net.TCPAddr).Port
	if err := preflight(t, fakeruntime.New(), "web", webWithPort(busy)); !errors.Is(err, ports.ErrPortInUse) || !strings.Contains(err.Error(), "another process") {
		t.Fatalf("host process: %v", err)
	}
}

func TestIPOverlap(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"127.0.0.1", "127.0.0.1", true}, {"0.0.0.0", "127.0.0.1", true}, {"::", "10.0.0.1", true},
		{"", "127.0.0.1", true}, {"127.0.0.1", "10.0.0.1", false},
	} {
		if got := ipOverlap(c.a, c.b); got != c.want {
			t.Errorf("ipOverlap(%q, %q) = %v", c.a, c.b, got)
		}
	}
}
