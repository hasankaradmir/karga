package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/hasankaradmir/karga-sdk/api"
)

func installed() *api.Result {
	return &api.Result{Release: "r", Revision: 2, From: 1, Status: api.StatusDeployed,
		Plan: api.Plan{Changes: []api.Change{
			{Kind: "Workload", Name: "web", Action: api.ActionUpdate, Strategy: "replace",
				Fields: []api.FieldChange{{Path: "spec.image", Old: `"nginx:1"`, New: `"nginx:2"`}}},
			{Kind: "Volume", Name: "cache", Action: api.ActionDelete, Notice: "its data is deleted"},
			{Kind: "Network", Name: "net", Action: api.ActionUnchanged},
		}},
		Warnings: []string{"workload web keeps image VOLUME /data in an anonymous volume"},
		Notes:    "r (revision 2) runs nginx:2."}
}

func TestDeploySendsPackageAndValues(t *testing.T) {
	e := newCLIEnv(t)
	e.f.result = installed()
	vals := filepath.Join(t.TempDir(), "prod.yaml")
	if err := os.WriteFile(vals, []byte("replicas: 3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := e.mustRun("upgrade", "r", demo, "-f", vals, "--set", "a=1", "--set-string", "b=007",
		"--reuse-values", "--strategy", "recreate", "--delete-volumes", "--timeout", "1m", "--yes")
	u := e.f.upgrade
	if u == nil || u.Name != "r" || !u.ReuseValues || u.Strategy != "recreate" || !u.DeleteVolumes || u.Timeout != api.Duration(time.Minute) || u.HistoryMax != nil {
		t.Fatalf("request: %+v", u)
	}
	// The server cannot read the client's files: the package and the values file travel as content.
	if e.f.pkg == nil || e.f.pkg.Metadata.Name != "demo" {
		t.Fatalf("package archive not received: %+v", e.f.pkg)
	}
	if len(u.Values.Files) != 1 || string(u.Values.Files[0]) != "replicas: 3\n" ||
		u.Values.Set[0] != "a=1" || u.Values.SetString[0] != "b=007" {
		t.Fatalf("values: %+v", u.Values)
	}
	for _, want := range []string{`Release "r" upgraded (revision 2).`, "update workload web (replace)",
		"delete volume cache (its data is deleted)", "NOTES:\nr (revision 2) runs nginx:2."} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "net") || !strings.Contains(e.stderr, "Warning: workload web keeps image VOLUME") {
		t.Fatalf("unchanged resources listed or warning missing:\n%s\n%s", out, e.stderr)
	}
}

func TestDryRunPlan(t *testing.T) {
	e := newCLIEnv(t)
	res := installed()
	res.DryRun = true
	e.f.result = res
	out := e.mustRun("install", "r", demo, "--dry-run")
	for _, want := range []string{"Deployment Plan", "Revision: 1 -> 2", "~ WORKLOAD web (replace)", "- VOLUME cache (its data is deleted)",
		"0 to create, 1 to update, 1 to delete, 1 unchanged."} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if out := e.mustRun("install", "r", demo, "--dry-run", "--verbose"); !strings.Contains(out, `spec.image: "nginx:1" -> "nginx:2"`) {
		t.Fatalf("verbose:\n%s", out)
	}
	if !e.f.install.DryRun {
		t.Fatal("dry run not sent")
	}
}

