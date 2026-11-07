package gateway

import (
	"context"
	"errors"

	"github.com/marvell/sprut/internal/config"
	"github.com/marvell/sprut/internal/credentials"
)

// Health is how an Upstream fared when started as serve starts it.
type Health struct {
	State HealthState
	// Tools is how many tools a HealthWorks Upstream listed.
	Tools int
	// Err is why the Upstream failed to start, nil if it works.
	Err error
	// Remedy is what the user runs to make a HealthNeedsLogin Upstream work.
	Remedy string
}

// HealthState is how an Upstream fared.
type HealthState int

const (
	HealthWorks HealthState = iota
	// HealthNeedsLogin is an OAuth Upstream without Credentials it can use:
	// it has none, they are dead, or the Upstream rejected them with a 401.
	HealthNeedsLogin
	HealthFailed
)

// Check starts u as Start does, with its Credentials from auths, lists its
// tools and stops it again, and returns how it fared.
func Check(ctx context.Context, u config.Upstream, auths *Auths, o *StartOptions) Health {
	return check(ctx, []config.Upstream{u}, auths, o)[0]
}

// check starts every Upstream in configured concurrently, as Start does,
// with its Credentials from auths, lists its tools and stops it again, and
// returns how each fared, in Config order.
func check(ctx context.Context, configured []config.Upstream, auths *Auths, o *StartOptions) []Health {
	s := newUpstreams(auths, o)
	defer s.close()
	health := make([]Health, len(configured))
	for i, res := range s.start(ctx, configured) {
		switch {
		case res.err == nil:
			health[i] = Health{State: HealthWorks, Tools: len(res.r.tools)}
		case errors.As(res.err, new(*credentials.LoginNeededError)):
			health[i] = Health{State: HealthNeedsLogin, Err: res.err, Remedy: hint(res.err)}
		default:
			health[i] = Health{State: HealthFailed, Err: res.err}
		}
	}
	return health
}
