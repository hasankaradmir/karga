// Package deployment turns a rendered manifest into runtime objects.
package deployment

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/hasankaradmir/karga-sdk/resource"
)

// Key identifies a resource inside a release.
type Key struct {
	Kind string
	Name string
}

func keyOf(r resource.Resource) Key { return Key{Kind: r.Kind, Name: r.Metadata.Name} }

// File modes for materialized Config and Secret files. The revision directory is private to the
// karga user; the mounted directory and files are world-readable so any uid inside the container
// can read them (bind mounts keep host ownership).
const (
	privateDirMode = 0o700
	mountDirMode   = 0o755
	mountFileMode  = 0o644
)

// RevisionDir is where a revision's Config and Secret files live.
func RevisionDir(home, release string, revision int) string {
	return filepath.Join(home, "releases", release, strconv.Itoa(revision))
}

// Materialize writes Config and Secret resources to host directories and returns the directory per
// resource for bind mounting. The revision directory is recreated from scratch. Every secret value
// is passed to redact so it never reaches logs.
func Materialize(home, release string, revision int, rs []resource.Resource, redact func(string)) (map[Key]string, error) {
	base := RevisionDir(home, release, revision)
	if err := os.RemoveAll(base); err != nil {
		return nil, err
	}
	for _, d := range []string{filepath.Join(home, "releases"), filepath.Dir(base), base} {
		if err := mkdirMode(d, privateDirMode); err != nil {
			return nil, err
		}
	}

	paths := map[Key]string{}
	for _, r := range rs {
		var sub string
		var files map[string]string
		switch s := r.Spec.(type) {
		case *resource.ConfigSpec:
			sub, files = "configs", s.Files
		case *resource.SecretSpec:
			sub, files = "secrets", s.Values
			for _, v := range s.Values {
				redact(v)
			}
		default:
			continue
		}
		if err := mkdirMode(filepath.Join(base, sub), privateDirMode); err != nil {
			return nil, err
		}
		dir := filepath.Join(base, sub, r.Metadata.Name)
		if err := mkdirMode(dir, mountDirMode); err != nil {
			return nil, err
		}
		for name, content := range files {
			p := filepath.Join(dir, name)
			if err := os.WriteFile(p, []byte(content), mountFileMode); err != nil {
				return nil, fmt.Errorf("write %s %s: %w", r.Kind, r.Metadata.Name, err)
			}
			// WriteFile is subject to umask; set the mode explicitly.
			if err := os.Chmod(p, mountFileMode); err != nil {
				return nil, err
			}
		}
		paths[keyOf(r)] = dir
	}
	return paths, nil
}

// mkdirMode creates dir if needed and sets its mode regardless of umask.
func mkdirMode(dir string, mode os.FileMode) error {
	if err := os.Mkdir(dir, mode); err != nil && !os.IsExist(err) {
		return err
	}
	return os.Chmod(dir, mode)
}
