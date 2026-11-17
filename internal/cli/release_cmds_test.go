package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hasankaradmir/karga/internal/adapters/fakeruntime"
	"github.com/hasankaradmir/karga/internal/adapters/sqlite"
	"github.com/hasankaradmir/karga/internal/application/deployment"
	"github.com/hasankaradmir/karga/internal/ports"
	"github.com/hasankaradmir/karga/internal/ports/runtimetest"
)

// cliEnv runs commands against a fake runtime and a temporary KARGA_HOME.
type cliEnv struct {
	t      *testing.T
	home   string
	rt     *fakeruntime.Runtime
	port   string
	stderr string // of the last run
}

func (e *cliEnv) lastStderr() string { return e.stderr }

func newCLIEnv(t *testing.T) *cliEnv {
	return &cliEnv{t: t, home: t.TempDir(), rt: fakeruntime.New().WithGateway(), port: strconv.Itoa(runtimetest.FreePort(t))}
}

func (e *cliEnv) run(args ...string) (string, error) {
	e.t.Helper()
	var out, errOut bytes.Buffer
	cmd := newRootCmd(&out, &errOut, func(context.Context) (ports.Runtime, func(), error) { return e.rt, func() {}, nil })
	cmd.SetArgs(append([]string{"--home", e.home}, args...))
	err := cmd.Execute()
	e.stderr = errOut.String()
	return out.String(), err
}

func (e *cliEnv) mustRun(args ...string) string {
	e.t.Helper()
	out, err := e.run(args...)
	if err != nil {
		e.t.Fatalf("karga %v: %v\n%s", args, err, out)
	}
	return out
}

const redis = "../../examples/redis"

// Tests run without Envoy: proxy and gateway configuration is written, readiness is assumed.
func init() { deployment.Proxies = fakeruntime.Proxies{} }

func TestReleaseLifecycleCLI(t *testing.T) {
	e := newCLIEnv(t)
	port := "service.enabled=false"

	out := e.mustRun("install", "r", redis, "--set", port)
	if !strings.Contains(out, `Release "r" installed (revision 1)`) || !strings.Contains(out, "create workload redis") {
		t.Fatalf("install:\n%s", out)
	}
	if out := e.mustRun("install", "r", redis, "--set", port); !strings.Contains(out, "already installed") {
		t.Fatalf("second install:\n%s", out)
	}

	out = e.mustRun("upgrade", "r", redis, "--set", port, "--set", "image.tag=7.2", "--dry-run")
	if !strings.Contains(out, "Revision: 1 -> 2") || !strings.Contains(out, "~ WORKLOAD redis (recreate)") ||
		!strings.Contains(out, "0 to create, 1 to update, 0 to delete") || strings.Contains(out, "spec.image") {
		t.Fatalf("dry run summary:\n%s", out)
	}
	out = e.mustRun("upgrade", "r", redis, "--set", port, "--set", "image.tag=7.2", "--dry-run", "--verbose")
	if !strings.Contains(out, `spec.image: "redis:7.4" -> "redis:7.2"`) {
		t.Fatalf("dry run --verbose:\n%s", out)
	}
	if img := e.rt.Workloads["r-redis"].Request.Image; img != "redis:7.4" {
		t.Fatalf("dry run changed the workload: %s", img)
	}

	e.mustRun("upgrade", "r", redis, "--set", port, "--set", "image.tag=7.2")
	out = e.mustRun("history", "r")
	if !strings.Contains(out, "REVISION") || !strings.Contains(out, "Upgrade complete") {
		t.Fatalf("history:\n%s", out)
	}
	var hist []map[string]any
	if err := json.Unmarshal([]byte(e.mustRun("history", "r", "-o", "json")), &hist); err != nil || len(hist) != 2 {
		t.Fatalf("history json: %v %v", hist, err)
	}

	out = e.mustRun("get", "values", "r")
	if !strings.Contains(out, "tag: \"7.2\"") || strings.Contains(out, "repository") {
		t.Fatalf("get values:\n%s", out)
	}
	if out := e.mustRun("get", "values", "r", "--all"); !strings.Contains(out, "repository: redis") {
		t.Fatalf("get values --all:\n%s", out)
	}
	if out := e.mustRun("get", "manifest", "r", "--revision", "1"); !strings.Contains(out, "image: redis:7.4") {
		t.Fatalf("get manifest:\n%s", out)
	}

	out = e.mustRun("rollback", "r", "1")
	if !strings.Contains(out, "rolled back (revision 3)") {
		t.Fatalf("rollback:\n%s", out)
	}
	out = e.mustRun("status", "r")
	if !strings.Contains(out, "Status:   deployed") || !strings.Contains(out, "r-redis") || !strings.Contains(out, "running") {
		t.Fatalf("status:\n%s", out)
	}
	if out := e.mustRun("list"); !strings.Contains(out, "r     3         deployed") {
		t.Fatalf("list:\n%s", out)
	}

	e.mustRun("uninstall", "r")
	if out := e.mustRun("list", "-o", "json"); strings.TrimSpace(out) != "[]" {
		t.Fatalf("list after uninstall: %s", out)
	}
	if _, ok := e.rt.Volumes["r-redis-data"]; !ok {
		t.Fatal("uninstall must keep the volume")
	}
}

