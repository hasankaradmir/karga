package envoy

import (
	"context"
	"flag"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite golden files")

var sample = Config{
	TCP: []TCPListener{{Address: "127.0.0.1", Port: 5432, Cluster: "db_pg_p5432"}},
	HTTP: []HTTPListener{{Address: "0.0.0.0", Port: 80, Routes: []HTTPRoute{
		{Host: "app.local", Path: "/", Cluster: "app_web_p3000"},
		{Host: "app.local", Path: "/api", Cluster: "app_api_p8080"},
		{Host: "b.local", Path: "/", Cluster: "b_web_p3000"},
	}}},
	Clusters: []Cluster{
		{Name: "db_pg_p5432", Host: "127.0.0.1", Port: 41001},
		{Name: "web-p8080", Host: "karga-web-r2", Port: 8080, DNS: true},
	},
}

func TestConfigGolden(t *testing.T) {
	boot, err := Bootstrap("web", 0o600)
	if err != nil {
		t.Fatal(err)
	}
	lds, err := sample.Listeners()
	if err != nil {
		t.Fatal(err)
	}
	cds, err := sample.ClustersJSON()
	if err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string][]byte{"envoy.json": boot, "lds.json": lds, "cds.json": cds} {
		golden := filepath.Join("testdata", name)
		if *update {
			if err := os.MkdirAll("testdata", 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(golden, got, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		want, err := os.ReadFile(golden)
		if err != nil {
			t.Fatalf("%v (run with -update)", err)
		}
		if string(got) != string(want) {
			t.Errorf("%s differs from golden:\n%s", name, got)
		}
	}
}

func TestWriteAndEndpoints(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "proxy")
	if e, err := Endpoints(dir); err != nil || len(e) != 0 {
		t.Fatalf("no config: %v %v", e, err)
	}
	if err := Write(dir, "web", 0o600, sample); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode: %v %v", info.Mode(), err)
	}
	before, _ := os.Stat(filepath.Join(dir, listenersFile))
	next := sample
	next.Clusters = []Cluster{{Name: "db_pg_p5432", Host: "127.0.0.1", Port: 41002}}
	if err := Write(dir, "web", 0o600, next); err != nil {
		t.Fatal(err)
	}
	if e, err := Endpoints(dir); err != nil || len(e) != 1 || e["db_pg_p5432"] != "127.0.0.1:41002" {
		t.Fatalf("endpoints: %v %v", e, err)
	}
	if after, _ := os.Stat(filepath.Join(dir, listenersFile)); !os.SameFile(before, after) {
		t.Fatal("unchanged listeners were rewritten")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 3 {
		t.Fatalf("leftover temp files: %v", entries)
	}
}

// fakeAdmin serves canned admin responses on the unix socket of a proxy directory.
func fakeAdmin(t *testing.T, clusters, stats func() string) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "envoy-admin-") // short path: unix socket paths are limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	l, err := net.Listen("unix", filepath.Join(dir, adminSocket))
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/clusters":
			_, _ = w.Write([]byte(clusters()))
		case "/stats":
			_, _ = w.Write([]byte(stats()))
		case "/config_dump":
			_, _ = w.Write([]byte(`{"configs":[{"active_state":{"listener":{"filter_chains":[{"filters":[{"typed_config":{"cluster":"p8080"}}]}]}}},
			  {"warming_state":{"listener":{"filter_chains":[{"filters":[{"typed_config":{"cluster":"p9000"}}]}]}}}]}`))
		default:
			http.NotFound(w, r)
		}
	})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return dir
}

func TestAdmin(t *testing.T) {
	healthy := `{"cluster_statuses":[{"name":"p8080","host_statuses":[{"hostname":"karga-web-r2","address":{"socket_address":{"address":"10.0.0.2","port_value":8080}},
	  "health_status":{"eds_health_status":"HEALTHY"},"stats":[{"name":"cx_active","value":"1"}]}]}]}`
	failing := `{"cluster_statuses":[{"name":"p8080","host_statuses":[{"hostname":"karga-web-r2",
	  "health_status":{"eds_health_status":"HEALTHY","failed_active_health_check":true},"stats":[{"name":"cx_active"}]}]}]}`
	clusters, total := healthy, "3"
	dir := fakeAdmin(t, func() string { return clusters }, func() string { return "cluster.p8080.upstream_cx_active: " + total })
	a := NewAdmin(dir)
	ctx := context.Background()

	if ok, err := a.Ready(ctx, map[string]string{"p8080": "karga-web-r2"}); err != nil || !ok {
		t.Fatalf("ready: %v %v", ok, err)
	}
	if ok, _ := a.Ready(ctx, map[string]string{"p8080": "karga-web-r1"}); ok {
		t.Fatal("wrong backend reported ready")
	}
	if ok, _ := a.Ready(ctx, map[string]string{"p9000": "karga-web-r2"}); ok {
		t.Fatal("cluster without an active listener reported ready")
	}
	if ok, err := a.Ready(ctx, map[string]string{"p8080": "10.0.0.2:8080"}); err != nil || !ok {
		t.Fatalf("ready by address: %v %v", ok, err)
	}
	if n, err := a.OldConnections(ctx, []string{"p8080"}); err != nil || n != 2 {
		t.Fatalf("old connections: %d %v", n, err)
	}
	clusters = failing
	if ok, _ := a.Ready(ctx, map[string]string{"p8080": "karga-web-r2"}); ok {
		t.Fatal("host failing health checks reported ready")
	}
	if err := a.WaitReady(ctx, map[string]string{"p8080": "karga-web-r2"}, 300*time.Millisecond); err == nil {
		t.Fatal("WaitReady should time out")
	}
	clusters, total = healthy, "1"
	if drained, err := a.WaitDrained(ctx, []string{"p8080"}, time.Second); err != nil || !drained {
		t.Fatalf("drained: %v %v", drained, err)
	}
	total = "5"
	if drained, _ := a.WaitDrained(ctx, []string{"p8080"}, 300*time.Millisecond); drained {
		t.Fatal("should not be drained")
	}
}
