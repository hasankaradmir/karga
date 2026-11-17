package cli

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const leak = "hunter2-LEAK"

// TestSecretsDoNotLeak runs a postgres release through every command and checks that the password
// appears nowhere except the materialized secret files: not in any output, log or state file.
func TestSecretsDoNotLeak(t *testing.T) {
	e := newCLIEnv(t)
	pg := "../../tests/testdata/postgres"
	set := []string{"--set", "service.enabled=false", "--set", "database.password=" + leak}
	debug := []string{"--debug", "--log-format", "json"}

	cmds := [][]string{
		append([]string{"template", "db", pg}, set...),
		append(append([]string{"install", "db", pg}, set...), debug...),
		append(append([]string{"upgrade", "db", pg, "--dry-run", "--set", "resources.memory=2Gi"}, set...), debug...),
		append(append([]string{"upgrade", "db", pg, "--set", "resources.memory=2Gi"}, set...), debug...),
		append([]string{"upgrade", "db", pg, "--reuse-values", "--set", "database.password=" + leak + "2"}, debug...),
		{"history", "db"},
		{"status", "db"},
		{"list"},
		{"get", "values", "db"},
		{"get", "values", "db", "--all"},
		{"get", "manifest", "db"},
		append([]string{"rollback", "db", "1"}, debug...),
	}
	for _, args := range cmds {
		for _, out := range []string{"table", "json"} {
			if args[0] == "template" && out == "json" {
				continue
			}
			stdout, stderr, err := e.runBoth(append(args, "-o", out)...)
			if err != nil {
				t.Fatalf("karga %v: %v\n%s", args, err, stderr)
			}
			if strings.Contains(stdout+stderr, leak) {
				t.Fatalf("karga %v -o %s leaked the secret:\nstdout:\n%s\nstderr:\n%s", args, out, stdout, stderr)
			}
			if args[0] == "upgrade" && args[3] != "--dry-run" && out == "table" {
				break // one real upgrade per variant is enough
			}
		}
	}

	var secretFiles int
	err := filepath.WalkDir(e.home, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if !bytes.Contains(b, []byte(leak)) {
			return nil
		}
		if strings.Contains(filepath.ToSlash(p), "/secrets/postgres-credentials/") {
			secretFiles++
			return nil
		}
		t.Errorf("secret found in %s", p)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if secretFiles == 0 {
		t.Fatal("expected the password in the materialized secret files")
	}
	if _, err := os.Stat(filepath.Join(e.home, "karga.db")); err != nil {
		t.Fatalf("state database not checked: %v", err)
	}
}

func (e *cliEnv) runBoth(args ...string) (string, string, error) {
	e.t.Helper()
	var out, errOut bytes.Buffer
	cmd := e.root(&out, &errOut)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), errOut.String(), err
}