func TestReleaseCLIErrors(t *testing.T) {
	e := newCLIEnv(t)
	for _, args := range [][]string{
		{"upgrade", "nope", redis},
		{"rollback", "nope"},
		{"rollback", "nope", "zero"},
		{"uninstall", "nope"},
		{"status", "nope"},
		{"history", "nope"},
		{"get", "values", "nope"},
	} {
		if _, err := e.run(args...); err == nil {
			t.Errorf("karga %v: expected error", args)
		}
	}
	e.mustRun("install", "r", redis, "--set", "service.enabled=false")
	e.rt.Health = ports.HealthUnhealthy
	_, err := e.run("upgrade", "r", redis, "--set", "service.enabled=false", "--set", "image.tag=7.2", "--timeout", "100ms")
	if err == nil || !strings.Contains(err.Error(), "revision 1 stays current") || !strings.Contains(err.Error(), "karga rollback r") {
		t.Fatalf("failed upgrade hint: %v", err)
	}
}

func TestStrategyFlag(t *testing.T) {
	e := newCLIEnv(t)
	web := "../../tests/integration/testdata/web"
	e.mustRun("install", "w", web)
	out := e.mustRun("upgrade", "w", web, "--set", "content=v2", "--dry-run")
	if !strings.Contains(out, "WORKLOAD web (replace)") {
		t.Fatalf("dry run:\n%s", out)
	}
	if out := e.mustRun("upgrade", "w", web, "--set", "content=v2", "--dry-run", "--strategy", "recreate"); !strings.Contains(out, "WORKLOAD web (recreate)") {
		t.Fatalf("override:\n%s", out)
	}
	if out := e.mustRun("upgrade", "w", web, "--set", "content=v2"); !strings.Contains(out, "update workload web (replace)") {
		t.Fatalf("upgrade:\n%s", out)
	}
	if _, err := e.run("upgrade", "w", web, "--strategy", "rolling"); err == nil || !strings.Contains(err.Error(), "invalid --strategy") {
		t.Fatalf("invalid strategy: %v", err)
	}
}

func TestStateBackup(t *testing.T) {
	e := newCLIEnv(t)
	e.mustRun("install", "w", "../../tests/integration/testdata/web")
	dest := filepath.Join(t.TempDir(), "state.db")
	if out := e.mustRun("state", "backup", dest); !strings.Contains(out, "state written to") {
		t.Fatalf("backup: %s", out)
	}
	if _, err := e.run("state", "backup", dest); err == nil {
		t.Fatal("backup over an existing file must fail")
	}
	store, err := sqlite.Open(context.Background(), dest)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if _, err := store.GetRelease(context.Background(), "w"); err != nil {
		t.Fatalf("backup misses release w: %v", err)
	}
}

