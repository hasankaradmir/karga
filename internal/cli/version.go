package cli

import (
	"fmt"
	"runtime"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// version is set at build time: -ldflags "-X github.com/hasankaradmir/karga/internal/cli.version=v0.1.0".
var version = "dev"

type versionInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	GoVersion string `json:"goVersion"`
}

func currentVersion() versionInfo {
	v := versionInfo{Version: version, Commit: "unknown", GoVersion: runtime.Version()}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" {
				v.Commit = s.Value
			}
		}
	}
	return v
}

func newVersionCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print karga version",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			v := currentVersion()
			if a.output == outputJSON {
				return writeJSON(a.stdout, v)
			}
			commit := v.Commit
			if len(commit) > 12 {
				commit = commit[:12]
			}
			_, err := fmt.Fprintf(a.stdout, "karga %s (commit %s, %s)\n", v.Version, commit, v.GoVersion)
			return err
		},
	}
}
