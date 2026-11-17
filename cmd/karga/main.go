package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/hasankaradmir/karga/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := cli.NewRootCmd(os.Stdout, os.Stderr).ExecuteContext(ctx)
	stop()
	var exit *cli.ExitCodeError
	switch {
	case errors.As(err, &exit):
		os.Exit(exit.Code) // the command's own exit code; it printed what it had to say
	case err != nil:
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
