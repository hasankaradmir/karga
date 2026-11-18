package cli

import (
	"context"
	"errors"
	"strings"

	"github.com/spf13/cobra"

	"github.com/hasankaradmir/karga-sdk/api"
)

// Exit codes, documented in USAGE.md: scripts can tell why karga failed.
const (
	ExitFailure     = 1 // any other failure
	ExitUsage       = 2 // wrong arguments or flags
	ExitUnavailable = 3 // karga-server is not running, does not answer in time or speaks another API
	ExitNotFound    = 4 // the release, revision or resource does not exist
	ExitConflict    = 5 // another operation holds the release, or it exists already
	ExitUnhealthy   = 6 // karga status: the release is not deployed and healthy
)

// usageError marks wrong arguments or flags.
type usageError struct{ error }

func (e usageError) Unwrap() error { return e.error }

// errUnhealthy is returned by karga status when the release is not as it should be.
var errUnhealthy = errors.New("the release is not deployed and healthy")

// ExitCode is the exit status for err, the error a command returned (0 for nil).
func ExitCode(err error) int {
	var exit *ExitCodeError
	var usage usageError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit):
		return exit.Code
	case errors.As(err, &usage), strings.HasPrefix(err.Error(), "unknown command"):
		return ExitUsage
	case errors.Is(err, api.ErrNotRunning), errors.Is(err, api.ErrIncompatible), errors.Is(err, api.ErrDisconnected),
		errors.Is(err, context.DeadlineExceeded):
		return ExitUnavailable
	case errors.Is(err, api.ErrNotFound):
		return ExitNotFound
	case errors.Is(err, api.ErrConflict):
		return ExitConflict
	case errors.Is(err, errUnhealthy):
		return ExitUnhealthy
	}
	return ExitFailure
}

// markUsageErrors makes argument and flag errors of every command usage errors.
func markUsageErrors(root *cobra.Command) {
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError{err} })
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		if args := c.Args; args != nil {
			c.Args = func(cmd *cobra.Command, a []string) error {
				if err := args(cmd, a); err != nil {
					return usageError{err}
				}
				return nil
			}
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
}