func TestLogs(t *testing.T) {
	e := newCLIEnv(t)
	web := "../../tests/integration/testdata/web"
	e.mustRun("install", "w", web, "--set", "second=true")
	e.rt.Output = map[string]string{"w-web": "web 1\nweb 2\nweb 3\n", "w-api": "api 1\napi 2"}

	if out := e.mustRun("logs", "w", "web", "--tail", "2"); out != "web 2\nweb 3\n" {
		t.Fatalf("one workload: %q", out)
	}
	if out := e.mustRun("logs", "w", "--tail", "1"); out != "api | api 2\nweb | web 3\n" {
		t.Fatalf("all workloads: %q", out)
	}
	if _, err := e.run("logs", "w", "nope"); err == nil || !strings.Contains(err.Error(), "workloads: api, web") {
		t.Fatalf("unknown workload: %v", err)
	}
	if _, err := e.run("logs"); err == nil || !strings.Contains(err.Error(), "--gateway") {
		t.Fatalf("no release: %v", err)
	}
	if _, err := e.run("logs", "w", "--proxy"); err != nil {
		t.Fatalf("release proxy logs: %v", err)
	}
	e.mustRun("install", "r", redis, "--set", "service.enabled=false")
	if _, err := e.run("logs", "r", "--proxy"); err == nil || !strings.Contains(err.Error(), "has no proxy") {
		t.Fatalf("no services, no proxy: %v", err)
	}
}

func TestGatewayStatusAndSettings(t *testing.T) {
	e := newCLIEnv(t)
	out := e.mustRun("gateway", "status")
	if !strings.Contains(out, "Gateway:    not running (nothing is exposed)") || !strings.Contains(out, "Access log: off") {
		t.Fatalf("status:\n%s", out)
	}
	e.mustRun("gateway", "set", "access-log", "on")
	if out := e.mustRun("gateway", "status"); !strings.Contains(out, "Access log: on") {
		t.Fatalf("after set:\n%s", out)
	}
	if _, err := e.run("gateway", "set", "access-log", "maybe"); err == nil {
		t.Fatal("invalid value accepted")
	}
	if out := e.mustRun("gateway", "status", "-o", "json"); !strings.Contains(out, `"accessLog": true`) || !strings.Contains(out, `"routes": []`) {
		t.Fatalf("json:\n%s", out)
	}
}

func TestGatewayListeners(t *testing.T) {
	e := newCLIEnv(t)
	if out := e.mustRun("gateway", "listener", "list"); !strings.Contains(out, "web        80    http") || !strings.Contains(out, "websecure  443   https") {
		t.Fatalf("default listeners:\n%s", out)
	}
	pgPort := strconv.Itoa(runtimetest.FreePort(t))
	e.mustRun("gateway", "listener", "add", "pg", "--port", pgPort, "--protocol", "tcp")
	if _, err := e.run("gateway", "listener", "add", "alt", "--port", "80", "--protocol", "http"); err == nil || !strings.Contains(err.Error(), `port 80 is used by listener "web"`) {
		t.Fatalf("duplicate port: %v", err)
	}
	if _, err := e.run("install", "r", redis, "--set", "service.listener=nope"); err == nil || !strings.Contains(err.Error(), "no such gateway listener") {
		t.Fatalf("unknown listener: %v", err)
	}
	e.mustRun("install", "r", redis, "--set", "service.listener=pg")
	if out := e.mustRun("gateway", "status"); !strings.Contains(out, "pg:"+pgPort) || !strings.Contains(out, "redis:6379") || !strings.Contains(out, "Listen:     127.0.0.1") {
		t.Fatalf("status:\n%s", out)
	}
	if out := e.mustRun("gateway", "listener", "list"); !strings.Contains(out, "r/redis") {
		t.Fatalf("users:\n%s", out)
	}
	if _, err := e.run("gateway", "listener", "remove", "pg"); err == nil || !strings.Contains(err.Error(), "is used by r/redis") {
		t.Fatalf("remove in use: %v", err)
	}
	e.mustRun("gateway", "set", "listen", "127.0.0.1,::1")
	if out := e.mustRun("gateway", "status", "-o", "json"); !strings.Contains(out, `"::1"`) {
		t.Fatalf("listen:\n%s", out)
	}
	if _, err := e.run("gateway", "set", "listen", "localhost"); err == nil || !strings.Contains(err.Error(), "not an IP address") {
		t.Fatalf("bad address: %v", err)
	}
	e.mustRun("uninstall", "r")
	e.mustRun("gateway", "listener", "remove", "pg")
}

