package deployment

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hasankaradmir/karga/internal/adapters/fakeruntime"
	"github.com/hasankaradmir/karga/internal/domain/release"
	"github.com/hasankaradmir/karga/internal/domain/resource"
	"github.com/hasankaradmir/karga/internal/infrastructure/envoy"
	"github.com/hasankaradmir/karga/internal/ports"
)

// fakeProxies records proxy control calls into the runtime's call log, so ordering against
// container operations can be checked in one list.
type fakeProxies struct {
	rt       *fakeruntime.Runtime
	backends map[string]string
	notReady bool
}

func (f *fakeProxies) log(s string) { f.rt.Calls = append(f.rt.Calls, s) }

func (f *fakeProxies) Configure(dir, _ string, _ []int, backend string) error {
	f.log("Proxy.Configure:" + backend)
	f.backends[dir] = backend
	return nil
}

func (f *fakeProxies) SetBackend(dir string, _ []int, backend string) error {
	f.log("Proxy.SetBackend:" + backend)
	f.backends[dir] = backend
	return nil
}

func (f *fakeProxies) Backend(dir string) (string, error) { return f.backends[dir], nil }

func (f *fakeProxies) WaitReady(_ context.Context, _ string, _ []int, backend string, _ time.Duration) error {
	f.log("Proxy.WaitReady:" + backend)
	if f.notReady {
		return errors.New("proxy not ready")
	}
	return nil
}

func (f *fakeProxies) WaitDrained(context.Context, string, []int, time.Duration) (bool, error) {
	f.log("Proxy.WaitDrained")
	return true, nil
}

func newProxyRequest(t *testing.T) (*request, *fakeProxies) {
	t.Helper()
	withProxySupport(t, true)
	r := newRequest(t, freePort(t))
	r.req.Resources = proxiedManifest(r.req.Resources[0].Spec.(*resource.WorkloadSpec).Ports[0].HostPort)
	px := &fakeProxies{rt: r.rt, backends: map[string]string{}}
	r.req.Proxies, r.req.ProxyUser = px, "1000:1000"
	return r, px
}

func before(t *testing.T, calls []string, first, second string) {
	t.Helper()
	a, b := callIndex(calls, first), callIndex(calls, second)
	if a < 0 || b < 0 || a > b {
		t.Fatalf("%s must come before %s:\n%s", first, second, strings.Join(calls, "\n"))
	}
}

func TestProxyInstall(t *testing.T) {
	r, px := newProxyRequest(t)
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	w := r.rt.Workloads["cache-redis"].Request
	if w.Ports[0].HostPort != 0 || !slices.Equal(w.Aliases, []string{"karga-redis-r1"}) {
		t.Fatalf("proxied workload must not publish and must carry the backend alias: %+v", w)
	}
	p, ok := r.rt.Workloads["cache-karga-proxy-redis"]
	if !ok {
		t.Fatalf("proxy not created: %v", r.rt.Calls)
	}
	pr := p.Request
	if pr.Ports[0].HostPort == 0 || !slices.Equal(pr.Aliases, []string{"redis"}) || pr.User != "1000:1000" ||
		pr.Sysctls["net.ipv4.ip_unprivileged_port_start"] != "0" || pr.Mounts[0].Target != "/etc/karga-proxy" {
		t.Fatalf("proxy request: %+v", pr)
	}
	before(t, r.rt.Calls, "StartWorkload:cache-redis", "Proxy.Configure:karga-redis-r1")
	before(t, r.rt.Calls, "Proxy.Configure:karga-redis-r1", "CreateWorkload:cache-karga-proxy-redis")
	if px.backends[ProxyDir(r.req.Home, "cache", "redis")] != "karga-redis-r1" {
		t.Fatalf("backends: %v", px.backends)
	}
}

func TestProxyReplace(t *testing.T) {
	r, _ := newProxyRequest(t)
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	r.rt.Calls = nil
	r.req.Revision = 2
	r.req.Resources = proxiedManifest(r.req.Resources[0].Spec.(*resource.WorkloadSpec).Ports[0].HostPort)
	r.req.Resources[0].Spec.(*resource.WorkloadSpec).Image = "redis:7.2"
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	c := r.rt.Calls
	for _, step := range [][2]string{
		{"StartWorkload:cache-redis-next", "Proxy.SetBackend:karga-redis-r2"},
		{"Proxy.SetBackend:karga-redis-r2", "Proxy.WaitReady:karga-redis-r2"},
		{"Proxy.WaitReady:karga-redis-r2", "Proxy.WaitDrained"},
		{"Proxy.WaitDrained", "StopWorkload:cache-redis"},
		{"DeleteWorkload:cache-redis", "RenameWorkload:cache-redis-next->cache-redis"},
	} {
		before(t, c, step[0], step[1])
	}
	if slices.ContainsFunc(c, func(s string) bool { return strings.Contains(s, "karga-proxy-redis") }) {
		t.Fatalf("the proxy container must not be touched by a replace: %v", c)
	}
	if w := r.rt.Workloads["cache-redis"]; w.Request.Image != "redis:7.2" || !slices.Equal(w.Request.Aliases, []string{"karga-redis-r2"}) {
		t.Fatalf("after replace: %+v", w.Request)
	}
}

