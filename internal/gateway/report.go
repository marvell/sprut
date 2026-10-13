package gateway

import (
	"context"
	"time"

	"github.com/marvell/sprut/internal/config"
)

// Report is how one entry of the Config is doing: the state of its
// Credentials and, if checked, how it fared.
type Report struct {
	Entry config.Entry
	Auth  AuthStatus
	// Health is how the entry fared when started, nil if it wasn't: no
	// check was asked for, or the Gateway doesn't start it.
	Health *Health
}

// Reports returns a Report of each of entries, in their order, with their
// Credentials from auths. Unless checkOpts is nil, it also starts every
// entry the Gateway starts, concurrently, to say how it fares.
func Reports(ctx context.Context, entries []config.Entry, auths *Auths, checkOpts *CheckOptions) []Report {
	reports := make([]Report, len(entries))
	var started []config.Upstream
	var at []int // the index in reports of each of started
	for i, e := range entries {
		reports[i] = Report{Entry: e, Auth: statusOf(auths, e)}
		if e.Starts() {
			started = append(started, *e.Upstream)
			at = append(at, i)
		}
	}
	if checkOpts != nil && len(started) > 0 {
		for i, h := range check(ctx, started, auths, checkOpts) {
			reports[at[i]].Health = &h
		}
	}
	return reports
}

// AllWork reports whether every checked entry of reports works.
func AllWork(reports []Report) bool {
	for _, r := range reports {
		if r.Health != nil && r.Health.State != HealthWorks {
			return false
		}
	}
	return true
}

// AuthStatus is the state of an entry's Credentials, as the Store holds
// them. It is read without contacting any server, so Credentials that the
// Authorization Server would reject read as good.
type AuthStatus struct {
	State AuthState
	// Expiry is when the access token of AuthLoggedIn expires, zero if it
	// doesn't.
	Expiry time.Time
	// Expired reports whether the access token of AuthLoggedIn has expired,
	// which leaves the Credentials usable only if Renewable.
	Expired bool
	// Renewable reports whether AuthLoggedIn Credentials have a refresh
	// token.
	Renewable bool
	// Err is why the Credentials of AuthUnreadable can't be read.
	Err error
	// Reason is why AuthDead Credentials can't be used, and Remedy what the
	// user runs to make them good again.
	Reason, Remedy string
}

// AuthState is what an entry's Credentials are.
type AuthState int

const (
	// AuthNotOAuth is an entry that is not an OAuth Upstream, or can't be
	// told to be one: a skipped entry has no URL to bind Credentials to. A
	// disabled one is judged as if enabled.
	AuthNotOAuth AuthState = iota
	AuthNoCredentials
	AuthLoggedIn
	// AuthDead is Credentials that can't be used any more without a Login
	// (see credentials.Credentials.Dead).
	AuthDead
	AuthUnreadable
)

// statusOf is the AuthStatus of e: that of its Auth, unless it is skipped.
func statusOf(auths *Auths, e config.Entry) AuthStatus {
	if e.Upstream == nil {
		return AuthStatus{State: AuthNotOAuth}
	}
	return auths.For(*e.Upstream).Status()
}
