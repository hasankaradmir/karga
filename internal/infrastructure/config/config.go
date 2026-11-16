// Package config resolves karga's runtime configuration.
package config

import (
	"errors"
	"path/filepath"
)

// Config is the resolved configuration for one invocation.
type Config struct {
	// Home is the absolute karga state directory. It is not created here.
	Home string
}

// Resolve picks the state directory: flagHome > KARGA_HOME > $XDG_DATA_HOME/karga > $HOME/.local/share/karga.
func Resolve(flagHome string, getenv func(string) string) (Config, error) {
	home := flagHome
	switch {
	case home != "":
	case getenv("KARGA_HOME") != "":
		home = getenv("KARGA_HOME")
	case getenv("XDG_DATA_HOME") != "":
		home = filepath.Join(getenv("XDG_DATA_HOME"), "karga")
	case getenv("HOME") != "":
		home = filepath.Join(getenv("HOME"), ".local", "share", "karga")
	default:
		return Config{}, errors.New("cannot resolve karga home: set --home or KARGA_HOME")
	}
	abs, err := filepath.Abs(home)
	if err != nil {
		return Config{}, err
	}
	return Config{Home: abs}, nil
}
