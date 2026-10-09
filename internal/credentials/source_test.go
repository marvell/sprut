package credentials_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/marvell/sprut/internal/credentials"
)

// wantToken checks that Token returns want.
func wantToken(t *testing.T, src *credentials.Source, want string) {
	t.Helper()
	got, err := src.Token(t.Context())
	if err != nil || got != want {
		t.Errorf("Token = %q, %v, want %q, nil", got, err, want)
	}
}

// wantLoginNeeded checks that err is a *LoginNeededError with reason, ""
// for none.
func wantLoginNeeded(t *testing.T, err error, reason string) {
	t.Helper()
	var e *credentials.LoginNeededError
	if !errors.As(err, &e) {
		t.Fatalf("err = %v, want a *LoginNeededError", err)
	}
	if e.Reason != reason {
		t.Errorf("reason = %q, want %q", e.Reason, reason)
	}
}

func TestSourceWithoutCredentialsSendsNoToken(t *testing.T) {
	t.Parallel()
	as := &authServer{}
	src := source(t, openStore(t, stateHome(t)), linear, as)

	wantToken(t, src, "")
	wantLoginNeeded(t, src.Unauthorized(), "")
	if as.Calls() != 0 {
		t.Errorf("refreshes = %d, want 0", as.Calls())
	}

	var none *credentials.Source
	if got, err := none.Token(t.Context()); got != "" || err != nil {
		t.Errorf("Token on the nil Source = %q, %v, want \"\", nil", got, err)
	}
}

func TestSourceSendsAFreshTokenWithoutRenewingIt(t *testing.T) {
	t.Parallel()
	s := openStore(t, stateHome(t))
	save(t, s, linear, loggedIn(linear, time.Hour))
	as := &authServer{}

	wantToken(t, source(t, s, linear, as), "access-1")
	if as.Calls() != 0 {
		t.Errorf("refreshes = %d, want 0", as.Calls())
	}
}

func TestSourceRenewsATokenThatExpiresWithinFiveMinutesAndSavesIt(t *testing.T) {
	t.Parallel()
	s := openStore(t, stateHome(t))
	save(t, s, linear, loggedIn(linear, 4*time.Minute))
	as := &authServer{}
	src := source(t, s, linear, as)

	wantToken(t, src, "access-2")
	wantToken(t, src, "access-2")
	if as.Calls() != 1 {
		t.Errorf("refreshes = %d, want 1", as.Calls())
	}
	if c := load(t, s, linear); c == nil || c.RefreshToken != "refresh-2" {
		t.Errorf("saved Credentials = %+v, want the rotated refresh-2", c)
	}
}

func TestSourceRenewsOnceForConcurrentRequests(t *testing.T) {
	t.Parallel()
	s := openStore(t, stateHome(t))
	save(t, s, linear, loggedIn(linear, time.Minute))
	as, release := holding(t)
	src := source(t, s, linear, as)

	const requests = 5
	var wg sync.WaitGroup
	for range requests {
		wg.Go(func() { wantToken(t, src, "access-2") })
	}
	as.waitEntered(t)
	release()
	wg.Wait()
	// A request that arrives once the renewal is over finds access-2 and
	// renews nothing either.
	if as.Calls() != 1 {
		t.Errorf("refreshes = %d, want 1", as.Calls())
	}
}

// Another process that renewed first has saved new Credentials by the time
// this one holds the lock: they are the ones to use, without a refresh.
func TestSourceUsesCredentialsAnotherProcessRenewedWhileItWaitedForTheLock(t *testing.T) {
	t.Parallel()
	home := stateHome(t)
	one, other := openStore(t, home), openStore(t, home)
	save(t, one, linear, loggedIn(linear, time.Hour))
	as := &authServer{}
	src := source(t, one, linear, as)
	wantToken(t, src, "access-1")

	renewed := loggedIn(linear, time.Hour)
	renewed.AccessToken, renewed.RefreshToken = "access-elsewhere", "refresh-elsewhere"
	save(t, other, linear, renewed)

	got, err := src.Rejected(t.Context(), "access-1", "")
	if err != nil || got != "access-elsewhere" {
		t.Errorf("Rejected = %q, %v, want access-elsewhere, nil", got, err)
	}
	if as.Calls() != 0 {
		t.Errorf("refreshes = %d, want 0", as.Calls())
	}
}

func TestSourceRenewsOnceForEachTokenTheUpstreamRejects(t *testing.T) {
	t.Parallel()
	s := openStore(t, stateHome(t))
	save(t, s, linear, loggedIn(linear, time.Hour))
	as := &authServer{}
	src := source(t, s, linear, as)
	wantToken(t, src, "access-1") // as the request the Upstream then rejects

	got, err := src.Rejected(t.Context(), "access-1", "")
	if err != nil || got != "access-2" {
		t.Fatalf("Rejected(access-1) = %q, %v, want access-2, nil", got, err)
	}
	// A token that a renewal on a 401 got, rejected too: the Upstream
	// rejects the Credentials, not their age.
	_, err = src.Rejected(t.Context(), "access-2", "")
	wantLoginNeeded(t, err, "the upstream rejected the credentials")
	if as.Calls() != 1 {
		t.Errorf("refreshes = %d, want 1", as.Calls())
	}
}

