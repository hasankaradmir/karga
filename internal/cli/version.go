package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/hasankaradmir/karga-sdk/api"
	"github.com/hasankaradmir/karga/internal/version"
)

type versionReport struct {
	version.Info
	// API is the CLI's API version.
	API int `json:"api"`
	// Server is karga-server's, nil when it does not answer.
	Server *api.ServerVersion `json:"server,omitempty"`
}

func newVersionCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the versions of karga and karga-server",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			v := versionReport{Info: version.Current(), API: api.Major}
			if c, ok := a.backend().(interface {
				Version(context.Context) (api.ServerVersion, error)
			}); ok {
				ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Second)
				if sv, err := c.Version(ctx); err == nil {
					v.Server = &sv
				}
				cancel()
			}
			if a.output == outputJSON {
				return writeJSON(a.stdout, v)
			}
			commit := v.Commit
			if len(commit) > 12 {
				commit = commit[:12]
			}
			if _, err := fmt.Fprintf(a.stdout, "karga %s (commit %s, %s, API %d)\n", v.Version, commit, v.GoVersion, v.API); err != nil {
				return err
			}
			server := "not running (" + a.socket + ")"
			if v.Server != nil {
				server = fmt.Sprintf("%s (API %d)", v.Server.Version, v.Server.API)
			}
			if _, err := fmt.Fprintf(a.stdout, "karga-server %s\n", server); err != nil {
				return err
			}
			if v.Server != nil && v.Server.API != v.API {
				_, err := fmt.Fprintf(a.stderr, "Warning: karga speaks API %d and karga-server API %d: upgrade the older one\n", v.API, v.Server.API)
				return err
			}
			return nil
		},
	}
}
