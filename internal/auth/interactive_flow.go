package auth

import (
	"context"
	"sync"
)

// Authorization holds no refresh/store transaction lock while waiting for the
// browser. The flight is shared by all live callers of the same identity and
// cancelled only when every waiter has left. Completed flights are removed.
type interactiveFlight struct {
	done      chan struct{}
	cancel    context.CancelFunc
	waiters   int
	abandoned bool
	completed bool
	consented bool
	err       error
}

type interactiveFlightGroup struct {
	mu     sync.Mutex
	active map[string]*interactiveFlight
}

var interactiveLogins interactiveFlightGroup

func (g *interactiveFlightGroup) run(ctx context.Context, key string, authorize func(context.Context) (bool, error)) (bool, bool, error) {
	for {
		if err := ctx.Err(); err != nil {
			return false, false, err
		}
		g.mu.Lock()
		flight := g.active[key]
		if flight != nil && flight.abandoned {
			// The old callback must close before a new attempt can bind the same
			// stable redirect. Do not join a flow already cancelled by all users.
			g.mu.Unlock()
			select {
			case <-flight.done:
				continue
			case <-ctx.Done():
				return false, false, ctx.Err()
			}
		}
		owned := flight == nil
		var flowContext context.Context
		if owned {
			var cancel context.CancelFunc
			flowContext, cancel = context.WithCancel(context.WithoutCancel(ctx))
			flight = &interactiveFlight{done: make(chan struct{}), cancel: cancel}
			if g.active == nil {
				g.active = make(map[string]*interactiveFlight)
			}
			g.active[key] = flight
		}
		flight.waiters++
		g.mu.Unlock()
		if owned {
			go func() {
				consented, err := authorize(flowContext)
				g.mu.Lock()
				flight.err, flight.consented, flight.completed = err, consented, true
				delete(g.active, key)
				close(flight.done)
				g.mu.Unlock()
				flight.cancel()
			}()
		}
		select {
		case <-flight.done:
		case <-ctx.Done():
		}
		g.mu.Lock()
		flight.waiters--
		if flight.waiters == 0 && !flight.completed {
			flight.abandoned = true
			flight.cancel()
		}
		err := flight.err
		consented := flight.consented
		g.mu.Unlock()
		if ctx.Err() != nil {
			return owned, false, ctx.Err()
		}
		return owned, consented, err
	}
}
