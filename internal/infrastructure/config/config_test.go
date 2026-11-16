package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolve(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		flag string
		env  map[string]string
		want string
	}{
		{"flag wins", "/f", map[string]string{"KARGA_HOME": "/k", "XDG_DATA_HOME": "/x", "HOME": "/h"}, "/f"},
		{"KARGA_HOME", "", map[string]string{"KARGA_HOME": "/k", "XDG_DATA_HOME": "/x", "HOME": "/h"}, "/k"},
		{"XDG_DATA_HOME", "", map[string]string{"XDG_DATA_HOME": "/x", "HOME": "/h"}, "/x/karga"},
		{"HOME fallback", "", map[string]string{"HOME": "/h"}, "/h/.local/share/karga"},
		{"relative made absolute", "rel/../state", nil, filepath.Join(cwd, "state")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Resolve(tt.flag, func(k string) string { return tt.env[k] })
			if err != nil {
				t.Fatal(err)
			}
			if got.Home != tt.want {
				t.Fatalf("Home = %q, want %q", got.Home, tt.want)
			}
		})
	}
}

func TestResolveNothingSet(t *testing.T) {
	if _, err := Resolve("", func(string) string { return "" }); err == nil {
		t.Fatal("expected error")
	}
}

func TestEnsureHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "a", "karga")
	if err := EnsureHome(Config{Home: home}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(home)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("home: %v %v", info.Mode(), err)
	}
	if err := EnsureHome(Config{Home: home}); err != nil {
		t.Fatalf("second call: %v", err)
	}
}
