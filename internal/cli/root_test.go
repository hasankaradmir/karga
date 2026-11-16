package cli

import (
	"bytes"
	"encoding/json"
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
	var v versionInfo
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("invalid JSON %q: %v", out, err)
	}
	if v.Version != "dev" || v.GoVersion == "" {
		t.Fatalf("unexpected version info: %+v", v)
	}
}

func TestInvalidOutput(t *testing.T) {
	if _, _, err := run(t, "version", "-o", "yaml"); err == nil {
		t.Fatal("expected error for -o yaml")
	}
}
