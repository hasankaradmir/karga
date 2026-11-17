package deployment

import (
	"context"
	"fmt"
	"time"
)

// Event reports what Execute is doing, for a live progress display.
type Event struct {
	Kind, Name string
	// Phase is one of pull, create, start, wait, switch, drain, stop, remove, done, failed.
	Phase  string
	Detail string
}

// Progress phases.
const (
	PhasePull   = "pull"
	PhaseCreate = "create"
	PhaseStart  = "start"
	PhaseWait   = "wait"
	PhaseSwitch = "switch"
	PhaseDrain  = "drain"
	PhaseStop   = "stop"
	PhaseRemove = "remove"
	PhaseDone   = "done"
	PhaseFailed = "failed"
)

type progressKey struct{}

func withProgress(ctx context.Context, fn func(Event)) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, progressKey{}, fn)
}

// progress reports an event to the callback carried by ctx, if any.
func progress(ctx context.Context, kind, name, phase, format string, args ...any) {
	if fn, ok := ctx.Value(progressKey{}).(func(Event)); ok {
		fn(Event{Kind: kind, Name: name, Phase: phase, Detail: fmt.Sprintf(format, args...)})
	}
}

// waitEvery is how often a long wait reports that it is still waiting.
var waitEvery = 2 * time.Second
