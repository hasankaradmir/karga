//go:build integration

package integration

import (
	"context"
	"io"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hasankaradmir/karga/internal/application/deployment"
	"github.com/hasankaradmir/karga/internal/application/packages"
	"github.com/hasankaradmir/karga/internal/application/releases"
	"github.com/hasankaradmir/karga/internal/domain/release"
	"github.com/hasankaradmir/karga/internal/ports/runtimetest"
)

func requireLinux(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the proxy only runs on Linux (ADR-009)")
	}
}

// hostPinger requests http://127.0.0.1:port/ every 50ms from the host until stopped.
type hostPinger struct {
	ok, failed atomic.Int64
	mu         sync.Mutex
	bodies     map[string]int
	stop       chan struct{}
	done       sync.WaitGroup
}

func startHostPinger(port int) *hostPinger {
	p := &hostPinger{bodies: map[string]int{}, stop: make(chan struct{})}
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	url := "http://127.0.0.1:" + strconv.Itoa(port) + "/"
	p.done.Add(1)
	go func() {
		defer p.done.Done()
		for {
			select {
			case <-p.stop:
				return
			case <-time.After(50 * time.Millisecond):
			}
			resp, err := client.Get(url)
			if err != nil {
				p.failed.Add(1)
				continue
			}
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				p.failed.Add(1)
				continue
			}
			p.ok.Add(1)
			p.mu.Lock()
			p.bodies[strings.TrimSpace(string(b))]++
			p.mu.Unlock()
		}
	}()
	return p
}

func (p *hostPinger) halt() { close(p.stop); p.done.Wait() }

func (p *hostPinger) reset() { p.ok.Store(0); p.failed.Store(0) }

func TestProxyReplaceWithoutLoss(t *testing.T) {
	requireLinux(t)
	ctx := context.Background()
	name := releaseName("it-proxy")
	s, rt := service(t, name)
	port := runtimetest.FreePort(t)
	common := opts()
	common.Timeout = 30 * time.Second
	sets := func(extra ...string) packages.Options {
		return packages.Options{Sets: append([]string{startDelay, "publish=true", "proxy=true", "port=" + strconv.Itoa(port)}, extra...)}
	}

	res, err := s.Install(ctx, releases.InstallRequest{CommonOptions: common, Name: name, Package: webPkg, Values: sets()})
	if err != nil {
		t.Fatal(err)
	}
	var proxyCreated bool
	for _, c := range res.Plan.Changes {
		proxyCreated = proxyCreated || (c.Kind == deployment.KindProxy && c.Action == "create")
	}
	if !proxyCreated {
		t.Fatalf("no proxy in the plan: %+v", res.Plan.Changes)
	}
	pub, err := rt.PublishedPorts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pub {
		if p.Release == name && p.Workload != deployment.ProxyName(name, "web") {
			t.Fatalf("only the proxy may publish the host port: %+v", pub)
		}
	}

	host := startHostPinger(port)
	defer host.halt()
	net := startPinger(t, rt, name)
	time.Sleep(time.Second)

	if _, err := s.Upgrade(ctx, releases.UpgradeRequest{CommonOptions: common, Name: name, Package: webPkg, Values: sets("content=v2")}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	// A broken version never takes traffic.
	if _, err := s.Upgrade(ctx, releases.UpgradeRequest{CommonOptions: common, Name: name, Package: webPkg, Values: sets("content=v3", "healthy=false")}); err == nil {
		t.Fatal("unhealthy replace should fail")
	}
	time.Sleep(time.Second)

	netOut := logs(t, net)
	t.Logf("host: %d ok, %d failed, bodies %v; network: %d failed of %d", host.ok.Load(), host.failed.Load(), host.bodies,
		strings.Count(netOut, "FAIL"), strings.Count(netOut, "\n")/2)
	if host.failed.Load() != 0 || strings.Count(netOut, "FAIL") != 0 {
		t.Fatalf("requests failed behind the proxy:\n%s", netOut)
	}
	if host.bodies["v2"] == 0 || host.bodies["v3"] != 0 {
		t.Fatalf("unexpected content: %v", host.bodies)
	}
	if r := currentRelease(t, s, name); r.Status != release.StatusDeployed || r.CurrentRevision != 2 {
		t.Fatalf("release after failed replace: %+v", r)
	}

	rep, err := s.Status(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	var backend string
	for _, r := range rep.Resources {
		if r.Kind == deployment.KindProxy {
			backend = r.Backend
		}
	}
	if backend != deployment.BackendAlias("web", 2) {
		t.Fatalf("proxy backend: %q", backend)
	}

	// Restarting the proxy or the backend (which may change its IP) keeps routing.
	for _, c := range []string{deployment.ProxyName(name, "web"), name + "-web"} {
		if err := rt.StopWorkload(ctx, c, 2*time.Second); err != nil {
			t.Fatal(err)
		}
		if err := rt.StartWorkload(ctx, c); err != nil {
			t.Fatal(err)
		}
		host.reset()
		deadline := time.Now().Add(30 * time.Second)
		for host.ok.Load() == 0 && time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
		}
		if host.ok.Load() == 0 {
			t.Fatalf("no traffic after restarting %s", c)
		}
	}
}

func TestProxyEnableDisable(t *testing.T) {
	requireLinux(t)
	ctx := context.Background()
	name := releaseName("it-proxy-toggle")
	s, rt := service(t, name)
	port := runtimetest.FreePort(t)
	common := opts()
	common.Timeout = 30 * time.Second
	upgrade := func(proxy bool) *releases.Result {
		t.Helper()
		res, err := s.Upgrade(ctx, releases.UpgradeRequest{CommonOptions: common, Name: name, Package: webPkg, Install: true,
			Values: packages.Options{Sets: []string{"publish=true", "port=" + strconv.Itoa(port), "proxy=" + strconv.FormatBool(proxy)}}})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	publisher := func() string {
		t.Helper()
		pub, err := rt.PublishedPorts(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range pub {
			if p.Port == port {
				return p.Workload
			}
		}
		return ""
	}

	upgrade(false)
	if got := publisher(); got != name+"-web" {
		t.Fatalf("without proxy the workload publishes: %q", got)
	}
	res := upgrade(true)
	for _, c := range res.Plan.Changes {
		if c.Kind == "Workload" && c.Label() != "recreate: moving behind the proxy" {
			t.Fatalf("enable: %+v", c)
		}
	}
	if got := publisher(); got != deployment.ProxyName(name, "web") {
		t.Fatalf("with proxy the proxy publishes: %q", got)
	}
	upgrade(false)
	if got := publisher(); got != name+"-web" {
		t.Fatalf("after disabling the workload publishes again: %q", got)
	}
	if _, err := rt.GetWorkload(ctx, deployment.ProxyName(name, "web")); err == nil {
		t.Fatal("proxy still exists")
	}
}

func TestProxyLowPort(t *testing.T) {
	requireLinux(t)
	ctx := context.Background()
	name := releaseName("it-proxy-80")
	s, _ := service(t, name)
	port := runtimetest.FreePort(t)
	if _, err := s.Install(ctx, releases.InstallRequest{CommonOptions: opts(), Name: name, Package: webPkg,
		Values: packages.Options{Sets: []string{"publish=true", "proxy=true", "listenPort=80", "port=" + strconv.Itoa(port)}}}); err != nil {
		t.Fatal(err)
	}
	p := startHostPinger(port)
	time.Sleep(time.Second)
	p.halt()
	if p.ok.Load() == 0 {
		t.Fatalf("non-root proxy did not serve containerPort 80 (failed %d)", p.failed.Load())
	}
}
