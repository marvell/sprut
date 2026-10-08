package gateway

import (
	"time"

	"github.com/marvell/sprut/internal/config"
	"github.com/marvell/sprut/internal/credentials"
)

// Status describes the Credentials of the OAuth Upstream u, as store holds
// them: whether it has any, when their access token expires, and whether
// they can be renewed. It contacts no server, so Credentials that the
// Authorization Server would reject read as good.
func Status(store *credentials.Store, u config.Upstream) (string, error) {
	c, err := store.Load(u)
	switch {
	case err != nil:
		return "", err
	case c == nil:
		// No hint: an Upstream that never asks for authorization has none
		// either, and needs no Login.
		return "no credentials", nil
	case c.Dead():
		return expired + "; " + loginHint(u.Name), nil
	}
	s := "logged in, access token "
	switch {
	case c.Expiry.IsZero():
		s += "does not expire"
	case time.Now().After(c.Expiry):
		s += "expired " + c.Expiry.Local().Format(time.RFC3339)
	default:
		s += "expires " + c.Expiry.Local().Format(time.RFC3339)
	}
	if c.RefreshToken == "" {
		return s + ", not renewable", nil
	}
	return s + ", renewable", nil
}