func TestProgressAndErrors(t *testing.T) {
	e := newCLIEnv(t)
	e.f.events = []api.Event{{Kind: "Workload", Name: "web", Phase: api.PhaseCreate, Detail: "creating r-web"}}
	e.f.result = installed()
	e.f.err = errors.New("Workload \"web\": image nginx:9 was not found")
	_, err := e.run("rollback", "r", "1")
	if err == nil || !strings.Contains(err.Error(), "image nginx:9 was not found") || e.f.rollback.Revision != 1 {
		t.Fatalf("error from the server: %v", err)
	}
	if !strings.Contains(e.stderr, "workload web: creating r-web") {
		t.Fatalf("progress on stderr:\n%s", e.stderr)
	}
	if _, err := e.run("upgrade", "x", demo, "--strategy", "rolling"); err == nil || !strings.Contains(err.Error(), `invalid --strategy: "rolling" must be recreate or replace`) {
		t.Fatalf("strategy checked locally: %v", err)
	}
	if _, err := e.run("install", "d", "does/not/exist"); err == nil || !strings.Contains(err.Error(), "does/not/exist") {
		t.Fatalf("missing package: %v", err)
	}
	if _, err := e.run("list", "-o", "xml"); err == nil || !strings.Contains(err.Error(), `invalid --output "xml"`) {
		t.Fatalf("output: %v", err)
	}
}

