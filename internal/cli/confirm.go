package cli

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// errAborted is returned when the user does not confirm.
var errAborted = errors.New("aborted")

// confirm asks before a command that loses data or exposes services: on a terminal it asks the user,
// otherwise (a script) it needs --yes.
func (a *app) confirm(cmd *cobra.Command, what string) error {
	if a.yes {
		return nil
	}
	f, ok := cmd.InOrStdin().(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return usageError{fmt.Errorf("%s; pass --yes to confirm (stdin is not a terminal)", what)}
	}
	if _, err := fmt.Fprintf(a.stderr, "%s. Continue? [y/N] ", what); err != nil {
		return err
	}
	answer, _ := bufio.NewReader(f).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return nil
	}
	return errAborted
}

// exposesHost reports whether addrs include an address other machines can reach.
func exposesHost(addrs []string) bool {
	for _, a := range addrs {
		if ip := net.ParseIP(strings.TrimSpace(a)); ip == nil || !ip.IsLoopback() {
			return true
		}
	}
	return false
}
