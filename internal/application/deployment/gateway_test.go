package deployment

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hasankaradmir/karga/internal/adapters/fakeruntime"
	"github.com/hasankaradmir/karga/internal/domain/resource"
	"github.com/hasankaradmir/karga/internal/ports"
)

func tlsRoute(host, secret string) resource.Route {
	return resource.Route{Host: host, Path: "/", HostPort: 443, HostIP: "127.0.0.1", TLS: &resource.RouteTLS{Secret: secret}}
}

func plainRoute(host string) resource.Route {
	return resource.Route{Host: host, Path: "/", HostPort: 80, HostIP: "127.0.0.1"}
}

func TestGatewayConfigTLSAndRedirects(t *testing.T) {
	targets := []gatewayTarget{
		{Release: "a", Workload: "web", Ports: []GatewayPort{{ContainerPort: 80, Upstream: 4001,
			Routes: []resource.Route{tlsRoute("secure.example", "cert"), plainRoute("plain.example")}}}},
		{Release: "b", Workload: "web", Ports: []GatewayPort{{ContainerPort: 80, Upstream: 4002,
			Routes: []resource.Route{tlsRoute("both.example", "cert"), plainRoute("both.example")}}}},
	}
	cfg := gatewayConfig(targets)
	tls, plain := -1, -1
	for i, l := range cfg.HTTP {
		switch l.Port {
		case 443:
			tls = i
		case 80:
			plain = i
		}
	}
	if tls < 0 || plain < 0 {
		t.Fatalf("listeners: %+v", cfg.HTTP)
	}
	if c := cfg.HTTP[tls].Certs; len(c) != 2 || !strings.HasPrefix(c["secure.example"].Chain, "/etc/karga-proxy/certs/a_443_secure.example_") {
		t.Fatalf("certs: %+v", c)
	}
	// secure.example gets a redirect; both.example is served plainly on 80, so it does not.
	if r := cfg.HTTP[plain].Redirects; !slices.Equal(r, []string{"secure.example"}) || cfg.HTTP[plain].RedirectPort != 443 {
		t.Fatalf("redirects: %v %d", r, cfg.HTTP[plain].RedirectPort)
	}
}

func TestSyncCertsCopiesAndPrunes(t *testing.T) {
	src := t.TempDir()
	for k, v := range map[string]string{resource.TLSCertKey: "CERT", resource.TLSKeyKey: "KEY"} {
		if err := os.WriteFile(filepath.Join(src, k), []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	targets := []gatewayTarget{{Release: "a", Workload: "web", Ports: []GatewayPort{{ContainerPort: 80,
		Routes: []resource.Route{tlsRoute("secure.example", "cert")}, CertDirs: map[string]string{"cert": src}}}}}
	if err := syncCerts(dir, targets, true); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "certs", certName("a", 443, "secure.example", src)+".key")
	if b, err := os.ReadFile(key); err != nil || string(b) != "KEY" {
		t.Fatalf("key: %q %v", b, err)
	}
	if info, _ := os.Stat(key); info.Mode().Perm() != 0o600 {
		t.Fatalf("key mode: %v", info.Mode())
	}
	if err := syncCerts(dir, nil, true); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "certs")); len(entries) != 0 {
		t.Fatalf("unused certificates kept: %v", entries)
	}
}

func TestCheckGatewayTLSConflicts(t *testing.T) {
	withProxySupport(t, true)
	ctx := context.Background()
	rt := fakeruntime.New("img")
	label, _ := json.Marshal([]GatewayPort{{ContainerPort: 80, Upstream: 4001, Routes: []resource.Route{tlsRoute("secure.example", "cert")}}})
	if err := rt.CreateWorkload(ctx, ports.WorkloadRequest{Name: "other-web", Image: "img", Labels: map[string]string{
		ports.LabelManaged: "true", ports.LabelRelease: "other", ports.LabelName: "web", LabelGateway: string(label)}}); err != nil {
		t.Fatal(err)
	}
	check := func(r resource.Route) string {
		w := resource.Resource{Kind: resource.KindWorkload, Metadata: resource.Metadata{Name: "web"}, Spec: &resource.WorkloadSpec{
			Ports: []resource.Port{{ContainerPort: 80, Protocol: "tcp", Routes: []resource.Route{r}}}}}
		return errorsText(checkGateway(ctx, rt, "mine", []resource.Resource{w}))
	}
	if got := check(resource.Route{Host: "x.example", Path: "/", HostPort: 443, HostIP: "127.0.0.1"}); !strings.Contains(got, "serves HTTPS on this port") {
		t.Errorf("plain on a TLS port: %q", got)
	}
	if got := check(resource.Route{Host: "secure.example", Path: "/api", HostPort: 443, HostIP: "127.0.0.1", TLS: &resource.RouteTLS{Secret: "mycert"}}); !strings.Contains(got, "another certificate") {
		t.Errorf("second certificate for a host: %q", got)
	}
}

func errorsText(errs []error) string {
	var s []string
	for _, e := range errs {
		s = append(s, e.Error())
	}
	return strings.Join(s, "; ")
}
