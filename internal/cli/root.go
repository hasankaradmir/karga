// Package cli wires karga's cobra commands. It holds no business logic.
package cli

import (
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/hasankaradmir/karga/internal/infrastructure/config"
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
	home      string
	debug     bool
	logFormat string

	cfg      config.Config
	log      *slog.Logger
	redactor *logger.Redactor
}

// NewRootCmd builds the karga command tree.
func NewRootCmd(stdout, stderr io.Writer) *cobra.Command {
	a := &app{stdout: stdout, stderr: stderr, redactor: logger.NewRedactor()}

	root := &cobra.Command{
		Use:           "karga",
		Short:         "Helm-like package manager for single-node container workloads",
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

	root.AddCommand(newVersionCmd(a), newTemplateCmd(a), newLintCmd(a), newShowCmd(a))
	return root
}
