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

func TestConfigGolden(t *testing.T) {
	boot, err := Bootstrap("web")
	if err != nil {
		t.Fatal(err)
	}
	lds, err := Listeners([]int{9000, 8080, 9000})
	if err != nil {
		t.Fatal(err)
	}
	cds, err := Clusters([]int{9000, 8080}, "karga-web-r2")
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

func TestWriteAllAndBackend(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "proxy")
	if b, err := Backend(dir); err != nil || b != "" {
		t.Fatalf("no config: %q %v", b, err)
	}
	if err := WriteAll(dir, "web", []int{8080}, "karga-web-r1"); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode: %v %v", info.Mode(), err)
	}
	if err := SetBackend(dir, []int{8080}, "karga-web-r2"); err != nil {
		t.Fatal(err)
	}
	if b, err := Backend(dir); err != nil || b != "karga-web-r2" {
		t.Fatalf("backend: %q %v", b, err)
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
		default:
			http.NotFound(w, r)
		}
	})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return dir
}

func TestAdmin(t *testing.T) {
	healthy := `{"cluster_statuses":[{"name":"p8080","host_statuses":[{"hostname":"karga-web-r2",
	  "health_status":{"eds_health_status":"HEALTHY"},"stats":[{"name":"cx_active","value":"1"}]}]}]}`
	failing := `{"cluster_statuses":[{"name":"p8080","host_statuses":[{"hostname":"karga-web-r2",
	  "health_status":{"eds_health_status":"HEALTHY","failed_active_health_check":true},"stats":[{"name":"cx_active"}]}]}]}`
	clusters, total := healthy, "3"
	dir := fakeAdmin(t, func() string { return clusters }, func() string { return "cluster.p8080.upstream_cx_active: " + total })
	a := NewAdmin(dir)
	ctx := context.Background()

	if ok, err := a.Ready(ctx, []int{8080}, "karga-web-r2"); err != nil || !ok {
		t.Fatalf("ready: %v %v", ok, err)
	}
	if ok, _ := a.Ready(ctx, []int{8080}, "karga-web-r1"); ok {
		t.Fatal("wrong backend reported ready")
	}
	if n, err := a.OldConnections(ctx, []int{8080}); err != nil || n != 2 {
		t.Fatalf("old connections: %d %v", n, err)
	}
	clusters = failing
	if ok, _ := a.Ready(ctx, []int{8080}, "karga-web-r2"); ok {
		t.Fatal("host failing health checks reported ready")
	}
	if err := a.WaitReady(ctx, []int{8080}, "karga-web-r2", 300*time.Millisecond); err == nil {
		t.Fatal("WaitReady should time out")
	}
	clusters, total = healthy, "1"
	if drained, err := a.WaitDrained(ctx, []int{8080}, time.Second); err != nil || !drained {
		t.Fatalf("drained: %v %v", drained, err)
	}
	total = "5"
	if drained, _ := a.WaitDrained(ctx, []int{8080}, 300*time.Millisecond); drained {
		t.Fatal("should not be drained")
	}
}
