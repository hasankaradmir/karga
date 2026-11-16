package deployment

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hasankaradmir/karga/internal/adapters/fakeruntime"
	"github.com/hasankaradmir/karga/internal/domain/release"
	"github.com/hasankaradmir/karga/internal/domain/resource"
	"github.com/hasankaradmir/karga/internal/infrastructure/envoy"
	"github.com/hasankaradmir/karga/internal/ports"
)

// fakeProxies writes real config files but fakes the admin API. Calls go into the runtime's call
// log, so ordering against container operations can be checked in one list:
// "Write:proxy|gateway:<endpoints>", "WaitReady:<kind>:<endpoints>", "WaitDrained:<kind>".
type fakeProxies struct {
	rt       *fakeruntime.Runtime
	home     string
	notReady map[string]bool // by kind
}

func (f *fakeProxies) kind(dir string) string {
	if dir == GatewayDir(f.home) {
		return "gateway"
	}
	return "proxy"
}

func (f *fakeProxies) log(s string) { f.rt.Calls = append(f.rt.Calls, s) }

func endpoints(m map[string]string) string {
	var out []string
	for _, k := range slices.Sorted(maps.Keys(m)) {
		out = append(out, m[k])
	}
	return strings.Join(out, ",")
}

func (f *fakeProxies) Write(dir, node string, mode int, cfg envoy.Config) error {
	if err := envoy.Write(dir, node, mode, cfg); err != nil {
		return err
	}
	eps, _ := envoy.Endpoints(dir)
	f.log("Write:" + f.kind(dir) + ":" + endpoints(eps))
	return nil
}

func (f *fakeProxies) WaitReady(_ context.Context, dir string, want map[string]string, _ time.Duration) error {
	f.log("WaitReady:" + f.kind(dir) + ":" + endpoints(want))
	if f.notReady[f.kind(dir)] {
		return errors.New(f.kind(dir) + " not ready")
	}
	return nil
}

func (f *fakeProxies) WaitDrained(_ context.Context, dir string, _ []string, _ time.Duration) (bool, error) {
	f.log("WaitDrained:" + f.kind(dir))
	return true, nil
}

func newProxyRequest(t *testing.T) (*request, *fakeProxies) {
	t.Helper()
	withProxySupport(t, true)
	r := newRequest(t, freePort(t))
	r.req.Resources = replaceManifest(r.req.Resources[0].Spec.(*resource.WorkloadSpec).Ports[0].HostPort)
	px := &fakeProxies{rt: r.rt, home: r.req.Home, notReady: map[string]bool{}}
	r.req.Proxies, r.req.ProxyUser = px, "1000:1000"
	return r, px
}

// upgrade runs revision rev of the replace manifest with a new image.
func (r *request) upgrade(rev int) error {
	r.rt.Calls = nil
	r.req.Revision = rev
	port := r.req.Resources[0].Spec.(*resource.WorkloadSpec).Ports[0].HostPort
	r.req.Resources = replaceManifest(port)
	r.req.Resources[0].Spec.(*resource.WorkloadSpec).Image = "redis:7.2"
	return r.run()
}

func before(t *testing.T, calls []string, first, second string) {
	t.Helper()
	a, b := callIndex(calls, first), callIndex(calls, second)
	if a < 0 || b < 0 || a > b {
		t.Fatalf("%s must come before %s:\n%s", first, second, strings.Join(calls, "\n"))
	}
}

func upstream(t *testing.T, w *fakeruntime.Workload) string {
	t.Helper()
	gp := parseGatewayLabel(w.Request.Labels)
	if len(gp) != 1 {
		t.Fatalf("gateway label: %v", w.Request.Labels)
	}
	return "127.0.0.1:" + strconv.Itoa(gp[0].Upstream)
}

