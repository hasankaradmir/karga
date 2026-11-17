// Package runtimetest is the contract every ports.Runtime implementation must pass (tech design §19.2).
package runtimetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hasankaradmir/karga/internal/ports"
)

// Config tells the suite which image and commands to use.
type Config struct {
	// Image must provide SleepCommand and HealthyCommand.
	Image          string
	SleepCommand   []string
	HealthyCommand []string
	// Wait bounds how long the suite waits for a workload to become healthy.
	Wait time.Duration
}

// Run exercises rt against the Runtime contract.
func Run(t *testing.T, rt ports.Runtime, cfg Config) {
	t.Helper()
	if cfg.Wait == 0 {
		cfg.Wait = 60 * time.Second
	}
	ctx := context.Background()
	release := "ct-" + randHex(4)
	labels := func(kind, name string) map[string]string {
		return map[string]string{
			ports.LabelManaged: "true", ports.LabelRelease: release,
			ports.LabelKind: kind, ports.LabelName: name, ports.LabelRevision: "1",
		}
	}
	name := func(n string) string { return release + "-" + n }

	t.Run("info", func(t *testing.T) {
		info, err := rt.Info(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if info.Name == "" || !info.Has(ports.CapHealthcheckExec) {
			t.Fatalf("unexpected info: %+v", info)
		}
	})

	t.Run("network", func(t *testing.T) {
		req := ports.NetworkRequest{Name: name("net"), Labels: labels("Network", "net"), Driver: "bridge"}
		must(t, rt.CreateNetwork(ctx, req))
		t.Cleanup(func() { _ = rt.DeleteNetwork(ctx, req.Name) })
		if err := rt.CreateNetwork(ctx, req); !errors.Is(err, ports.ErrConflict) {
			t.Fatalf("duplicate network: want ErrConflict, got %v", err)
		}
		assertListed(t, rt, release, ports.ObjectNetwork, req.Name, true)
		must(t, rt.DeleteNetwork(ctx, req.Name))
		must(t, rt.DeleteNetwork(ctx, req.Name))
		assertListed(t, rt, release, ports.ObjectNetwork, req.Name, false)
	})

	t.Run("volume", func(t *testing.T) {
		req := ports.VolumeRequest{Name: name("vol"), Labels: labels("Volume", "vol"), Driver: "local"}
		must(t, rt.CreateVolume(ctx, req))
		t.Cleanup(func() { _ = rt.DeleteVolume(ctx, req.Name) })
		if err := rt.CreateVolume(ctx, req); !errors.Is(err, ports.ErrConflict) {
			t.Fatalf("duplicate volume: want ErrConflict, got %v", err)
		}
		assertListed(t, rt, release, ports.ObjectVolume, req.Name, true)
		must(t, rt.DeleteVolume(ctx, req.Name))
		must(t, rt.DeleteVolume(ctx, req.Name))
	})

	t.Run("image never missing", func(t *testing.T) {
		if err := rt.EnsureImage(ctx, "karga.invalid/missing:0", "never"); !errors.Is(err, ports.ErrImageNotFound) {
			t.Fatalf("want ErrImageNotFound, got %v", err)
		}
	})

	t.Run("missing workload", func(t *testing.T) {
		if _, err := rt.GetWorkload(ctx, name("nope")); !errors.Is(err, ports.ErrNotFound) {
			t.Fatalf("GetWorkload: want ErrNotFound, got %v", err)
		}
		if err := rt.StartWorkload(ctx, name("nope")); !errors.Is(err, ports.ErrNotFound) {
			t.Fatalf("StartWorkload: want ErrNotFound, got %v", err)
		}
		must(t, rt.DeleteWorkload(ctx, name("nope")))
	})

	t.Run("workload lifecycle", func(t *testing.T) {
		must(t, rt.EnsureImage(ctx, cfg.Image, "if-not-present"))
		netReq := ports.NetworkRequest{Name: name("wnet"), Labels: labels("Network", "wnet"), Driver: "bridge"}
		must(t, rt.CreateNetwork(ctx, netReq))
		t.Cleanup(func() { _ = rt.DeleteNetwork(ctx, netReq.Name) })

		port := freePort(t)
		req := ports.WorkloadRequest{
			Name:     name("app"),
			Labels:   labels("Workload", "app"),
			Image:    cfg.Image,
			Command:  cfg.SleepCommand,
			Env:      map[string]string{"KARGA_TEST": "1"},
			Ports:    []ports.PortBinding{{ContainerPort: 8080, HostPort: port, HostIP: "127.0.0.1", Protocol: "tcp"}},
			Networks: []string{netReq.Name},
			Aliases:  []string{"app"},
			HealthCheck: &ports.HealthCheck{
				Command: cfg.HealthyCommand, Interval: time.Second, Timeout: time.Second, Retries: 3,
			},
			MemoryBytes:     64 << 20,
			MilliCPU:        500,
			Sysctls:         map[string]string{"net.ipv4.ip_unprivileged_port_start": "0"},
			RestartPolicy:   "never",
			CapDrop:         []string{"ALL"},
			CapAdd:          []string{"NET_BIND_SERVICE"},
			NoNewPrivileges: true,
		}
		req.Labels["io.karga.contract"] = "x"
		must(t, rt.CreateWorkload(ctx, req))
		t.Cleanup(func() { _ = rt.DeleteWorkload(ctx, req.Name) })
		if err := rt.CreateWorkload(ctx, req); !errors.Is(err, ports.ErrConflict) {
			t.Fatalf("duplicate workload: want ErrConflict, got %v", err)
		}
		st, err := rt.GetWorkload(ctx, req.Name)
		must(t, err)
		if st.State != ports.StateCreated {
			t.Fatalf("state after create: %q", st.State)
		}

		must(t, rt.StartWorkload(ctx, req.Name))
		st = waitFor(t, rt, req.Name, cfg.Wait, func(s ports.WorkloadStatus) bool {
			return s.State == ports.StateRunning && s.Health == ports.HealthHealthy
		})
		if st.Labels[ports.LabelRelease] != release || st.Labels[ports.LabelName] != "app" {
			t.Fatalf("labels: %v", st.Labels)
		}
		assertListed(t, rt, release, ports.ObjectWorkload, req.Name, true)
		var logBuf strings.Builder
		if err := rt.Logs(ctx, req.Name, 10, false, &logBuf); err != nil {
			t.Fatalf("logs: %v", err)
		}
		labeled, err := rt.ListLabeled(ctx, "io.karga.contract")
		must(t, err)
		if !slices.ContainsFunc(labeled, func(o ports.Object) bool { return o.Name == req.Name && o.Labels["io.karga.contract"] == "x" }) {
			t.Fatalf("ListLabeled does not return %s: %+v", req.Name, labeled)
		}

		pub, err := rt.PublishedPorts(ctx)
		must(t, err)
		if !slices.ContainsFunc(pub, func(p ports.PublishedPort) bool {
			return p.Port == port && p.Protocol == "tcp" && p.Release == release && p.Workload == req.Name
		}) {
			t.Fatalf("port %d not reported in %+v", port, pub)
		}

		// Rename keeps the container running; network membership can be changed while it runs.
		renamed := req.Name + "-next"
		t.Cleanup(func() { _ = rt.DeleteWorkload(ctx, renamed) })
		must(t, rt.RenameWorkload(ctx, req.Name, renamed))
		if _, err := rt.GetWorkload(ctx, req.Name); !errors.Is(err, ports.ErrNotFound) {
			t.Fatalf("old name after rename: want ErrNotFound, got %v", err)
		}
		if st, err := rt.GetWorkload(ctx, renamed); err != nil || st.State != ports.StateRunning {
			t.Fatalf("renamed workload: %+v %v", st, err)
		}
		must(t, rt.DisconnectNetwork(ctx, renamed, netReq.Name))
		must(t, rt.DisconnectNetwork(ctx, renamed, netReq.Name))
		must(t, rt.ConnectNetwork(ctx, renamed, netReq.Name, []string{"app"}))
		if err := rt.RenameWorkload(ctx, "ct-missing-workload", "x"); !errors.Is(err, ports.ErrNotFound) {
			t.Fatalf("rename missing: want ErrNotFound, got %v", err)
		}
		must(t, rt.RenameWorkload(ctx, renamed, req.Name))

		must(t, rt.StopWorkload(ctx, req.Name, 5*time.Second))
		if st, err := rt.GetWorkload(ctx, req.Name); err != nil || st.State != ports.StateExited {
			t.Fatalf("after stop: %+v, %v", st, err)
		}
		must(t, rt.DeleteWorkload(ctx, req.Name))
		if _, err := rt.GetWorkload(ctx, req.Name); !errors.Is(err, ports.ErrNotFound) {
			t.Fatalf("after delete: want ErrNotFound, got %v", err)
		}
		must(t, rt.DeleteWorkload(ctx, req.Name))

		req.Name = name("hostnet")
		hostNetworkCase(t, rt, cfg, req)
	})
}

// hostNetworkCase runs a workload in the host network namespace (the gateway's mode).
func hostNetworkCase(t *testing.T, rt ports.Runtime, cfg Config, req ports.WorkloadRequest) {
	ctx := context.Background()
	req.HostNetwork, req.Networks, req.Aliases, req.Ports, req.HealthCheck, req.Sysctls = true, nil, nil, nil, nil, nil
	must(t, rt.CreateWorkload(ctx, req))
	t.Cleanup(func() { _ = rt.DeleteWorkload(ctx, req.Name) })
	must(t, rt.StartWorkload(ctx, req.Name))
	waitFor(t, rt, req.Name, cfg.Wait, func(s ports.WorkloadStatus) bool { return s.State == ports.StateRunning })
	must(t, rt.DeleteWorkload(ctx, req.Name))
}

func waitFor(t *testing.T, rt ports.Runtime, name string, d time.Duration, ok func(ports.WorkloadStatus) bool) ports.WorkloadStatus {
	t.Helper()
	deadline := time.Now().Add(d)
	var last ports.WorkloadStatus
	for time.Now().Before(deadline) {
		st, err := rt.GetWorkload(context.Background(), name)
		must(t, err)
		if ok(st) {
			return st
		}
		last = st
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("workload %s did not reach the wanted state in %s, last: %+v", name, d, last)
	return last
}

func assertListed(t *testing.T, rt ports.Runtime, release, kind, name string, want bool) {
	t.Helper()
	objs, err := rt.ListManaged(context.Background(), release)
	must(t, err)
	got := slices.ContainsFunc(objs, func(o ports.Object) bool { return o.Kind == kind && o.Name == name })
	if got != want {
		t.Fatalf("ListManaged(%s) contains %s %s = %v, want %v (%+v)", release, kind, name, got, want, objs)
	}
}

// FreePort returns a TCP port that was free a moment ago on 127.0.0.1.
func FreePort(t *testing.T) int { return freePort(t) }

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
