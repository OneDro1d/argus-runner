package runner

import (
	"context"

	"github.com/OneDro1d/argus-runner/internal/argus"
)

// tallyKey is the context key the live run Tally travels under, from the code that starts the heartbeat to
// NewRunFunc, which hands it to the run loop (toolcore.Env.Tally). It rides the context, not the Executor
// and not a package variable, because the RunFunc signature is fixed (ctx, assignment) and an Executor is
// shared: a context belongs to exactly one run, so two runs can never read each other's counts.
type tallyKey struct{}

// withTally returns ctx carrying t for the run that is about to start.
func withTally(ctx context.Context, t *argus.Tally) context.Context {
	return context.WithValue(ctx, tallyKey{}, t)
}

// tallyFrom is the run's Tally, or nil when the caller set none (a RunFunc called directly, every test).
// Nil is valid everywhere downstream and means today's behaviour exactly.
func tallyFrom(ctx context.Context) *argus.Tally {
	t, _ := ctx.Value(tallyKey{}).(*argus.Tally)
	return t
}