func TestProxyAndGatewayInstall(t *testing.T) {
	r, _ := newProxyRequest(t)
	port := r.req.Resources[0].Spec.(*resource.WorkloadSpec).Ports[0].HostPort
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	w := r.rt.Workloads["cache-redis"]
	if !slices.Equal(w.Request.Aliases, []string{"karga-redis-r1"}) {
		t.Fatalf("proxied workload must carry the backend alias: %+v", w.Request)
	}
	if p := w.Request.Ports[0]; p.HostIP != "127.0.0.1" || p.HostPort == port || p.HostPort == 0 {
		t.Fatalf("workload must publish only a loopback upstream: %+v", p)
	}
	gp := parseGatewayLabel(w.Request.Labels)
	if len(gp) != 1 || gp[0].HostPort != port || gp[0].Upstream != w.Request.Ports[0].HostPort {
		t.Fatalf("gateway label: %+v", gp)
	}

	pr := r.rt.Workloads["cache-karga-proxy"].Request
	if !slices.Equal(pr.Aliases, []string{"redis"}) || !slices.Equal(pr.Networks, []string{"cache-net"}) || pr.User != "1000:1000" ||
		len(pr.Ports) != 0 || pr.Sysctls["net.ipv4.ip_unprivileged_port_start"] != "0" || pr.Mounts[0].Target != envoy.MountPath {
		t.Fatalf("proxy request: %+v", pr)
	}
	gw := r.rt.Workloads[GatewayName].Request
	if !gw.HostNetwork || gw.User != "0:0" || !slices.Equal(gw.CapDrop, []string{"ALL"}) || !gw.NoNewPrivileges || gw.Labels[ports.LabelRelease] != "" {
		t.Fatalf("gateway request: %+v", gw)
	}

	c := r.rt.Calls
	before(t, c, "StartWorkload:cache-redis", "Write:proxy:karga-redis-r1:6379")
	before(t, c, "Write:proxy:karga-redis-r1:6379", "CreateWorkload:cache-karga-proxy")
	before(t, c, "StartWorkload:cache-karga-proxy", "WaitReady:proxy:karga-redis-r1")
	before(t, c, "WaitReady:proxy:karga-redis-r1", "Write:gateway:"+upstream(t, w))
	before(t, c, "Write:gateway:", "CreateWorkload:"+GatewayName)
	before(t, c, "StartWorkload:"+GatewayName, "WaitReady:gateway:"+upstream(t, w))
	cfgEps, _ := envoy.Endpoints(GatewayDir(r.req.Home))
	if cfgEps[gatewayCluster("cache", "redis", 6379)] != upstream(t, w) {
		t.Fatalf("gateway endpoints: %v", cfgEps)
	}
}

func TestProxyAndGatewayReplace(t *testing.T) {
	r, _ := newProxyRequest(t)
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	if err := r.upgrade(2); err != nil {
		t.Fatal(err)
	}
	c := r.rt.Calls
	next := r.rt.Workloads["cache-redis"] // renamed from -next
	for _, step := range [][2]string{
		{"StartWorkload:cache-redis-next", "Write:proxy:karga-redis-r2"},
		{"Write:proxy:karga-redis-r2", "WaitReady:proxy:karga-redis-r2"},
		{"WaitReady:proxy:karga-redis-r2", "Write:gateway:" + upstream(t, next)},
		{"Write:gateway:" + upstream(t, next), "WaitReady:gateway:" + upstream(t, next)},
		{"WaitReady:gateway:", "WaitDrained:proxy"},
		{"WaitDrained:proxy", "WaitDrained:gateway"},
		{"WaitDrained:gateway", "StopWorkload:cache-redis"},
		{"DeleteWorkload:cache-redis", "RenameWorkload:cache-redis-next->cache-redis"},
	} {
		before(t, c, step[0], step[1])
	}
	if slices.ContainsFunc(c, func(s string) bool {
		return strings.HasSuffix(s, ":cache-karga-proxy") || strings.HasSuffix(s, ":"+GatewayName)
	}) {
		t.Fatalf("a replace must not touch the proxy or gateway containers: %v", c)
	}
	if w := next.Request; w.Image != "redis:7.2" || !slices.Equal(w.Aliases, []string{"karga-redis-r2"}) {
		t.Fatalf("after replace: %+v", w)
	}
}

