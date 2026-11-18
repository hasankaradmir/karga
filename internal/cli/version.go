package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/hasankaradmir/karga-sdk/api"

	"github.com/hasankaradmir/karga-cli/internal/version"
)

type versionReport struct {
	version.Info
	// API is the CLI's API version, Major.Minor.
	API string `json:"api"`
	// Server is karga-server's, nil when it does not answer; ServerError then says why.
	Server      *api.ServerVersion `json:"server,omitempty"`
	ServerError string             `json:"serverError,omitempty"`
}

func newVersionCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the versions of karga and karga-server",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			v := versionReport{Info: version.Current(), API: api.Version}
			if c, ok := a.backend().(interface {
				Version(context.Context) (api.ServerVersion, error)
			}); ok {
				ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Second)
				if sv, err := c.Version(ctx); err == nil {
					v.Server = &sv
				} else if errors.Is(err, api.ErrNotRunning) {
					v.ServerError = "not running (" + a.socket + ")"
				} else {
					v.ServerError = err.Error() // e.g. permission denied on the socket
				}
				cancel()
			}
			var mismatch error
			if v.Server != nil && !api.Compatible(v.Server.API) {
				mismatch = fmt.Errorf("%w: karga speaks API %s and karga-server API %s: upgrade the older one", api.ErrIncompatible, v.API, v.Server.API)
			}
			if a.structured() {
				if err := a.write(v); err != nil {
					return err
				}
				return mismatch
			}
			commit := v.Commit
			if len(commit) > 12 {
				commit = commit[:12]
			}
			if _, err := fmt.Fprintf(a.stdout, "karga %s (commit %s, %s, API %s)\n", v.Version, commit, v.GoVersion, v.API); err != nil {
				return err
			}
			server := v.ServerError
			if v.Server != nil {
				server = fmt.Sprintf("%s (API %s)", v.Server.Version, v.Server.API)
			}
			if _, err := fmt.Fprintf(a.stdout, "karga-server %s\n", server); err != nil {
				return err
			}
			return mismatch
		},
	}
}
