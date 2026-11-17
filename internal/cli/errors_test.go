package cli

import (
	"strings"
	"testing"
)

// TestErrorMessages pins the messages users see for common mistakes.
func TestErrorMessages(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{"no gateway", []string{"install", "a", redis},
			[]string{"services redis need karga's gateway and release proxy (Linux with a local Docker)"}},
		{"image missing", []string{"install", "b", redis, "--set", "service.enabled=false", "--set", "image.repository=missing/redis"},
			[]string{"missing/redis:7.4", "image not found", "nothing was installed"}},
		{"schema", []string{"install", "c", redis, "--set", "service.port=abc"},
			[]string{"values do not match values.schema.json", "/service/port", "want integer"}},
		{"unknown release", []string{"upgrade", "nope", redis}, []string{"release nope: not found", "--install"}},
		{"bad release name", []string{"install", "Bad_Name", redis}, []string{"must be a DNS label"}},
		{"strategy", []string{"upgrade", "x", redis, "--strategy", "rolling"}, []string{`invalid --strategy "rolling"`}},
		{"output", []string{"list", "-o", "yaml"}, []string{`invalid --output "yaml"`}},
		{"missing package", []string{"install", "d", "does/not/exist"}, []string{"does/not/exist", "no such file or directory"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newCLIEnv(t)
			if tt.name == "no gateway" {
				e.rt.Capabilities = nil // Docker that is not local Linux
			}
			_, err := e.run(tt.args...)
			if err == nil {
				t.Fatal("expected an error")
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("missing %q in:\n%s", w, err)
				}
			}
		})
	}
}