func TestStatusTable(t *testing.T) {
	e := newCLIEnv(t)
	e.f.status = &api.StatusReport{
		Release: api.Release{Name: "r", Status: api.StatusDeployed, CurrentRevision: 3, PackageName: "shop", PackageVersion: "1.0.0", UpdatedAt: time.Now()},
		Resources: []api.ResourceStatus{
			{Kind: "Workload", Name: "web", RuntimeName: "r-web", Revision: 3, Desired: "applied", Actual: "running", Health: "healthy", Restarts: 4, OOMKilled: true},
			{Kind: "Service", Name: "web", RuntimeName: "r-karga-proxy", Revision: 3, Desired: "applied", Actual: "routed", Backend: "karga-web-r3"},
		},
		Orphans:   []string{"workload/r-old"},
		Reconcile: &api.ReconcileStatus{Last: time.Now(), Status: "succeeded", GaveUp: map[string]string{"Workload/web": "boom"}},
	}
	out := e.mustRun("status", "r")
	for _, want := range []string{"Status:   deployed", "Revision: 3", "Repaired: ", "succeeded", "healthy, oom-killed", "-> karga-web-r3",
		"Not tracked by karga: workload/r-old", "gave up repairing", "Workload/web: boom"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	e.f.status.Release.Paused = true
	if out := e.mustRun("status", "r"); !strings.Contains(out, "Repaired: paused (karga resume r)") {
		t.Fatalf("paused:\n%s", out)
	}
	var js api.StatusReport
	if err := json.Unmarshal([]byte(e.mustRun("status", "r", "-o", "json")), &js); err != nil || js.Resources[0].Restarts != 4 {
		t.Fatalf("json: %+v %v", js, err)
	}
}

func TestColorsAlignTables(t *testing.T) {
	e := newCLIEnv(t)
	e.f.releases = []api.Release{{Name: "web", Status: api.StatusDeployed, CurrentRevision: 1}, {Name: "db", Status: api.StatusFailed, CurrentRevision: 2}}
	plain := e.mustRun("list", "--color", "never")
	colored := e.mustRun("list", "--color", "always")
	if !strings.Contains(colored, "\x1b[") {
		t.Fatal("no colors with --color always")
	}
	if stripped := regexp.MustCompile("\x1b\\[[0-9;]*m").ReplaceAllString(colored, ""); stripped != plain {
		t.Fatalf("colored table misaligned:\n%s\n---\n%s", stripped, plain)
	}
}

func TestHistoryAndDiff(t *testing.T) {
	e := newCLIEnv(t)
	e.f.history = []api.Revision{
		{Release: "r", Number: 1, Status: api.RevisionDeployed, Description: "Install complete"},
		{Release: "r", Number: 2, Status: api.RevisionDeployed, Description: "Upgrade complete"},
	}
	e.f.diff = &api.RevisionDiff{Release: "r", From: 1, To: 2,
		Values:    []api.FieldChange{{Path: "tag", Old: `"a"`, New: `"b"`}},
		Resources: []api.ResourceDiff{{Kind: "Workload", Name: "web", Action: api.ActionUpdate, Fields: []api.FieldChange{{Path: "spec.image", Old: `"a"`, New: `"b"`}}}}}
	if out := e.mustRun("history", "r", "--diff"); !strings.Contains(out, "first") || !strings.Contains(out, "+0 ~1 -0, 1 values") {
		t.Fatalf("history --diff:\n%s", out)
	}
	out := e.mustRun("diff", "r", "1")
	for _, want := range []string{"revision 1 -> 2", `tag: "a" -> "b"`, "~ WORKLOAD web", `spec.image: "a" -> "b"`} {
		if !strings.Contains(out, want) {
			t.Errorf("diff misses %q:\n%s", want, out)
		}
	}
	if _, err := e.run("diff", "r", "x"); err == nil {
		t.Fatal("a revision must be a number")
	}
}

func TestGetCommands(t *testing.T) {
	e := newCLIEnv(t)
	if out := e.mustRun("get", "values", "r"); !strings.Contains(out, "password: <redacted>") || !strings.Contains(out, "replicas: 2") {
		t.Fatalf("values:\n%s", out)
	}
	if out := e.mustRun("get", "notes", "r"); !strings.Contains(out, "notes here") {
		t.Fatalf("notes:\n%s", out)
	}
	if out := e.mustRun("get", "manifest", "r"); !strings.Contains(out, "kind: Network") {
		t.Fatalf("manifest:\n%s", out)
	}
}

func TestStreams(t *testing.T) {
	e := newCLIEnv(t)
	e.f.logs = "web | line 1\nweb | line 2\n"
	if out := e.mustRun("logs", "r", "web", "--tail", "2"); out != e.f.logs {
		t.Fatalf("logs: %q", out)
	}

	// exec: stdin goes in, stdout and stderr come back, the exit code becomes karga's.
	var out, errOut bytes.Buffer
	cmd := e.root(&out, &errOut)
	cmd.SetIn(strings.NewReader("hello"))
	cmd.SetArgs([]string{"exec", "r", "web", "-i", "--", "cat"})
	e.f.exitCode = 3
	err := cmd.Execute()
	var exit *ExitCodeError
	if !errors.As(err, &exit) || exit.Code != 3 || out.String() != "out:hello" || errOut.String() != "to stderr" || e.f.execCmd[0] != "cat" {
		t.Fatalf("exec: %v %q %q %v", err, out.String(), errOut.String(), e.f.execCmd)
	}

	// Backups are written by the CLI: 0600, never over an existing file.
	dir := t.TempDir()
	e.f.backup = "TARBALL"
	file := filepath.Join(dir, "data.tar.gz")
	if out := e.mustRun("volume", "backup", "r", "data", file, "--stop"); !strings.Contains(out, "written to "+file) {
		t.Fatalf("backup: %s", out)
	}
	if info, err := os.Stat(file); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("backup file: %v %v", info, err)
	}
	if b, _ := os.ReadFile(file); string(b) != "TARBALL" {
		t.Fatalf("backup content: %q", b)
	}
	if _, err := e.run("volume", "backup", "r", "data", file); err == nil {
		t.Fatal("backup over an existing file must fail")
	}
	if out := e.mustRun("volume", "restore", "--yes", "r", "data", file); !strings.Contains(out, "restored from") || e.f.restored != "TARBALL" {
		t.Fatalf("restore: %s %q", out, e.f.restored)
	}
	e.f.err = errors.New("volume data is not mounted")
	failed := filepath.Join(dir, "failed.tar.gz")
	if _, err := e.run("volume", "backup", "r", "data", failed); err == nil || !strings.Contains(err.Error(), "not mounted") {
		t.Fatalf("failed backup: %v", err)
	}
	if _, err := os.Stat(failed); err == nil {
		t.Fatal("a failed backup leaves no file")
	}
}

