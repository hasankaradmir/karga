package cli

import (
	"net"
	"strconv"
	"strings"
	"testing"
)

// TestErrorMessages pins the messages users see for common mistakes.
func TestErrorMessages(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = busy.Close() }()
	busyPort := strconv.Itoa(busy.Addr().(*net.TCPAddr).Port)

	tests := []struct {
		name string
		args []string
		want []string
	}{
		{"port taken", []string{"install", "a", redis, "--set", "network.port=" + busyPort},
			[]string{"hostPort 127.0.0.1:" + busyPort + "/tcp is taken by another process on this host", "nothing was installed"}},
		{"image missing", []string{"install", "b", redis, "--set", "image.repository=missing/redis"},
			[]string{"missing/redis:7.4", "image not found"}},
		{"schema", []string{"install", "c", redis, "--set", "network.port=abc"},
			[]string{"values do not match values.schema.json", "/network/port", "want integer"}},
		{"unknown release", []string{"upgrade", "nope", redis}, []string{"release nope: not found", "--install"}},
		{"bad release name", []string{"install", "Bad_Name", redis}, []string{"must be a DNS label"}},
		{"strategy", []string{"upgrade", "x", redis, "--strategy", "rolling"}, []string{`invalid --strategy "rolling"`}},
		{"output", []string{"list", "-o", "yaml"}, []string{`invalid --output "yaml"`}},
		{"missing package", []string{"install", "d", "does/not/exist"}, []string{"does/not/exist", "no such file or directory"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newCLIEnv(t)
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
