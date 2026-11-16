package docker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// contextHost returns the daemon host of the active docker CLI context, or "" to use the SDK default.
// The SDK only reads DOCKER_HOST; OrbStack, Docker Desktop and colima are usually selected through a context.
// Order matches the docker CLI: DOCKER_HOST, then DOCKER_CONTEXT, then currentContext in config.json.
func contextHost(getenv func(string) string) (string, error) {
	if getenv("DOCKER_HOST") != "" {
		return "", nil
	}
	dir := getenv("DOCKER_CONFIG")
	if dir == "" {
		home := getenv("HOME")
		if home == "" {
			return "", nil
		}
		dir = filepath.Join(home, ".docker")
	}
	name := getenv("DOCKER_CONTEXT")
	if name == "" {
		var cfg struct {
			CurrentContext string `json:"currentContext"`
		}
		data, err := os.ReadFile(filepath.Join(dir, "config.json"))
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		if err := json.Unmarshal(data, &cfg); err != nil {
			return "", fmt.Errorf("%s: %w", filepath.Join(dir, "config.json"), err)
		}
		name = cfg.CurrentContext
	}
	if name == "" || name == "default" {
		return "", nil
	}
	sum := sha256.Sum256([]byte(name))
	metaPath := filepath.Join(dir, "contexts", "meta", hex.EncodeToString(sum[:]), "meta.json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return "", fmt.Errorf("docker context %q: %w", name, err)
	}
	var meta struct {
		Endpoints struct {
			Docker struct {
				Host string `json:"Host"`
			} `json:"docker"`
		} `json:"Endpoints"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return "", fmt.Errorf("docker context %q: %w", name, err)
	}
	return meta.Endpoints.Docker.Host, nil
}
