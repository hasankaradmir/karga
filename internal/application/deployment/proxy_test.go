package deployment

import (
	"strings"
	"testing"

	"github.com/hasankaradmir/karga/internal/domain/release"
	"github.com/hasankaradmir/karga/internal/domain/resource"
)

// withProxySupport pretends to run on Linux (or not) for the duration of a test.
func withProxySupport(t *testing.T, on bool) {
	t.Helper()
	old := proxySupported.Load()
	proxySupported.Store(on)
	t.Cleanup(func() { proxySupported.Store(old) })
}

// replaceManifest is manifest() with a replaceable workload that publishes its port.
func replaceManifest(port int) []resource.Resource {
	rs := manifest(port)
	w := rs[0].Spec.(*resource.WorkloadSpec)
	w.UpdateStrategy = &resource.UpdateStrategy{Type: resource.StrategyReplace, DrainTimeout: "2s"}
	w.Mounts[0].ReadOnly = true
	return rs
}

// withWorkload adds a replaceable workload on network net listening on port.
func withWorkload(rs []resource.Resource, name string, port int) []resource.Resource {
	return append(rs, resource.Resource{Kind: resource.KindWorkload, Metadata: resource.Metadata{Name: name}, Spec: &resource.WorkloadSpec{
		Image: "app:1", PullPolicy: "if-not-present", RestartPolicy: "always", Networks: []string{"net"},
		Ports:          []resource.Port{{ContainerPort: port, Protocol: "tcp"}},
		HealthCheck:    &resource.HealthCheck{Type: "exec", Command: []string{"true"}},
		UpdateStrategy: &resource.UpdateStrategy{Type: resource.StrategyReplace},
	}})
}

func changeOf(t *testing.T, p Plan, kind, name string) Change {
	t.Helper()
	for _, c := range p.Changes {
		if c.Kind == kind && (name == "" || c.Name == name) {
			return c
		}
	}
	return Change{}
}

