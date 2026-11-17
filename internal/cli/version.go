package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/hasankaradmir/karga/internal/version"
)

type versionReport struct {
	version.Info
	// Server is karga-server's version, empty when it does not answer.
	Server string `json:"server,omitempty"`
}

func newVersionCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the versions of karga and karga-server",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			v := versionReport{Info: version.Current()}
			if c, ok := a.backend().(interface {
				Version(context.Context) (string, error)
			}); ok {
				ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Second)
				v.Server, _ = c.Version(ctx)
				cancel()
			}
			if a.output == outputJSON {
				return writeJSON(a.stdout, v)
			}
			commit := v.Commit
			if len(commit) > 12 {
				commit = commit[:12]
			}
			if _, err := fmt.Fprintf(a.stdout, "karga %s (commit %s, %s)\n", v.Version, commit, v.GoVersion); err != nil {
				return err
			}
			server := v.Server
			if server == "" {
				server = "not running (" + a.socket + ")"
			}
			if _, err := fmt.Fprintf(a.stdout, "karga-server %s\n", server); err != nil {
				return err
			}
			if v.Server != "" && v.Server != v.Version {
				_, err := fmt.Fprintf(a.stderr, "Warning: karga %s and karga-server %s differ; use the same version of both\n", v.Version, v.Server)
				return err
			}
			return nil
		},
	}
}