func TestStatusShowsRestartsAndOOM(t *testing.T) {
	e := newCLIEnv(t)
	e.mustRun("install", "w", "../../tests/integration/testdata/web")
	e.rt.Workloads["w-web"].Restarts, e.rt.Workloads["w-web"].OOMKilled = 3, true
	out := e.mustRun("status", "w")
	if !strings.Contains(out, "RESTARTS") || !strings.Contains(out, "3") || !strings.Contains(out, "oom-killed") {
		t.Fatalf("status:\n%s", out)
	}
}

func TestVolumeBackupRestore(t *testing.T) {
	e := newCLIEnv(t)
	e.mustRun("install", "r", redis, "--set", "service.enabled=false")
	e.rt.Files = map[string]map[string][]byte{"r-redis": {"/data/dump.rdb": []byte("snapshot"), "/data/appendonly/a.aof": []byte("log")}}
	file := filepath.Join(t.TempDir(), "data.tar.gz")

	if out := e.mustRun("volume", "backup", "r", "redis-data", file, "--stop"); !strings.Contains(out, "written to") {
		t.Fatalf("backup: %s", out)
	}
	if info, err := os.Stat(file); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("backup file: %v %v", info, err)
	}
	if w := e.rt.Workloads["r-redis"]; w.State != ports.StateRunning {
		t.Fatalf("--stop must start the workload again: %s", w.State)
	}
	if _, err := e.run("volume", "backup", "r", "redis-data", file); err == nil {
		t.Fatal("backup over an existing file must fail")
	}

	e.rt.Files["r-redis"] = map[string][]byte{}
	e.mustRun("volume", "restore", "r", "redis-data", file)
	if got := e.rt.Files["r-redis"]; string(got["/data/dump.rdb"]) != "snapshot" || string(got["/data/appendonly/a.aof"]) != "log" {
		t.Fatalf("restored files: %v", got)
	}
	if _, err := e.run("volume", "backup", "r", "nope", file+"2"); err == nil || !strings.Contains(err.Error(), `no volume "nope"`) {
		t.Fatalf("unknown volume: %v", err)
	}
}

func TestLiveProgress(t *testing.T) {
	e := newCLIEnv(t)
	web := "../../tests/integration/testdata/web"
	e.mustRun("install", "w", web)
	progress := e.lastStderr()
	for _, want := range []string{"workload web: image busybox:1.37", "workload web: creating w-web", "workload web: create done"} {
		if !strings.Contains(progress, want) {
			t.Errorf("progress misses %q:\n%s", want, progress)
		}
	}
	e.mustRun("upgrade", "w", web, "--set", "content=v2")
	if p := e.lastStderr(); !strings.Contains(p, "starting w-web-next next to w-web") || !strings.Contains(p, "switching traffic to w-web-next") {
		t.Errorf("replace progress:\n%s", p)
	}
	e.mustRun("upgrade", "w", web, "--set", "content=v3", "-o", "json")
	if p := e.lastStderr(); p != "" {
		t.Errorf("no progress with JSON output: %q", p)
	}
}

