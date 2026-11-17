//go:build e2e

// Package e2e runs the karga binary against a real Docker Engine (tech design §19.4, without the
// registry steps: packages are installed from local directories).
package e2e

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

var karga, kargaServer string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "karga-e2e-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if runtime.GOOS != "linux" {
		// Services need karga's gateway and release proxy: Linux with a local Docker.
		fmt.Fprintln(os.Stderr, "e2e tests run on Linux with a local Docker: use make test-linux")
		os.Exit(1)
	}
	karga, kargaServer = filepath.Join(dir, "karga"), filepath.Join(dir, "karga-server")
	for bin, pkg := range map[string]string{karga: "../../cmd/karga", kargaServer: "../../cmd/karga-server"} {
		build := exec.Command("go", "build", "-o", bin, pkg)
		build.Stdout, build.Stderr = os.Stdout, os.Stderr
		if err := build.Run(); err != nil {
			fmt.Fprintln(os.Stderr, "build", pkg+":", err)
			os.Exit(1)
		}
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

type env struct {
	t      *testing.T
	home   string
	socket string
}

// newEnv starts a karga-server with its own KARGA_HOME and socket; the CLI connects to it.
func newEnv(t *testing.T) *env {
	t.Helper()
	dir, err := os.MkdirTemp("", "ke2e") // short: unix socket paths are limited
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, home: t.TempDir(), socket: filepath.Join(dir, "s.sock")}
	srv := exec.Command(kargaServer, "--home", e.home, "--socket", e.socket)
	log, err := os.Create(filepath.Join(e.home, "server.log"))
	if err != nil {
		t.Fatal(err)
	}
	srv.Stdout, srv.Stderr = log, log
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = srv.Process.Signal(syscall.SIGTERM)
		_ = srv.Wait()
		_ = log.Close()
		if t.Failed() {
			b, _ := os.ReadFile(filepath.Join(e.home, "server.log"))
			t.Logf("karga-server log:\n%s", b)
		}
		_ = os.RemoveAll(dir)
	})
	deadline := time.Now().Add(15 * time.Second)
	for {
		if out, err := e.try("version"); err == nil && !strings.Contains(out, "not running") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("karga-server did not start")
		}
		time.Sleep(100 * time.Millisecond)
	}
	return e
}