func TestGatewayCommands(t *testing.T) {
	e := newCLIEnv(t)
	e.f.health = api.State{Container: "running", Ready: true, Version: "1.36.13", Listen: []string{"127.0.0.1"}}
	e.f.exposed = []api.Exposed{{Listener: "web:80", Route: "shop.local/", Release: "shop", Service: "web:80", Upstream: "127.0.0.1:4001"}}
	out := e.mustRun("gateway", "status")
	for _, want := range []string{"running, ready (Envoy 1.36.13)", "Listen:     127.0.0.1", "Access log: off", "web:80", "shop.local/", "127.0.0.1:4001"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	e.f.health = api.State{Container: "missing", Listen: []string{"127.0.0.1"}}
	e.f.exposed = nil
	if out := e.mustRun("gateway", "status", "-o", "json"); !strings.Contains(out, `"routes": []`) {
		t.Fatalf("json:\n%s", out)
	}
	e.f.settings = api.Settings{Listen: []string{"127.0.0.1"}, Listeners: []api.Listener{{Name: "web", Port: 80, Protocol: api.ProtocolHTTP}, {Name: "websecure", Port: 443, Protocol: api.ProtocolHTTPS}}}
	e.f.users = map[string][]string{"web": {"shop/web"}}
	if out := e.mustRun("gateway", "listener", "list"); !strings.Contains(out, "web        80    http      shop/web") {
		t.Fatalf("listeners:\n%s", out)
	}
	e.mustRun("gateway", "listener", "add", "pg", "--port", "5432", "--protocol", "tcp")
	e.mustRun("gateway", "listener", "remove", "pg", "--yes")
	e.mustRun("gateway", "set", "listen", "10.0.0.5,127.0.0.1", "--yes")
	e.mustRun("gateway", "set", "access-log", "on")
	if got := strings.Join(e.f.calls, ","); got != "add pg,remove pg,listen,access-log on" || len(e.f.settings.Listen) != 2 {
		t.Fatalf("calls: %s %v", got, e.f.settings.Listen)
	}
	if _, err := e.run("gateway", "set", "access-log", "maybe"); err == nil {
		t.Fatal("invalid value accepted")
	}
}

func TestReconcileCommands(t *testing.T) {
	e := newCLIEnv(t)
	e.f.recon = &api.ReconcileResult{Release: "r", Actions: []api.Step{{Kind: "Workload", Name: "web", Action: "start", Status: "done"}}}
	if out := e.mustRun("reconcile", "r"); !strings.Contains(out, "start workload web: done") {
		t.Fatalf("reconcile:\n%s", out)
	}
	e.f.recon = &api.ReconcileResult{Release: "r", Skipped: "paused"}
	if out := e.mustRun("reconcile", "r"); !strings.Contains(out, "not reconciled: paused") {
		t.Fatalf("skipped:\n%s", out)
	}
	e.f.recon = &api.ReconcileResult{Release: "r"}
	if out := e.mustRun("reconcile", "r"); !strings.Contains(out, "nothing to repair") {
		t.Fatalf("nothing:\n%s", out)
	}
	e.mustRun("pause", "r")
	e.mustRun("resume", "r")
	e.f.ops = []api.Operation{
		{ID: 2, Release: "r", Type: "reconcile", Revision: 1, Status: api.OperationSucceeded, StartedAt: time.Now(),
			Steps: []api.Step{{Kind: "Workload", Name: "web", Action: "start"}}},
		{ID: 1, Release: "r", Type: "install", Revision: 1, Status: api.OperationSucceeded, StartedAt: time.Now().Add(-time.Minute), Actor: "alice"},
	}
	out := e.mustRun("events", "r")
	if !strings.Contains(out, "USER") || !strings.Contains(out, "alice") || strings.Index(out, "install") > strings.Index(out, "reconcile") || !strings.Contains(out, "start workload web") {
		t.Fatalf("events, oldest first:\n%s", out)
	}
}

func TestDynamicCompletion(t *testing.T) {
	e := newCLIEnv(t)
	complete := func(args ...string) string {
		t.Helper()
		var out bytes.Buffer
		cmd := e.root(&out, &bytes.Buffer{})
		cmd.SetArgs(append([]string{"__complete"}, args...))
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		var vals []string
		for _, l := range strings.Split(out.String(), "\n") {
			if l != "" && !strings.HasPrefix(l, ":") && !strings.HasPrefix(l, "Completion ended") {
				vals = append(vals, strings.SplitN(l, "\t", 2)[0])
			}
		}
		return strings.Join(vals, " ")
	}
	e.f.releases = []api.Release{{Name: "redis1"}, {Name: "web1"}}
	e.f.applied = []api.AppliedResource{{Kind: "Workload", Name: "api"}, {Kind: "Workload", Name: "web"}, {Kind: "Volume", Name: "data"}}
	e.f.history = []api.Revision{{Number: 1, Status: api.RevisionDeployed}, {Number: 2, Status: api.RevisionDeployed}}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"status", ""}, "redis1 web1"},
		{[]string{"status", "w"}, "web1"},
		{[]string{"logs", "web1", ""}, "api web"},
		{[]string{"volume", "backup", "web1", ""}, "data"},
		{[]string{"rollback", "web1", ""}, "2 1"},
		{[]string{"events", ""}, "redis1 web1"},
	} {
		if got := complete(tc.args...); got != tc.want {
			t.Errorf("complete %v: got %q, want %q", tc.args, got, tc.want)
		}
	}
}

