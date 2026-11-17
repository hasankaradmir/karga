package cli

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	cmd := NewRootCmd(&out, &errOut)
	cmd.SetArgs(args)
	err = cmd.Execute()
	return out.String(), errOut.String(), err
}

func TestVersionTable(t *testing.T) {
	out, _, err := run(t, "version")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "karga dev (commit ") {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestVersionJSON(t *testing.T) {
	out, _, err := run(t, "version", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var v versionReport
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("invalid JSON %q: %v", out, err)
	}
	if v.Version != "dev" || v.GoVersion == "" {
		t.Fatalf("unexpected version info: %+v", v)
	}
}

func TestInvalidLogFormat(t *testing.T) {
	if _, _, err := run(t, "--log-format", "xml", "version"); err == nil {
		t.Fatal("expected error for --log-format xml")
	}
}

func TestDebugLogsToStderr(t *testing.T) {
	out, errOut, err := run(t, "--debug", "--socket", "/tmp/karga-test.sock", "version", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut, "socket=/tmp/karga-test.sock") {
		t.Fatalf("debug log missing on stderr: %q", errOut)
	}
	if strings.Contains(out, "karga-server") && strings.Contains(out, "socket=") {
		t.Fatalf("log leaked to stdout: %q", out)
	}
}

func TestInvalidOutput(t *testing.T) {
	if _, _, err := run(t, "version", "-o", "yaml"); err == nil {
		t.Fatal("expected error for -o yaml")
	}
}

func TestVersionAsksTheServer(t *testing.T) {
	e := newCLIEnv(t)
	if out := e.mustRun("version"); !strings.Contains(out, "karga-server dev") {
		t.Fatalf("version:\n%s", out)
	}
	out, _, err := run(t, "--socket", filepath.Join(t.TempDir(), "none.sock"), "version")
	if err != nil || !strings.Contains(out, "karga-server not running") {
		t.Fatalf("without a server: %v\n%s", err, out)
	}
	if _, _, err := run(t, "--socket", filepath.Join(t.TempDir(), "none.sock"), "list"); err == nil || !strings.Contains(err.Error(), "karga-server is not running") {
		t.Fatalf("list without a server: %v", err)
	}
}