func TestExec(t *testing.T) {
	e := newCLIEnv(t)
	e.mustRun("install", "w", "../../tests/integration/testdata/web")
	var got []string
	e.rt.ExecFunc = func(name string, o ports.ExecOptions) int {
		got = append([]string{name}, o.Cmd...)
		_, _ = io.WriteString(o.Stdout, "hello\\n")
		return 7
	}
	out, err := e.run("exec", "w", "web", "--", "echo", "hello")
	var exit *ExitCodeError
	if !errors.As(err, &exit) || exit.Code != 7 || out != "hello\\n" || strings.Join(got, " ") != "w-web echo hello" {
		t.Fatalf("exec: %q %v %v", out, err, got)
	}
	if _, err := e.run("exec", "w", "nope", "--", "true"); err == nil || !strings.Contains(err.Error(), "workloads: web") {
		t.Fatalf("unknown workload: %v", err)
	}
	if _, err := e.run("exec", "w", "web"); err == nil || !strings.Contains(err.Error(), "needs a terminal") {
		t.Fatalf("default interactive sh without a terminal: %v", err)
	}
}

func TestDiffAndHistory(t *testing.T) {
	e := newCLIEnv(t)
	e.mustRun("install", "r", redis, "--set", "service.enabled=false")
	e.mustRun("upgrade", "r", redis, "--set", "service.enabled=false", "--set", "image.tag=7.2")
	out := e.mustRun("diff", "r", "1")
	for _, want := range []string{"revision 1 -> 2", `image.tag: "7.4" -> "7.2"`, "~ WORKLOAD redis", `spec.image: "redis:7.4" -> "redis:7.2"`} {
		if !strings.Contains(out, want) {
			t.Errorf("diff misses %q:\n%s", want, out)
		}
	}
	if out := e.mustRun("diff", "r", "2", "2"); !strings.Contains(out, "no differences") {
		t.Fatalf("same revision:\n%s", out)
	}
	if out := e.mustRun("history", "r", "--diff"); !strings.Contains(out, "CHANGES") || !strings.Contains(out, "first") || !strings.Contains(out, "~1") {
		t.Fatalf("history --diff:\n%s", out)
	}

	// Secret values and writeOnly values never show, only that they changed.
	pg := "../../examples/postgres"
	pgPort := "service.enabled=false"
	e.mustRun("install", "db", pg, "--set", pgPort, "--set", "database.password=first-secret")
	e.mustRun("upgrade", "db", pg, "--set", pgPort, "--set", "database.password=second-secret")
	out = e.mustRun("diff", "db", "1")
	if strings.Contains(out, "first-secret") || strings.Contains(out, "second-secret") || !strings.Contains(out, "(changed)") {
		t.Fatalf("secrets in diff:\n%s", out)
	}
}