func TestServerNotRunning(t *testing.T) {
	out, _, err := run(t, "--socket", filepath.Join(t.TempDir(), "none.sock"), "list")
	if err == nil || !strings.Contains(err.Error(), "karga-server is not running") || !strings.Contains(err.Error(), "systemctl start karga-server") {
		t.Fatalf("list without a server: %v\n%s", err, out)
	}
	// Package commands need no server.
	if _, _, err := run(t, "--socket", filepath.Join(t.TempDir(), "none.sock"), "lint", demo); err != nil {
		t.Fatalf("lint without a server: %v", err)
	}
}

func TestExitCodes(t *testing.T) {
	e := newCLIEnv(t)
	code := func(args ...string) int {
		t.Helper()
		_, err := e.run(args...)
		return ExitCode(err)
	}
	e.f.status = &api.StatusReport{Release: api.Release{Name: "r", Status: api.StatusDeployed},
		Resources: []api.ResourceStatus{{Kind: "Workload", Name: "web", Desired: "applied", Actual: "running", Health: "healthy"}}}
	if c := code("status", "r"); c != 0 {
		t.Fatalf("healthy status: %d", c)
	}
	e.f.status.Resources[0].Health = "unhealthy"
	if c := code("status", "r"); c != ExitUnhealthy {
		t.Fatalf("unhealthy status: %d", c)
	}
	for _, args := range [][]string{{"status"}, {"list", "--nope"}, {"nope"}} {
		if c := code(args...); c != ExitUsage {
			t.Errorf("%v: %d, want %d", args, c, ExitUsage)
		}
	}
	e.f.err = fmt.Errorf("release x: %w", api.ErrNotFound)
	if c := code("list"); c != ExitNotFound {
		t.Fatalf("not found: %d", c)
	}
	e.f.err = fmt.Errorf("%w: busy", api.ErrConflict)
	if c := code("list"); c != ExitConflict {
		t.Fatalf("conflict: %d", c)
	}
	e.f.err = fmt.Errorf("%w: karga-server (API 2.0) does not know field x; upgrade karga-server", api.ErrIncompatible)
	if c := code("list"); c != ExitUnavailable {
		t.Fatalf("incompatible: %d", c)
	}
	e.f.err = errors.New("boom")
	if c := code("list"); c != ExitFailure {
		t.Fatalf("other failure: %d", c)
	}
	_, _, err := run(t, "--socket", filepath.Join(t.TempDir(), "none.sock"), "list")
	if c := ExitCode(err); c != ExitUnavailable {
		t.Fatalf("no server: %d", c)
	}
}

