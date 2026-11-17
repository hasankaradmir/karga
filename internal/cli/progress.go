package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/hasankaradmir/karga-sdk/plan"
)

// isTerminal reports whether w is an interactive terminal.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// progressPrinter shows deployment events on stderr: one line each, with the time since start. On
// a terminal the repeated "still waiting" line is rewritten in place.
type progressPrinter struct {
	mu      sync.Mutex
	w       io.Writer
	tty     bool
	start   time.Time
	pending bool // a rewritable line is on screen
}

func newProgressPrinter(w io.Writer) *progressPrinter {
	return &progressPrinter{w: w, tty: isTerminal(w), start: time.Now()}
}

func (p *progressPrinter) event(ev plan.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	line := fmt.Sprintf("[%4s] %s %s: %s", time.Since(p.start).Round(time.Second), strings.ToLower(ev.Kind), ev.Name, ev.Detail)
	if ev.Phase == plan.PhaseDone {
		line = fmt.Sprintf("[%4s] %s %s: %s done", time.Since(p.start).Round(time.Second), strings.ToLower(ev.Kind), ev.Name, ev.Detail)
	}
	if !p.tty {
		_, _ = fmt.Fprintln(p.w, line)
		return
	}
	if p.pending {
		_, _ = io.WriteString(p.w, "\r\033[K")
	}
	if ev.Phase == plan.PhaseWait {
		_, _ = io.WriteString(p.w, line)
		p.pending = true
		return
	}
	_, _ = fmt.Fprintln(p.w, line)
	p.pending = false
}

// finish ends a rewritable line so the result starts on its own line.
func (p *progressPrinter) finish() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pending {
		_, _ = io.WriteString(p.w, "\n")
		p.pending = false
	}
}
