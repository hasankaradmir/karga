package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/hasankaradmir/karga/internal/adapters/fakeruntime"
	"github.com/hasankaradmir/karga/internal/adapters/sqlite"
	"github.com/hasankaradmir/karga/internal/ports"
	"github.com/hasankaradmir/karga/internal/ports/runtimetest"
)

// cliEnv runs commands against a fake runtime and a temporary KARGA_HOME.
type cliEnv struct {
	t    *testing.T
	home string
	rt   *fakeruntime.Runtime
	port string
}

func newCLIEnv(t *testing.T) *cliEnv {
	return &cliEnv{t: t, home: t.TempDir(), rt: fakeruntime.New(), port: strconv.Itoa(runtimetest.FreePort(t))}
}

func (e *cliEnv) run(args ...string) (string, error) {
	e.t.Helper()
	var out, errOut bytes.Buffer
	cmd := newRootCmd(&out, &errOut, func(context.Context) (ports.Runtime, func(), error) { return e.rt, func() {}, nil })
	cmd.SetArgs(append(args, "--home", e.home))
	err := cmd.Execute()
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

func TestReleaseLifecycleCLI(t *testing.T) {
	e := newCLIEnv(t)
	port := "network.port=" + e.port

	out := e.mustRun("install", "r", redis, "--set", port)
	if !strings.Contains(out, `Release "r" installed (revision 1)`) || !strings.Contains(out, "create workload redis") {
		t.Fatalf("install:\n%s", out)
	}
	if out := e.mustRun("install", "r", redis, "--set", port); !strings.Contains(out, "already installed") {
		t.Fatalf("second install:\n%s", out)
	}

	out = e.mustRun("upgrade", "r", redis, "--set", port, "--set", "image.tag=7.2", "--dry-run")
	if !strings.Contains(out, "Revision: 1 -> 2") || !strings.Contains(out, `spec.image: "redis:7.4" -> "redis:7.2"`) {
		t.Fatalf("dry run:\n%s", out)
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
	e.mustRun("install", "r", redis, "--set", "network.port="+e.port)
	e.rt.Health = ports.HealthUnhealthy
	_, err := e.run("upgrade", "r", redis, "--set", "network.port="+e.port, "--set", "image.tag=7.2", "--timeout", "100ms")
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
	if out := e.mustRun("upgrade", "w", web, "--set", "content=v2", "--set", "publish=true", "--dry-run"); !strings.Contains(out, "(recreate: publishes hostPort 18080/tcp without the gateway (Linux, tcp))") {
		t.Fatalf("fallback:\n%s", out)
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
	if _, err := e.run("logs", "w", "--proxy"); err == nil || !strings.Contains(err.Error(), "has no proxy") {
		t.Fatalf("no proxy off Linux: %v", err)
	}
}