func TestCreateLintInstallNotes(t *testing.T) {
	e := newCLIEnv(t)
	dir := filepath.Join(t.TempDir(), "shop")
	e.mustRun("create", dir)
	if out := e.mustRun("lint", dir); !strings.Contains(out, "0 failed") {
		t.Fatalf("lint of a created package:\n%s", out)
	}
	if _, err := e.run("create", dir); err == nil {
		t.Fatal("create over an existing directory must fail")
	}
	// The fake runtime runs nothing: the container's address is 127.0.0.1 and, once it "exists",
	// a server answers karga's http health check on the port the package listens on.
	for _, f := range []string{"templates/web.yaml"} {
		p := filepath.Join(dir, f)
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		b = []byte(strings.ReplaceAll(strings.ReplaceAll(string(b), "containerPort: 80", "containerPort: "+e.port), "port: 80", "port: "+e.port))
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	e.rt.IPs = map[string]map[string]string{"s-web": {"s-net": "127.0.0.1"}}
	srv := &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), ReadHeaderTimeout: time.Second}
	t.Cleanup(func() { _ = srv.Close() })
	go func() {
		for range 1000 {
			if _, err := e.rt.GetWorkload(context.Background(), "s-web"); err == nil {
				if l, err := net.Listen("tcp", "127.0.0.1:"+e.port); err == nil {
					_ = srv.Serve(l)
				}
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	out := e.mustRun("install", "s", dir, "--set", "expose.host=shop.local")
	if !strings.Contains(out, "NOTES:") || !strings.Contains(out, "s (revision 1) runs nginx") {
		t.Fatalf("notes after install:\n%s", out)
	}
	if out := e.mustRun("get", "notes", "s"); !strings.Contains(out, "karga logs s -f") {
		t.Fatalf("get notes:\n%s", out)
	}
	if _, err := e.run("create", filepath.Join(t.TempDir(), "Bad_Name")); err == nil {
		t.Fatal("a package name must be a DNS label")
	}
}

func TestStatusWatchAndColor(t *testing.T) {
	e := newCLIEnv(t)
	e.mustRun("install", "w", "../../tests/integration/testdata/web")
	out := e.mustRun("status", "w", "--watch", "--count", "2", "--interval", "10ms")
	if strings.Count(out, "Every 10ms: karga status w") != 2 || strings.Contains(out, "\x1b[") {
		t.Fatalf("watch without a terminal:\n%s", out)
	}
	out = e.mustRun("status", "w", "--color", "always")
	if !strings.Contains(out, colorGreen+"deployed"+colorReset) || !strings.Contains(out, colorGreen+"running"+colorReset) {
		t.Fatalf("colors:\n%q", out)
	}
	// Coloring never shifts columns: the plain and colored tables have the same layout.
	plain := e.mustRun("status", "w", "--color", "never")
	stripped := regexp.MustCompile(`\x1b\[[0-9;]*m`).ReplaceAllString(out, "")
	if stripped != plain {
		t.Fatalf("colored table misaligned:\n%s\n---\n%s", stripped, plain)
	}
}

// complete runs shell completion for args (the last one is the word being completed).
func (e *cliEnv) complete(args ...string) []string {
	e.t.Helper()
	var out bytes.Buffer
	cmd := newRootCmd(&out, &bytes.Buffer{}, func(context.Context) (ports.Runtime, func(), error) { return e.rt, func() {}, nil })
	cmd.SetArgs(append([]string{"__complete", "--home", e.home}, args...))
	if err := cmd.Execute(); err != nil {
		e.t.Fatalf("complete %v: %v", args, err)
	}
	var vals []string
	for _, l := range strings.Split(out.String(), "\n") {
		if l != "" && !strings.HasPrefix(l, ":") && !strings.HasPrefix(l, "Completion ended") {
			vals = append(vals, strings.SplitN(l, "\t", 2)[0])
		}
	}
	return vals
}

func TestDynamicCompletion(t *testing.T) {
	e := newCLIEnv(t)
	if got := e.complete("status", ""); len(got) != 0 {
		t.Fatalf("no state yet, nothing to offer: %v", got)
	}
	if _, err := os.Stat(filepath.Join(e.home, "karga.db")); err == nil {
		t.Fatal("completion created the state database")
	}
	e.mustRun("install", "web1", "../../tests/integration/testdata/web", "--set", "second=true")
	e.mustRun("install", "redis1", redis, "--set", "service.enabled=false")
	e.mustRun("upgrade", "web1", "../../tests/integration/testdata/web", "--set", "second=true", "--set", "content=v2")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"status", ""}, "redis1 web1"},
		{[]string{"status", "w"}, "web1"},
		{[]string{"logs", "web1", ""}, "api web"},
		{[]string{"exec", "web1", "a"}, "api"},
		{[]string{"rollback", "web1", ""}, "2 1"},
		{[]string{"volume", "backup", "redis1", ""}, "redis-data"},
		{[]string{"get", "notes", ""}, "redis1 web1"},
	} {
		if got := strings.Join(e.complete(tc.args...), " "); got != tc.want {
			t.Errorf("complete %v: %q, want %q", tc.args, got, tc.want)
		}
	}
}
