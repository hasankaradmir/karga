package cli

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// demo is the application-layer fixture: Volume, Config, Workload and an optional Secret.
const demo = "testdata/demo"

func TestTemplateCmd(t *testing.T) {
	out, _, err := run(t, "template", "web", demo, "--set", "image.tag=1.28", "--set", "password=hunter2")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# Source: demo/templates/workload.yaml", `image: "nginx:1.28"`, "password: <redacted>"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "hunter2") {
		t.Fatal("secret leaked without --show-secrets")
	}

	out, _, err = run(t, "template", "web", demo, "--set", "password=hunter2", "--show-secrets")
	if err != nil || !strings.Contains(out, "hunter2") {
		t.Fatalf("--show-secrets: err=%v\n%s", err, out)
	}
}

func TestTemplateCmdErrors(t *testing.T) {
	cases := [][]string{
		{"template", "web", demo, "-o", "xml"},
		{"template", "web", demo, "--set", "image.tag=1"},
		{"template", "web", demo, "--set", "typo=x", "--strict"},
		{"template", "web"},
	}
	for _, args := range cases {
		if _, _, err := run(t, args...); err == nil {
			t.Errorf("%v: expected error", args)
		}
	}
}

func TestLintCmd(t *testing.T) {
	out, _, err := run(t, "lint", demo)
	if err != nil {
		t.Fatalf("lint demo: %v\n%s", err, out)
	}
	if !strings.Contains(out, "1 package(s) linted, 0 failed") {
		t.Fatalf("summary missing:\n%s", out)
	}

	out, _, err = run(t, "lint", demo, "does-not-exist", "-o", "json")
	if err == nil {
		t.Fatal("expected failure")
	}
	var res []lintResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if len(res) != 2 || len(res[0].Errors) != 0 || len(res[1].Errors) != 1 {
		t.Fatalf("unexpected results: %+v", res)
	}
}

func TestShowCmd(t *testing.T) {
	out, _, err := run(t, "show", "package", demo)
	if err != nil || !strings.Contains(out, "name: demo") || !strings.Contains(out, "version: 1.2.3") {
		t.Fatalf("show package: %v\n%s", err, out)
	}
	out, _, err = run(t, "show", "values", demo)
	if err != nil || !strings.HasPrefix(out, "# Demo values.") {
		t.Fatalf("show values should keep comments: %v\n%s", err, out)
	}
	out, _, err = run(t, "show", "package", demo, "-o", "json")
	if err != nil || !strings.Contains(out, `"apiVersion": "karga.io/v1alpha1"`) {
		t.Fatalf("show package json: %v\n%s", err, out)
	}
}

// The scaffold karga create writes must be a working package.
func TestCreateCmd(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shop")
	if _, _, err := run(t, "create", dir); err != nil {
		t.Fatal(err)
	}
	if out, _, err := run(t, "lint", dir); err != nil {
		t.Fatalf("scaffold does not lint clean: %v\n%s", err, out)
	}
	if out, _, err := run(t, "template", "shop", dir); err != nil || !strings.Contains(out, "kind: Workload") {
		t.Fatalf("scaffold does not render: %v\n%s", err, out)
	}
	if _, _, err := run(t, "create", dir); err == nil {
		t.Fatal("create must refuse an existing directory")
	}
	if _, _, err := run(t, "create", filepath.Join(t.TempDir(), "Bad_Name")); err == nil {
		t.Fatal("create must refuse a name that is not a DNS label")
	}
}