// appliedWithProxy is the applied state after installing rs, proxy rows included.
func appliedWithProxy(t *testing.T, rs []resource.Resource) []release.AppliedResource {
	t.Helper()
	p, err := Diff("cache", rs, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	var out []release.AppliedResource
	for _, c := range p.Changes {
		out = append(out, release.AppliedResource{Kind: c.Kind, Name: c.Name, RuntimeName: c.RuntimeName, Spec: c.Spec, Digest: c.Digest})
	}
	return out
}

func TestProxyPlan(t *testing.T) {
	withProxySupport(t, true)

	// Fresh install: one release proxy answering for the replace workload.
	p, err := Diff("cache", replaceManifest(6379), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	px := changeOf(t, p, KindProxy, "")
	if px.Action != ActionCreate || px.Name != ProxyResource || px.RuntimeName != "cache-karga-proxy" ||
		strings.Join(px.Proxy.Networks["net"], ",") != "redis" {
		t.Fatalf("proxy create: %+v %+v", px, px.Proxy)
	}

	// A workload installed without the proxy (e.g. by karga on macOS) moves behind it with a recreate.
	withProxySupport(t, false)
	plain := appliedFrom(t, replaceManifest(6379))
	withProxySupport(t, true)
	p, err = Diff("cache", replaceManifest(6379), plain, "")
	if err != nil {
		t.Fatal(err)
	}
	if w := changeOf(t, p, resource.KindWorkload, "redis"); w.Label() != "recreate: moving behind the proxy" || w.Fields[0].Path != "(proxy placement)" {
		t.Fatalf("enable: %+v", w)
	}

	// Behind the proxy an image change is a replace, even with --strategy recreate; the proxy stays.
	applied := appliedWithProxy(t, replaceManifest(6379))
	v2 := replaceManifest(6379)
	v2[0].Spec.(*resource.WorkloadSpec).Image = "redis:7.2"
	p, err = Diff("cache", v2, applied, "recreate")
	if err != nil {
		t.Fatal(err)
	}
	if w := changeOf(t, p, resource.KindWorkload, "redis"); w.Label() != "replace" {
		t.Fatalf("proxied update must replace: %+v", w)
	}
	if px := changeOf(t, p, KindProxy, ""); px.Action != ActionUnchanged {
		t.Fatalf("proxy should be unchanged: %+v", px)
	}

	// A second workload on the network reconnects the proxy with its alias.
	p, err = Diff("cache", withWorkload(replaceManifest(6379), "web", 8080), applied, "")
	if err != nil {
		t.Fatal(err)
	}
	if px := changeOf(t, p, KindProxy, ""); px.Action != ActionUpdate || px.Label() != "reconnect: net: brief interruption" {
		t.Fatalf("add workload: %+v", px)
	}

	// The same container port cannot be proxied twice: the later workload switches by DNS.
	both := appliedWithProxy(t, withWorkload(replaceManifest(6379), "web", 6379))
	v3 := withWorkload(replaceManifest(6379), "web", 6379)
	v3[len(v3)-1].Spec.(*resource.WorkloadSpec).Image = "app:2"
	p, err = Diff("cache", v3, both, "")
	if err != nil {
		t.Fatal(err)
	}
	if w := changeOf(t, p, resource.KindWorkload, "web"); w.Label() != "replace: not behind the proxy, containerPort 6379 already proxied for redis" {
		t.Fatalf("port clash: %+v", w)
	}

	// Back to recreate: the proxy goes and the workload takes its name back.
	p, err = Diff("cache", manifest(6379), applied, "")
	if err != nil {
		t.Fatal(err)
	}
	if px := changeOf(t, p, KindProxy, ""); px.Action != ActionDelete {
		t.Fatalf("disable proxy: %+v", px)
	}
	if w := changeOf(t, p, resource.KindWorkload, "redis"); w.Label() != "recreate: moving out from behind the proxy" {
		t.Fatalf("disable workload: %+v", w)
	}

	// A per-workload proxy of an earlier version is removed; its workload counts as proxied.
	old := append(appliedFrom(t, replaceManifest(6379)), release.AppliedResource{Kind: KindProxy, Name: "redis", RuntimeName: "cache-karga-proxy-redis", Digest: "sha256:old"})
	p, err = Diff("cache", replaceManifest(6379), old, "")
	if err != nil {
		t.Fatal(err)
	}
	if c := changeOf(t, p, KindProxy, "redis"); c.Action != ActionDelete || c.RuntimeName != "cache-karga-proxy-redis" {
		t.Fatalf("old proxy: %+v", c)
	}
}

func TestProxyFallbackReasons(t *testing.T) {
	label := func(mutate func(*resource.WorkloadSpec)) string {
		t.Helper()
		v2 := replaceManifest(6379)
		w := v2[0].Spec.(*resource.WorkloadSpec)
		mutate(w)
		applied := appliedFrom(t, v2)
		w.Image = "redis:7.2"
		p, err := Diff("cache", v2, applied, "")
		if err != nil {
			t.Fatal(err)
		}
		if changeOf(t, p, KindProxy, "").Kind != "" {
			t.Fatal("no proxy expected when it cannot be used")
		}
		return changeOf(t, p, resource.KindWorkload, "redis").Label()
	}
	withProxySupport(t, false)
	if got := label(func(*resource.WorkloadSpec) {}); got != "recreate: publishes hostPort 6379/tcp without the gateway (Linux, tcp)" {
		t.Errorf("macOS: %q", got)
	}
	withProxySupport(t, true)
	for want, mutate := range map[string]func(*resource.WorkloadSpec){
		"replace: not behind the proxy, not on a network": func(w *resource.WorkloadSpec) { w.Networks = nil },
		"recreate: publishes hostPort 6379/udp":           func(w *resource.WorkloadSpec) { w.Ports[0].Protocol = "udp" },
		"replace: not behind the proxy, udp port 6379":    func(w *resource.WorkloadSpec) { w.Ports[0].Protocol, w.Ports[0].HostPort = "udp", 0 },
		"recreate: mounts volume data read-write":         func(w *resource.WorkloadSpec) { w.Mounts[0].ReadOnly = false },
	} {
		if got := label(mutate); !strings.HasPrefix(got, want) {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

func TestRoutesNeedTheGateway(t *testing.T) {
	withProxySupport(t, false)
	rs := manifest(0)
	rs[0].Spec.(*resource.WorkloadSpec).Ports[0].Routes = []resource.Route{{Host: "a.local", Path: "/", HostPort: 80, HostIP: "127.0.0.1"}}
	if _, err := Diff("cache", rs, nil, ""); err == nil || !strings.Contains(err.Error(), "routes need the gateway") {
		t.Fatalf("want routes error, got %v", err)
	}
}
