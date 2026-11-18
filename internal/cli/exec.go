package cli

import (
	"errors"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/hasankaradmir/karga-sdk/api"
)

// ExitCodeError makes karga exit with Code without printing anything (e.g. a command's own code).
type ExitCodeError struct{ Code int }

func (e *ExitCodeError) Error() string { return "exit status " + strconv.Itoa(e.Code) }

func newExecCmd(a *app) *cobra.Command {
	var interactive, tty bool
	cmd := &cobra.Command{
		Use:   "exec <release> <workload> [-- command...]",
		Short: "Run a command in a workload's container (default: an interactive sh)",
		Example: "  karga exec shop web -- ls /data\n" +
			"  karga exec shop db -it -- psql -U app\n" +
			"  karga exec shop web            # interactive sh",
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			command := args[2:]
			if len(command) == 0 {
				command, interactive, tty = []string{"sh"}, true, true
			}
			opts := api.ExecOptions{Cmd: command, TTY: tty, Stdout: a.stdout, Stderr: a.stderr}
			if interactive {
				opts.Stdin = cmd.InOrStdin()
			}
			if tty {
				// The terminal is read from stdin, with or without -i (its keys are the input only with -i).
				in, ok := cmd.InOrStdin().(*os.File)
				if !ok || !term.IsTerminal(int(in.Fd())) {
					return usageError{errors.New("-t needs a terminal on stdin; drop -t (and -i) for non-interactive use")}
				}
				state, err := term.MakeRaw(int(in.Fd()))
				if err != nil {
					return err
				}
				defer func() { _ = term.Restore(int(in.Fd()), state) }()
				size := func() [2]uint {
					w, h, err := term.GetSize(int(in.Fd()))
					if err != nil {
						return [2]uint{80, 24}
					}
					return [2]uint{uint(w), uint(h)}
				}
				sz := size()
				opts.Width, opts.Height = sz[0], sz[1]
				resize := make(chan [2]uint, 1)
				winch := make(chan os.Signal, 1)
				signal.Notify(winch, syscall.SIGWINCH)
				defer signal.Stop(winch)
				go func() {
					for range winch {
						select {
						case resize <- size():
						default:
						}
					}
				}()
				opts.Resize = resize
			}
			s := a.backend()
			code, err := s.Exec(cmd.Context(), args[0], args[1], opts)
			if err != nil {
				return err
			}
			if code != 0 {
				return &ExitCodeError{Code: code}
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&interactive, "stdin", "i", false, "pass stdin to the command")
	cmd.Flags().BoolVarP(&tty, "tty", "t", false, "allocate a terminal (needs a terminal on stdin)")
	return cmd
}
