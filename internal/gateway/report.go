package gateway

import (
	"context"
	"time"

	"github.com/marvell/sprut/internal/config"
	"github.com/marvell/sprut/internal/credentials"
)

// Report is how one entry of the Config is doing: whether the Gateway
// starts it, the state of its Credentials and, if checked, how it fared.
type Report struct {
	Entry config.Entry
	Start StartState
	Auth  Auth
	// Health is how the entry fared when started, nil if it wasn't: no
	// check was asked for, or the Gateway doesn't start it.
	Health *Health
}

// StartState is whether the Gateway starts an entry.
type StartState int

const (
	StartEnabled StartState = iota
	StartDisabled
	// StartSkipped is an enabled entry that the Gateway skips, for the
	// reason in its Skipped.
	StartSkipped
)

// Reports returns a Report of each of entries, in their order, with the
// Credentials as store holds them; a nil store is one that can't be located
// (credentials.ErrNoHome). Unless checkOpts is nil, it also starts every
// entry the Gateway starts, concurrently, to say how it fares.
func Reports(ctx context.Context, entries []config.Entry, store *credentials.Store, checkOpts *CheckOptions) []Report {
	reports := make([]Report, len(entries))
	var started []config.Upstream
	var at []int // the index in reports of each of started
	for i, e := range entries {
		reports[i] = Report{Entry: e, Start: startState(e), Auth: authOf(store, e)}
		if e.Starts() {
			started = append(started, *e.Upstream)
			at = append(at, i)
		}
	}
	if checkOpts != nil && len(started) > 0 {
		for i, h := range check(ctx, started, checkOpts) {
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

func startState(e config.Entry) StartState {
	switch {
	case e.Disabled:
		return StartDisabled
	case e.Skipped != "":
		return StartSkipped
	}
	return StartEnabled
}

// Auth is the state of an entry's Credentials, as the store holds them. It
// is read without contacting any server, so Credentials that the
// Authorization Server would reject read as good.
type Auth struct {
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
	// AuthNotOAuth is an entry that is not an OAuth Upstream (see
	// CheckOAuthUpstream), or can't be told to be one: a skipped entry has
	// no URL to bind Credentials to. A disabled one is judged as if enabled.
	AuthNotOAuth AuthState = iota
	AuthNoCredentials
	AuthLoggedIn
	// AuthDead is Credentials that can't be used any more without a Login
	// (see credentials.Credentials.Dead).
	AuthDead
	AuthUnreadable
)

// authOf is the Auth of e in store, as for Reports.
func authOf(store *credentials.Store, e config.Entry) Auth {
	if e.Upstream == nil || CheckOAuthUpstream(*e.Upstream) != nil {
		return Auth{State: AuthNotOAuth}
	}
	if store == nil {
		return Auth{State: AuthUnreadable, Err: credentials.ErrNoHome}
	}
	c, err := store.Load(*e.Upstream)
	switch {
	case err != nil:
		return Auth{State: AuthUnreadable, Err: err}
	case c == nil:
		// No remedy: an Upstream that never asks for authorization has
		// none either, and needs no Login.
		return Auth{State: AuthNoCredentials}
	case c.Dead():
		return Auth{State: AuthDead, Reason: credentials.ReasonExpired, Remedy: loginHint(*e.Upstream)}
	}
	return Auth{
		State:     AuthLoggedIn,
		Expiry:    c.Expiry,
		Expired:   !c.Expiry.IsZero() && time.Now().After(c.Expiry),
		Renewable: c.RefreshToken != "",
	}
}
