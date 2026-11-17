// Package cli wires karga's cobra commands. It holds no business logic.
package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/hasankaradmir/karga/internal/adapters/docker"
	"github.com/hasankaradmir/karga/internal/infrastructure/config"
	"github.com/hasankaradmir/karga/internal/infrastructure/logger"
	"github.com/hasankaradmir/karga/internal/ports"
)

const (
	outputTable = "table"
	outputJSON  = "json"
)

// app is the per-invocation state shared by all subcommands.
type app struct {
	stdout    io.Writer
	stderr    io.Writer
	output    string
	home      string
	debug     bool
	logFormat string

	cfg      config.Config
	log      *slog.Logger
	redactor *logger.Redactor

	// newRuntime opens the container runtime; tests replace it with a fake.
	newRuntime func(context.Context) (ports.Runtime, func(), error)
}

// NewRootCmd builds the karga command tree.
func NewRootCmd(stdout, stderr io.Writer) *cobra.Command {
	return newRootCmd(stdout, stderr, dockerRuntime)
}

func dockerRuntime(ctx context.Context) (ports.Runtime, func(), error) {
	rt, err := docker.New(ctx)
	if err != nil {
		return nil, nil, err
	}
	return rt, func() { _ = rt.Close() }, nil
}

func newRootCmd(stdout, stderr io.Writer, newRuntime func(context.Context) (ports.Runtime, func(), error)) *cobra.Command {
	a := &app{stdout: stdout, stderr: stderr, redactor: logger.NewRedactor(), newRuntime: newRuntime}

	root := &cobra.Command{
		Use:           "karga",
		Short:         "Package manager for container workloads on a single host",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(*cobra.Command, []string) error {
			if a.output != outputTable && a.output != outputJSON {
				return fmt.Errorf("invalid --output %q: must be %q or %q", a.output, outputTable, outputJSON)
			}
			log, err := logger.New(a.stderr, a.logFormat, a.debug, a.redactor)
			if err != nil {
				return err
			}
			a.log = log
			cfg, err := config.Resolve(a.home, os.Getenv)
			if err != nil {
				return err
			}
			a.cfg = cfg
			a.log.Debug("config resolved", "home", cfg.Home)
			return nil
		},
	}
	root.SetOut(stdout)
	root.SetErr(stderr)

	root.PersistentFlags().StringVarP(&a.output, "output", "o", outputTable, "output format: table|json")
	root.PersistentFlags().StringVar(&a.home, "home", "", "karga state directory (default $KARGA_HOME, $XDG_DATA_HOME/karga or ~/.local/share/karga)")
	root.PersistentFlags().BoolVar(&a.debug, "debug", false, "enable debug logging")
	root.PersistentFlags().StringVar(&a.logFormat, "log-format", "text", "log format on stderr: text|json")

	root.AddCommand(newVersionCmd(a), newCreateCmd(a), newTemplateCmd(a), newLintCmd(a), newShowCmd(a),
		newInstallCmd(a), newUpgradeCmd(a), newRollbackCmd(a), newUninstallCmd(a),
		newHistoryCmd(a), newListCmd(a), newStatusCmd(a), newGetCmd(a), newGatewayCmd(a), newStateCmd(a), newLogsCmd(a), newVolumeCmd(a), newExecCmd(a), newDiffCmd(a))
	return root
}
