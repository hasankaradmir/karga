// Package cli wires karga's cobra commands. It holds no business logic.
package cli

import (
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/hasankaradmir/karga/internal/api"
	"github.com/hasankaradmir/karga/internal/infrastructure/logger"
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
	socket    string
	debug     bool
	logFormat string
	color     string // auto, always or never

	log *slog.Logger

	// backend returns karga-server's client; tests replace it.
	backend func() api.Backend
}

// NewRootCmd builds the karga command tree.
func NewRootCmd(stdout, stderr io.Writer) *cobra.Command {
	return newRootCmd(stdout, stderr, nil)
}

func newRootCmd(stdout, stderr io.Writer, backend func() api.Backend) *cobra.Command {
	a := &app{stdout: stdout, stderr: stderr, backend: backend}
	if a.backend == nil {
		a.backend = func() api.Backend { return api.NewClient(a.socket) }
	}

	root := &cobra.Command{
		Use:           "karga",
		Short:         "Package manager for container workloads",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(*cobra.Command, []string) error {
			if a.output != outputTable && a.output != outputJSON {
				return fmt.Errorf("invalid --output %q: must be %q or %q", a.output, outputTable, outputJSON)
			}
			log, err := logger.New(a.stderr, a.logFormat, a.debug, logger.NewRedactor())
			if err != nil {
				return err
			}
			a.log = log
			if a.socket == "" {
				a.socket = api.DefaultSocket
				if s := os.Getenv("KARGA_SOCKET"); s != "" {
					a.socket = s
				}
			}
			a.log.Debug("karga-server", "socket", a.socket)
			return nil
		},
	}
	root.SetOut(stdout)
	root.SetErr(stderr)

	root.PersistentFlags().StringVarP(&a.output, "output", "o", outputTable, "output format: table|json")
	root.PersistentFlags().StringVar(&a.socket, "socket", "", "karga-server's socket (default $KARGA_SOCKET or "+api.DefaultSocket+")")
	root.PersistentFlags().BoolVar(&a.debug, "debug", false, "enable debug logging")
	root.PersistentFlags().StringVar(&a.logFormat, "log-format", "text", "log format on stderr: text|json")
	root.PersistentFlags().StringVar(&a.color, "color", "auto", "color tables: auto (a terminal without NO_COLOR), always or never")

	root.AddCommand(newVersionCmd(a), newCreateCmd(a), newTemplateCmd(a), newLintCmd(a), newShowCmd(a),
		newInstallCmd(a), newUpgradeCmd(a), newRollbackCmd(a), newUninstallCmd(a),
		newHistoryCmd(a), newListCmd(a), newStatusCmd(a), newGetCmd(a), newGatewayCmd(a), newStateCmd(a), newLogsCmd(a), newVolumeCmd(a), newExecCmd(a), newDiffCmd(a))
	root.AddCommand(newReconcileCmds(a)...)
	addCompletion(a, root)
	return root
}
