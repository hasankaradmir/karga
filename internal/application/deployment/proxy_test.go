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
	old := proxySupported
	proxySupported = on
	t.Cleanup(func() { proxySupported = old })
}

// proxiedManifest is manifest() with a replace+proxy workload that publishes its port.
func proxiedManifest(port int) []resource.Resource {
	rs := manifest(port)
	w := rs[0].Spec.(*resource.WorkloadSpec)
	w.UpdateStrategy = &resource.UpdateStrategy{Type: resource.StrategyReplace, Proxy: true, DrainTimeout: "2s"}
	w.Mounts[0].ReadOnly = true
	return rs
}

func changeOf(t *testing.T, p Plan, kind string) Change {
	t.Helper()
	for _, c := range p.Changes {
		if c.Kind == kind {
			return c
		}
	}
	return Change{}
}

func TestProxyPlan(t *testing.T) {
	withProxySupport(t, true)

	// Fresh install: the proxy is created.
	p, err := Diff("cache", proxiedManifest(6379), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	px := changeOf(t, p, KindProxy)
	if px.Action != ActionCreate || px.RuntimeName != "cache-karga-proxy-redis" || px.Proxy.Ports[0].HostPort != 6379 {
		t.Fatalf("proxy create: %+v", px)
	}

	// Moving an existing workload behind the proxy recreates it; only the proxy flag differs.
	notProxied := proxiedManifest(6379)
	notProxied[0].Spec.(*resource.WorkloadSpec).UpdateStrategy.Proxy = false
	plain := appliedFrom(t, notProxied)
	p, err = Diff("cache", proxiedManifest(6379), plain, "")
	if err != nil {
		t.Fatal(err)
	}
	if w := changeOf(t, p, resource.KindWorkload); w.Action != ActionUpdate || w.Label() != "recreate: moving behind the proxy" || w.Fields[0].Path != "(proxy placement)" {
		t.Fatalf("enable: %+v", w)
	}

	// Already behind the proxy: an image change is a replace and the proxy is untouched.
	applied := appliedWithProxy(t, proxiedManifest(6379))
	v2 := proxiedManifest(6379)
	v2[0].Spec.(*resource.WorkloadSpec).Image = "redis:7.2"
	p, err = Diff("cache", v2, applied, "recreate")
	if err != nil {
		t.Fatal(err)
	}
	if w := changeOf(t, p, resource.KindWorkload); w.Label() != "replace" {
		t.Fatalf("proxied update must replace even with --strategy recreate: %+v", w)
	}
	if px := changeOf(t, p, KindProxy); px.Action != ActionUnchanged {
		t.Fatalf("proxy should be unchanged: %+v", px)
	}

	// A new host port recreates the proxy.
	p, err = Diff("cache", proxiedManifest(6380), applied, "")
	if err != nil {
		t.Fatal(err)
	}
	if px := changeOf(t, p, KindProxy); px.Action != ActionUpdate || px.Label() != "recreate: proxy config changed" {
		t.Fatalf("port change: %+v", px)
	}

	// Turning the proxy off deletes it and recreates the workload with its host port.
	p, err = Diff("cache", manifest(6379), applied, "")
	if err != nil {
		t.Fatal(err)
	}
	if px := changeOf(t, p, KindProxy); px.Action != ActionDelete {
		t.Fatalf("disable proxy: %+v", px)
	}
	if w := changeOf(t, p, resource.KindWorkload); w.Label() != "recreate: moving out from behind the proxy" {
		t.Fatalf("disable workload: %+v", w)
	}
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

func TestProxyFallbackReasons(t *testing.T) {
	label := func(mutate func(*resource.WorkloadSpec)) string {
		t.Helper()
		applied := appliedFrom(t, proxiedManifest(6379))
		v2 := proxiedManifest(6379)
		w := v2[0].Spec.(*resource.WorkloadSpec)
		w.Image = "redis:7.2"
		mutate(w)
		p, err := Diff("cache", v2, applied, "")
		if err != nil {
			t.Fatal(err)
		}
		if changeOf(t, p, KindProxy).Kind != "" {
			t.Fatal("no proxy expected when it cannot be used")
		}
		return changeOf(t, p, resource.KindWorkload).Label()
	}
	withProxySupport(t, false)
	if got := label(func(*resource.WorkloadSpec) {}); got != "recreate: proxy requires Linux" {
		t.Errorf("macOS: %q", got)
	}
	withProxySupport(t, true)
	for want, mutate := range map[string]func(*resource.WorkloadSpec){
		"recreate: proxy needs the workload on a network": func(w *resource.WorkloadSpec) { w.Networks = nil },
		"recreate: udp port 6379 is not proxied":          func(w *resource.WorkloadSpec) { w.Ports[0].Protocol = "udp" },
		"recreate: mounts volume data read-write":         func(w *resource.WorkloadSpec) { w.Mounts[0].ReadOnly = false },
	} {
		if got := label(mutate); !strings.HasPrefix(got, want) {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}