func TestReplaceRevertsWhenProxyNotReady(t *testing.T) {
	r, px := newProxyRequest(t)
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	old := upstream(t, r.rt.Workloads["cache-redis"])
	px.notReady["proxy"] = true
	if err := r.upgrade(2); err == nil {
		t.Fatal("expected failure")
	}
	before(t, r.rt.Calls, "Write:proxy:karga-redis-r2", "Write:proxy:karga-redis-r1")
	if slices.Contains(r.rt.Calls, "StopWorkload:cache-redis") || slices.ContainsFunc(r.rt.Calls, func(s string) bool { return strings.HasPrefix(s, "WaitReady:gateway") }) {
		t.Fatalf("old container stopped or gateway switched although the proxy never got ready: %v", r.rt.Calls)
	}
	if eps, _ := envoy.Endpoints(ProxyDir(r.req.Home, "cache")); eps["redis-p6379"] != "karga-redis-r1:6379" {
		t.Fatalf("old backend not restored: %v", eps)
	}
	if eps, _ := envoy.Endpoints(GatewayDir(r.req.Home)); eps[gatewayCluster("cache", "redis", 6379)] != old {
		t.Fatalf("gateway moved: %v", eps)
	}
	if n := r.rt.Workloads["cache-redis-next"]; n == nil || n.State != ports.StateExited {
		t.Fatalf("failed -next should be stopped and kept: %+v", n)
	}
}

func TestReplaceRevertsWhenGatewayNotReady(t *testing.T) {
	r, px := newProxyRequest(t)
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	old := upstream(t, r.rt.Workloads["cache-redis"])
	px.notReady["gateway"] = true
	if err := r.upgrade(2); err == nil {
		t.Fatal("expected failure")
	}
	if slices.Contains(r.rt.Calls, "StopWorkload:cache-redis") {
		t.Fatal("old container stopped although the gateway never got ready")
	}
	if eps, _ := envoy.Endpoints(GatewayDir(r.req.Home)); eps[gatewayCluster("cache", "redis", 6379)] != old {
		t.Fatalf("old upstream not restored: %v", eps)
	}
	if eps, _ := envoy.Endpoints(ProxyDir(r.req.Home, "cache")); eps["redis-p6379"] != "karga-redis-r1:6379" {
		t.Fatalf("proxy not restored: %v", eps)
	}
}

func TestGatewayGoesWithTheLastRelease(t *testing.T) {
	r, _ := newProxyRequest(t)
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	if err := Delete(context.Background(), r.rt, r.req.Home, "cache", false); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.rt.Workloads[GatewayName]; ok {
		t.Fatal("gateway still exists")
	}
	if eps, _ := envoy.Endpoints(GatewayDir(r.req.Home)); len(eps) != 0 {
		t.Fatalf("gateway config left: %v", eps)
	}
}

func TestReconcileKeepsWhatTheProxyRoutesTo(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	rt := fakeruntime.New("img")
	create := func(name, rev string) {
		t.Helper()
		lbl := map[string]string{ports.LabelManaged: "true", ports.LabelRelease: "web", ports.LabelName: "app", ports.LabelRevision: rev}
		if err := rt.CreateWorkload(ctx, ports.WorkloadRequest{Name: name, Image: "img", Labels: lbl}); err != nil {
			t.Fatal(err)
		}
	}
	applied := []release.AppliedResource{{Kind: resource.KindWorkload, Name: "app", RuntimeName: "web-app", Digest: "sha256:x"}}
	// Interrupted after the proxy switched to revision 2 but before the old container went away.
	create("web-app", "1")
	create("web-app-next", "2")
	cfg := envoy.Config{Clusters: []envoy.Cluster{{Name: "app-p8080", Host: BackendAlias("app", 2), Port: 8080, DNS: true}}}
	if err := envoy.Write(ProxyDir(home, "web"), "web", 0o600, cfg); err != nil {
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