func TestProxyReplaceRevertsWhenProxyNotReady(t *testing.T) {
	r, px := newProxyRequest(t)
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	r.rt.Calls, px.notReady = nil, true
	r.req.Revision = 2
	r.req.Resources = proxiedManifest(r.req.Resources[0].Spec.(*resource.WorkloadSpec).Ports[0].HostPort)
	r.req.Resources[0].Spec.(*resource.WorkloadSpec).Image = "redis:7.2"
	if err := r.run(); err == nil {
		t.Fatal("expected failure")
	}
	before(t, r.rt.Calls, "Proxy.SetBackend:karga-redis-r2", "Proxy.SetBackend:karga-redis-r1")
	if slices.Contains(r.rt.Calls, "StopWorkload:cache-redis") {
		t.Fatal("old container stopped although the proxy never got ready")
	}
	if px.backends[ProxyDir(r.req.Home, "cache", "redis")] != "karga-redis-r1" {
		t.Fatal("old backend not restored")
	}
	if n := r.rt.Workloads["cache-redis-next"]; n == nil || n.State != ports.StateExited {
		t.Fatalf("failed -next should be stopped and kept: %+v", n)
	}
}

func TestProxyEnableAndDisableOrdering(t *testing.T) {
	withProxySupport(t, true)
	r := newRequest(t, freePort(t))
	port := r.req.Resources[0].Spec.(*resource.WorkloadSpec).Ports[0].HostPort
	px := &fakeProxies{rt: r.rt, backends: map[string]string{}}
	r.req.Proxies = px
	if err := r.run(); err != nil {
		t.Fatal(err)
	}

	// Enable: the workload holding the host port goes before the proxy takes it.
	r.rt.Calls = nil
	r.req.Revision, r.req.Resources = 2, proxiedManifest(port)
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	before(t, r.rt.Calls, "DeleteWorkload:cache-redis", "CreateWorkload:cache-karga-proxy-redis")

	// Disable: the proxy goes before the workload takes the host port back.
	r.rt.Calls = nil
	r.req.Revision, r.req.Resources = 3, manifest(port)
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	before(t, r.rt.Calls, "DeleteWorkload:cache-karga-proxy-redis", "CreateWorkload:cache-redis")
	if w := r.rt.Workloads["cache-redis"].Request; w.Ports[0].HostPort != port {
		t.Fatalf("host port not back on the workload: %+v", w)
	}
}

func TestReconcileKeepsWhatTheProxyRoutesTo(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	rt := fakeruntime.New("img")
	create := func(name, rev string) {
		t.Helper()
		lbl := map[string]string{ports.LabelManaged: "true", ports.LabelRelease: "web", ports.LabelRevision: rev}
		if err := rt.CreateWorkload(ctx, ports.WorkloadRequest{Name: name, Image: "img", Labels: lbl}); err != nil {
			t.Fatal(err)
		}
	}
	applied := []release.AppliedResource{
		{Kind: resource.KindWorkload, Name: "app", RuntimeName: "web-app", Digest: "sha256:x"},
		{Kind: KindProxy, Name: "app", RuntimeName: "web-karga-proxy-app", Digest: "sha256:p"},
	}
	// Interrupted after the proxy switched to revision 2 but before the old container went away.
	create("web-app", "1")
	create("web-app-next", "2")
	if err := envoy.WriteAll(ProxyDir(home, "web", "app"), "app", []int{8080}, BackendAlias("app", 2)); err != nil {
		t.Fatal(err)
	}
	repaired, err := Reconcile(ctx, rt, home, "web", applied)
	if err != nil {
		t.Fatal(err)
	}
	w, ok := rt.Workloads["web-app"]
	if !ok || w.Request.Labels[ports.LabelRevision] != "2" || rt.Workloads["web-app-next"] != nil || len(repaired) != 1 {
		t.Fatalf("kept the wrong container: %v %+v", repaired, rt.Workloads)
	}
}
