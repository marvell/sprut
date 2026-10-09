package gateway

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/marvell/sprut/internal/config"
	"github.com/marvell/sprut/internal/credentials"
)

// Health is how an Upstream fared when started as serve starts it.
type Health struct {
	// Tools is how many tools it listed, if it started.
	Tools int
	// Err is why it failed to start, nil if it started.
	Err error
}

// NeedsLogin reports whether the Upstream failed because it is an OAuth
// Upstream without Credentials it can use: it has none, they are dead, or
// the Upstream rejected them with a 401.
func (h Health) NeedsLogin() bool {
	return errors.As(h.Err, new(*credentials.LoginNeededError))
}

// Check starts every Upstream in configured concurrently, as Start does,
// lists its tools and stops it again, and returns how each fared, in
// Config order. env and startupTimeout are as for Start; failures are
// logged to log.
func Check(ctx context.Context, configured []config.Upstream, env []string, startupTimeout time.Duration, version string, log *slog.Logger) []Health {
	s := newUpstreams(env, startupTimeout, version, log)
	defer s.close()
	health := make([]Health, len(configured))
	for i, res := range s.start(ctx, configured) {
		if res.err != nil {
			health[i].Err = res.err
			continue
		}
		health[i].Tools = len(res.r.tools)
	}
	return health
}

// Hint is the remedy of the Upstream's failure, such as the command that
// logs in to it, or "" if there is none.
func (h Health) Hint() string {
	return hint(h.Err)
}
