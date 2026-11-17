package deployment

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hasankaradmir/karga/internal/adapters/fakeruntime"
	"github.com/hasankaradmir/karga/internal/domain/resource"
	"github.com/hasankaradmir/karga/internal/ports"
)

func httpSpec(port int, startPeriod string) *resource.WorkloadSpec {
	return &resource.WorkloadSpec{
		Ports: []resource.Port{{ContainerPort: port, Protocol: "tcp"}},
		HealthCheck: &resource.HealthCheck{Type: resource.HealthHTTP, Port: port, Path: "/health",
			Interval: "10ms", Timeout: "200ms", Retries: 2, StartPeriod: startPeriod},
	}
}

// probedWorkload is a running fake container whose gateway upstream is addr's port.
func probedWorkload(t *testing.T, rt *fakeruntime.Runtime, name string, containerPort int, addr string) {
	t.Helper()
	_, p, _ := net.SplitHostPort(addr)
	up, _ := strconv.Atoi(p)
	label, _ := json.Marshal([]GatewayPort{{ContainerPort: containerPort, Upstream: up}})
	if err := rt.CreateWorkload(context.Background(), ports.WorkloadRequest{Name: name, Image: "img",
		Labels: map[string]string{ports.LabelManaged: "true", LabelGateway: string(label)}}); err != nil {
		t.Fatal(err)
	}
	if err := rt.StartWorkload(context.Background(), name); err != nil {
		t.Fatal(err)
	}
}

func TestWaitReadyProbes(t *testing.T) {
	withProxySupport(t, true)
	var status atomic.Int32
	status.Store(http.StatusServiceUnavailable)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(int(status.Load()))
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	rt := fakeruntime.New("img")
	probedWorkload(t, rt, "w", 8080, addr)
	ctx := context.Background()

	// Failing after the start period: unhealthy once retries are used up.
	if _, err := waitReady(ctx, rt, []readyTarget{{"w", httpSpec(8080, "")}}, 5*time.Second); err == nil || !strings.Contains(err.Error(), "is unhealthy") {
		t.Fatalf("want unhealthy, got %v", err)
	}
	// Failures during the start period do not count; the server recovers within it.
	time.AfterFunc(150*time.Millisecond, func() { status.Store(http.StatusOK) })
	if notReady, err := waitReady(ctx, rt, []readyTarget{{"w", httpSpec(8080, "1s")}}, 5*time.Second); err != nil || len(notReady) != 0 {
		t.Fatalf("want ready, got %v %v", notReady, err)
	}
	if got := ProbeHealth(ctx, mustStatus(t, rt, "w"), httpSpec(8080, "")); got != ports.HealthHealthy {
		t.Fatalf("status health: %s", got)
	}

	// tcp: a closed port is unhealthy, an open one healthy.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tcp := &resource.WorkloadSpec{Ports: []resource.Port{{ContainerPort: 9000, Protocol: "tcp"}},
		HealthCheck: &resource.HealthCheck{Type: resource.HealthTCP, Port: 9000, Interval: "10ms", Retries: 1}}
	probedWorkload(t, rt, "t", 9000, l.Addr().String())
	if _, err := waitReady(ctx, rt, []readyTarget{{"t", tcp}}, 5*time.Second); err != nil {
		t.Fatalf("tcp open: %v", err)
	}
	_ = l.Close()
	if got := ProbeHealth(ctx, mustStatus(t, rt, "t"), tcp); got != ports.HealthUnhealthy {
		t.Fatalf("tcp closed: %s", got)
	}
}

func mustStatus(t *testing.T, rt *fakeruntime.Runtime, name string) ports.WorkloadStatus {
	t.Helper()
	st, err := rt.GetWorkload(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestProbeNeedsAReachablePort(t *testing.T) {
	withProxySupport(t, false)
	rs := manifest(0)
	w := rs[0].Spec.(*resource.WorkloadSpec)
	w.Ports[0].HostPort = 0
	w.HealthCheck = &resource.HealthCheck{Type: resource.HealthTCP, Port: 6379}
	if _, err := Diff("cache", rs, nil, ""); err == nil || !strings.Contains(err.Error(), "needs port 6379 published") {
		t.Fatalf("want unreachable error, got %v", err)
	}
	w.Ports[0].HostPort = 16379
	if _, err := Diff("cache", rs, nil, ""); err != nil {
		t.Fatalf("published port is reachable: %v", err)
	}
	// Behind the proxy Envoy checks the same path.
	if got := httpHealthPath(httpSpec(8080, ""), 8080); got != "/health" {
		t.Fatalf("envoy health path: %q", got)
	}
}