// run executes karga and fails the test on error.
func (e *env) run(args ...string) string {
	e.t.Helper()
	out, err := e.try(args...)
	if err != nil {
		e.t.Fatalf("karga %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func (e *env) try(args ...string) (string, error) {
	cmd := exec.Command(karga, args...)
	cmd.Env = append(os.Environ(), "KARGA_SOCKET="+e.socket)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func releaseName(prefix string) string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return prefix + "-" + hex.EncodeToString(b)
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// cleanup removes the release and, as a safety net, anything Docker still has labelled with it.
func (e *env) cleanup(rel string) {
	e.t.Cleanup(func() {
		_, _ = e.try("uninstall", rel, "--delete-volumes")
		filter := "label=io.karga.release=" + rel
		for _, args := range [][]string{
			{"ps", "-aq", "--filter", filter},
			{"network", "ls", "-q", "--filter", filter},
			{"volume", "ls", "-q", "--filter", filter},
		} {
			ids, _ := exec.Command("docker", args...).Output()
			for _, id := range strings.Fields(string(ids)) {
				rm := map[string][]string{"ps": {"rm", "-f"}, "network": {"network", "rm"}, "volume": {"volume", "rm", "-f"}}[args[0]]
				_ = exec.Command("docker", append(rm, id)...).Run()
			}
		}
	})
}

// redis sends one inline command to Redis on the host port and returns the raw reply.
func redis(t *testing.T, port int, command string) string {
	t.Helper()
	var lastErr error
	for range 20 {
		conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), time.Second)
		if err != nil {
			lastErr = err
			time.Sleep(250 * time.Millisecond)
			continue
		}
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := fmt.Fprintf(conn, "%s\r\n", command); err != nil {
			_ = conn.Close()
			lastErr = err
			continue
		}
		var reply strings.Builder
		r := bufio.NewReader(conn)
		for {
			line, err := r.ReadString('\n')
			reply.WriteString(line)
			if err != nil || r.Buffered() == 0 {
				break
			}
		}
		_ = conn.Close()
		if reply.Len() > 0 {
			return reply.String()
		}
		lastErr = fmt.Errorf("empty reply")
	}
	t.Fatalf("redis %q on port %d: %v", command, port, lastErr)
	return ""
}

func status(t *testing.T, e *env, rel string) (string, map[string]string) {
	t.Helper()
	var rep struct {
		Release struct {
			Status string `json:"status"`
		} `json:"release"`
		Resources []struct {
			Kind   string `json:"kind"`
			Actual string `json:"actual"`
			Health string `json:"health"`
		} `json:"resources"`
	}
	if err := json.Unmarshal([]byte(e.run("status", rel, "-o", "json")), &rep); err != nil {
		t.Fatal(err)
	}
	actual := map[string]string{}
	for _, r := range rep.Resources {
		actual[r.Kind] = r.Actual + "/" + r.Health
	}
	return rep.Release.Status, actual
}

func revisions(t *testing.T, e *env, rel string) []string {
	t.Helper()
	var hist []struct {
		Revision int    `json:"revision"`
		Status   string `json:"status"`
	}
	if err := json.Unmarshal([]byte(e.run("history", rel, "-o", "json")), &hist); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, h := range hist {
		out = append(out, fmt.Sprintf("%d:%s", h.Revision, h.Status))
	}
	return out
}

func TestRedisLifecycle(t *testing.T) {
	e := newEnv(t)
	rel := releaseName("e2e-redis")
	e.cleanup(rel)
	port := freePort(t)
	// The host's administrator opens a tcp listener; the package exposes its Service on it.
	e.run("gateway", "listener", "add", "redis", "--port", strconv.Itoa(port), "--protocol", "tcp")
	portSet := "service.listener=redis"

	// 1. Lint and 2. install.
	e.run("lint", "../../examples/redis")
	if out := e.run("install", rel, "../../examples/redis", "--set", portSet); !strings.Contains(out, "installed (revision 1)") {
		t.Fatalf("install:\n%s", out)
	}
	if st, actual := status(t, e, rel); st != "deployed" || actual["Workload"] != "running/healthy" {
		t.Fatalf("status after install: %s %v", st, actual)
	}

	// 3. Use it from the host; it is published on 127.0.0.1 only.
	if got := redis(t, port, "PING"); !strings.HasPrefix(got, "+PONG") {
		t.Fatalf("PING: %q", got)
	}
	if got := redis(t, port, "SET e2e kept"); !strings.HasPrefix(got, "+OK") {
		t.Fatalf("SET: %q", got)
	}

	// 3b. Volume backup and restore: the key comes back after the data is wiped.
	backup := filepath.Join(t.TempDir(), "redis-data.tar.gz")
	redis(t, port, "SAVE")
	if out := e.run("volume", "backup", rel, "redis-data", backup, "--stop"); !strings.Contains(out, "written to") {
		t.Fatalf("volume backup:\n%s", out)
	}
	redis(t, port, "FLUSHALL")
	redis(t, port, "SAVE")
	if got := redis(t, port, "GET e2e"); !strings.HasPrefix(got, "$-1") {
		t.Fatalf("data not wiped: %q", got)
	}
	if out := e.run("volume", "restore", rel, "redis-data", backup); !strings.Contains(out, "restored from") {
		t.Fatalf("volume restore:\n%s", out)
	}
	if got := redis(t, port, "GET e2e"); !strings.Contains(got, "kept") {
		t.Fatalf("key after restore: %q", got)
	}
	// The workload publishes nothing; the release proxy only a loopback upstream for the gateway.
	if out, _ := exec.Command("docker", "port", rel+"-redis").Output(); len(strings.TrimSpace(string(out))) != 0 {
		t.Fatalf("the workload published ports: %s", out)
	}
	if out, _ := exec.Command("docker", "port", rel+"-karga-proxy").Output(); !strings.Contains(string(out), "127.0.0.1:") || strings.Contains(string(out), "0.0.0.0") {
		t.Fatalf("proxy ports: %s", out)
	}
	if out := e.run("gateway", "status"); !strings.Contains(out, "redis:"+strconv.Itoa(port)) || !strings.Contains(out, "Listen:     127.0.0.1") {
		t.Fatalf("gateway status:\n%s", out)
	}

	// 4. Dry run shows the plan and changes nothing.
	plan := e.run("upgrade", rel, "../../examples/redis", "--set", portSet, "--set", "config.maxmemory=128mb", "--dry-run")
	if !strings.Contains(plan, "Revision: 1 -> 2") || !strings.Contains(plan, "CONFIG redis-config") || !strings.Contains(plan, "WORKLOAD redis (recreate") {
		t.Fatalf("plan:\n%s", plan)
	}
	if got := strings.Join(revisions(t, e, rel), ","); got != "1:deployed" {
		t.Fatalf("dry run created a revision: %s", got)
	}

	// 5. Upgrade: new config, data kept.
	e.run("upgrade", rel, "../../examples/redis", "--set", portSet, "--set", "config.maxmemory=128mb")
	if got := redis(t, port, "CONFIG GET maxmemory"); !strings.Contains(got, "134217728") {
		t.Fatalf("maxmemory after upgrade: %q", got)
	}
	if got := redis(t, port, "GET e2e"); !strings.Contains(got, "kept") {
		t.Fatalf("data after upgrade: %q", got)
	}

	// 6. Rollback to revision 1 restores the previous desired state as revision 3.
	e.run("rollback", rel, "1")
	if got := redis(t, port, "CONFIG GET maxmemory"); !strings.Contains(got, "268435456") {
		t.Fatalf("maxmemory after rollback: %q", got)
	}
	if got := strings.Join(revisions(t, e, rel), ","); got != "1:deployed,2:deployed,3:deployed" {
		t.Fatalf("history: %s", got)
	}

	// 7. Uninstall keeps the volume, 8. a reinstall finds the data again.
	e.run("uninstall", rel)
	if err := exec.Command("docker", "volume", "inspect", rel+"-redis-data").Run(); err != nil {
		t.Fatalf("volume not retained: %v", err)
	}
	e.run("install", rel, "../../examples/redis", "--set", portSet)
	if got := redis(t, port, "GET e2e"); !strings.Contains(got, "kept") {
		t.Fatalf("data after reinstall: %q", got)
	}

	// 9. --delete-volumes removes the data.
	e.run("uninstall", rel, "--delete-volumes")
	if err := exec.Command("docker", "volume", "inspect", rel+"-redis-data").Run(); err == nil {
		t.Fatal("volume still exists after --delete-volumes")
	}
}

func TestPostgresSecretStaysOutOfDocker(t *testing.T) {
	e := newEnv(t)
	rel := releaseName("e2e-pg")
	e.cleanup(rel)
	const password = "e2e-hunter2"
	e.run("install", rel, "../../examples/postgres", "--set", "database.password="+password)
	if st, actual := status(t, e, rel); st != "deployed" || actual["Workload"] != "running/healthy" {
		t.Fatalf("status: %s %v", st, actual)
	}
	inspect, err := exec.Command("docker", "inspect", rel+"-postgres").Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(inspect), password) {
		t.Fatal("password visible in docker inspect")
	}
	if out := e.run("get", "values", rel, "--all"); strings.Contains(out, password) || !strings.Contains(out, "<redacted>") {
		t.Fatalf("get values:\n%s", out)
	}
}
