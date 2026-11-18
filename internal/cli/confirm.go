package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// errAborted is returned when the user does not confirm.
var errAborted = errors.New("aborted")

// confirm asks the user to agree to risk, a request's Risk (what it changes that cannot be changed
// back; empty: nothing to ask): on a terminal it asks, otherwise (a script) it needs --yes. The
// caller then sets the request's Confirmed, which karga-server requires.
func (a *app) confirm(cmd *cobra.Command, risk string) error {
	if a.yes || risk == "" {
		return nil
	}
	what := strings.ToUpper(risk[:1]) + risk[1:]
	f, ok := cmd.InOrStdin().(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return usageError{fmt.Errorf("%s; pass --yes to confirm (stdin is not a terminal)", what)}
	}
	if _, err := fmt.Fprintf(a.stderr, "%s. Continue? [y/N] ", what); err != nil {
		return err
	}
	answer, err := bufio.NewReader(f).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) { // EOF, no answer: not agreed
		return err
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return nil
	}
	return errAborted
}
