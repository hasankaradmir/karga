// Package cli wires karga's cobra commands. It holds no business logic.
package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

const (
	outputTable = "table"
	outputJSON  = "json"
)

// app is the per-invocation state shared by all subcommands.
type app struct {
	stdout io.Writer
	stderr io.Writer
	output string
}

// NewRootCmd builds the karga command tree.
func NewRootCmd(stdout, stderr io.Writer) *cobra.Command {
	a := &app{stdout: stdout, stderr: stderr}

	root := &cobra.Command{
		Use:           "karga",
		Short:         "Helm-like package manager for single-node container workloads",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(*cobra.Command, []string) error {
			if a.output != outputTable && a.output != outputJSON {
				return fmt.Errorf("invalid --output %q: must be %q or %q", a.output, outputTable, outputJSON)
			}
			return nil
		},
	}
	root.SetOut(stdout)
	root.SetErr(stderr)

	root.PersistentFlags().StringVarP(&a.output, "output", "o", outputTable, "output format: table|json")

	root.AddCommand(newVersionCmd(a))
	return root
}