// Commands that delete data or expose services need --yes when nobody can be asked.
func TestConfirmations(t *testing.T) {
	e := newCLIEnv(t)
	e.f.result = installed()
	for _, args := range [][]string{
		{"uninstall", "r"},
		{"uninstall", "r", "--delete-volumes"},
		{"gateway", "set", "listen", "0.0.0.0"},
		{"gateway", "listener", "remove", "web"},
		{"upgrade", "r", demo, "--delete-volumes"},
		{"rollback", "r", "--delete-volumes"},
	} {
		if _, err := e.run(args...); err == nil || !strings.Contains(err.Error(), "--yes") || ExitCode(err) != ExitUsage {
			t.Errorf("%v: want a refusal asking for --yes, got %v", args, err)
		}
		if _, err := e.run(append(args, "--yes")...); err != nil {
			t.Errorf("%v --yes: %v", args, err)
		}
	}
	// Nothing to confirm: a dry run, loopback addresses.
	for _, args := range [][]string{{"uninstall", "r", "--dry-run"}, {"gateway", "set", "listen", "127.0.0.1, ::1"}, {"upgrade", "r", demo, "--delete-volumes", "--dry-run"}} {
		if _, err := e.run(args...); err != nil {
			t.Errorf("%v: %v", args, err)
		}
	}
	if got := e.f.settings.Listen; len(got) != 2 || got[1] != "::1" {
		t.Errorf("listen addresses are trimmed: %q", got)
	}
}

// Every command speaks json and yaml; streams write one document per item.
func TestStructuredOutput(t *testing.T) {
	e := newCLIEnv(t)
	e.f.status = &api.StatusReport{Release: api.Release{Name: "r", Status: api.StatusDeployed}}
	e.f.ops = []api.Operation{{ID: 2, Release: "r", Type: "upgrade"}, {ID: 1, Release: "r", Type: "install"}}
	for _, args := range [][]string{
		{"pause", "r"}, {"resume", "r"}, {"gateway", "set", "access-log", "on"},
		{"gateway", "listener", "add", "pg", "--port", "5432", "--protocol", "tcp"},
		{"gateway", "listener", "remove", "pg", "--yes"}, {"uninstall", "r", "--yes"},
		{"status", "r"}, {"events", "r"}, {"template", "web", demo}, {"show", "readme", demo},
	} {
		out, err := e.run(append(args, "-o", "json")...)
		var v any
		if err != nil || json.Unmarshal([]byte(out), &v) != nil {
			t.Errorf("%v -o json: %v\n%s", args, err, out)
		}
		out, err = e.run(append(args, "-o", "yaml")...)
		if err != nil || yaml.Unmarshal([]byte(out), &v) != nil || strings.HasPrefix(out, "{") {
			t.Errorf("%v -o yaml: %v\n%s", args, err, out)
		}
	}
	// --watch: one JSON document per line, nothing else.
	out := e.mustRun("status", "r", "--watch", "--count", "2", "--interval", "1ms", "-o", "json")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("status --watch -o json:\n%s", out)
	}
	for _, l := range lines {
		var rep api.StatusReport
		if err := json.Unmarshal([]byte(l), &rep); err != nil || rep.Release.Name != "r" {
			t.Fatalf("line %q: %v", l, err)
		}
	}
	// events: oldest first.
	var ops []api.Operation
	if err := json.Unmarshal([]byte(e.mustRun("events", "r", "-o", "json")), &ops); err != nil || ops[0].ID != 1 {
		t.Fatalf("events order: %+v %v", ops, err)
	}
}
