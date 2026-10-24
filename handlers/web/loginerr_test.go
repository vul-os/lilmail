package web

import (
	"errors"
	"fmt"
	"lilmail/handlers/api"
	"testing"
)

func TestLoginFailure(t *testing.T) {
	cases := []struct {
		err    error
		status int
		msg    string
	}{
		{fmt.Errorf("%w: invalid credentials", api.ErrLogin), 401, "Invalid email or password"},
		{fmt.Errorf("oauth2 %w: bad token", api.ErrLogin), 401, "Invalid email or password"},
		{fmt.Errorf("%w: i/o timeout", api.ErrConnect), 503, "Could not reach the mail server. Check that it is running and try again."},
		{errors.New("something else"), 502, "Login failed because of a mail server error. Try again later."},
	}
	for _, c := range cases {
		status, msg := loginFailure(c.err)
		if status != c.status || msg != c.msg {
			t.Errorf("loginFailure(%v) = %d %q; want %d %q", c.err, status, msg, c.status, c.msg)
		}
	}
}