func TestSourceNeedsALoginOnceTheAuthorizationServerRejectsTheRefreshToken(t *testing.T) {
	t.Parallel()
	s := openStore(t, stateHome(t))
	save(t, s, linear, loggedIn(linear, time.Minute))
	as := &authServer{err: credentials.ErrRefreshRejected}
	src := source(t, s, linear, as)

	for range 2 {
		_, err := src.Token(t.Context())
		wantLoginNeeded(t, err, "the authorization server rejected the credentials")
	}
	if as.Calls() != 1 {
		t.Errorf("refreshes = %d, want 1: dead Credentials are not renewed again", as.Calls())
	}
	if c := load(t, s, linear); c == nil {
		t.Error("the rejected Credentials were deleted; only a Login or a logout replaces them")
	}
}

// A process that renewed without waiting for the lock leaves this one's
// refresh token stale, so the Authorization Server rejects it: the
// Credentials that the other process saved are the ones to use.
func TestSourceKeepsCredentialsAnotherProcessRotatedOnARejectedRefresh(t *testing.T) {
	t.Parallel()
	home := stateHome(t)
	one := openStore(t, home)
	save(t, one, linear, loggedIn(linear, time.Minute))
	as, release := holding(t)
	as.err = credentials.ErrRefreshRejected
	src := source(t, one, linear, as)

	renewing := make(chan error, 1)
	go func() {
		got, err := src.Token(t.Context())
		if err == nil && got != "access-elsewhere" {
			err = errors.New("token " + got + ", want access-elsewhere")
		}
		renewing <- err
	}()
	as.waitEntered(t)
	rotated := loggedIn(linear, time.Hour)
	rotated.AccessToken, rotated.RefreshToken = "access-elsewhere", "refresh-elsewhere"
	writeUnlocked(t, home, linear, rotated)
	release()

	if err := <-renewing; err != nil {
		t.Errorf("Token: %v", err)
	}
}

func TestSourceWithoutARefreshTokenSendsTheTokenUntilItExpires(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		expiresIn time.Duration
		wantToken string
		reason    string // of the *LoginNeededError, if one is wanted
	}{
		{name: "expiring soon", expiresIn: time.Minute, wantToken: "access-1"},
		{name: "expired", expiresIn: -time.Minute, reason: credentials.ReasonExpired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := openStore(t, stateHome(t))
			c := loggedIn(linear, tt.expiresIn)
			c.RefreshToken = ""
			save(t, s, linear, c)
			as := &authServer{}
			src := source(t, s, linear, as)

			got, err := src.Token(t.Context())
			if tt.reason != "" {
				wantLoginNeeded(t, err, tt.reason)
			} else if err != nil || got != tt.wantToken {
				t.Errorf("Token = %q, %v, want %q, nil", got, err, tt.wantToken)
			}
			if as.Calls() != 0 {
				t.Errorf("refreshes = %d, want 0", as.Calls())
			}
		})
	}
}

func TestSourcePicksUpALoginAndALogoutWithoutARestart(t *testing.T) {
	t.Parallel()
	home := stateHome(t)
	serve, login := openStore(t, home), openStore(t, home)
	src := source(t, serve, linear, &authServer{})
	wantToken(t, src, "")

	save(t, login, linear, loggedIn(linear, time.Hour))
	wantToken(t, src, "access-1")

	if _, err := login.Delete(t.Context(), linear.Name); err != nil {
		t.Fatal(err)
	}
	wantToken(t, src, "")
	wantLoginNeeded(t, src.Unauthorized(), "")
}

// The Authorization Server may already have rotated the refresh token when
// the request that started a renewal gives up, so the renewal goes on, and
// Wait lets the process save it before it exits.
func TestSourceFinishesARenewalTheRequestGaveUpOnAndWaitSavesIt(t *testing.T) {
	t.Parallel()
	s := openStore(t, stateHome(t))
	save(t, s, linear, loggedIn(linear, time.Minute))
	as, release := holding(t)
	src := source(t, s, linear, as)

	ctx, cancel := context.WithCancel(t.Context())
	gaveUp := make(chan error, 1)
	go func() {
		_, err := src.Token(ctx)
		gaveUp <- err
	}()
	as.waitEntered(t)
	cancel()
	if err := <-gaveUp; !errors.Is(err, context.Canceled) {
		t.Errorf("Token after its request gave up: err = %v, want context.Canceled", err)
	}

	waited := make(chan struct{})
	go func() {
		s.Wait()
		close(waited)
	}()
	release()
	select {
	case <-waited:
	case <-time.After(10 * time.Second):
		t.Fatal("Wait did not return once the renewal ended")
	}
	if c := load(t, s, linear); c == nil || c.RefreshToken != "refresh-2" {
		t.Errorf("saved Credentials = %+v, want the rotated refresh-2", c)
	}
}
