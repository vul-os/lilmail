package api

import "testing"

func TestSanitizeUsername(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// Normal usernames pass through unchanged.
		{"alice", "alice"},
		{"alice@example.com", "alice@example.com"},
		{"user.name+tag-123", "user.name+tag-123"},

		// Path traversal characters must be neutralised.
		{"../../etc/passwd", "etc_passwd"},
		{"/root", "root"},
		{"foo/bar", "foo_bar"},
		{"foo\\bar", "foo_bar"},

		// Control characters become underscores.
		{"user\x00name", "user_name"},
		{"user\nname", "user_name"},
		{"user\tname", "user_name"},

		// Multiple consecutive unsafe chars collapse to one underscore.
		{"a//b", "a_b"},
		{"a///b", "a_b"},

		// Empty input returns the safe fallback.
		{"", "_user"},

		// All-unsafe input returns the safe fallback.
		{"///", "_user"},
		{"\x00\x01\x02", "_user"},

		// Dots and underscores in the middle are fine.
		{"my_user.name", "my_user.name"},

		// Leading/trailing unsafe chars are stripped.
		{"/alice/", "alice"},

		// ".." sequences are replaced (path traversal via dotdot).
		{"..alice..", "alice"},
		{"../../etc/passwd", "etc_passwd"},
	}

	for _, tc := range cases {
		got := SanitizeUsername(tc.in)
		if got != tc.want {
			t.Errorf("SanitizeUsername(%q) = %q, want %q", tc.in, got, tc.want)
		}
		// Result must never be empty.
		if got == "" {
			t.Errorf("SanitizeUsername(%q) returned empty string", tc.in)
		}
		// Result must not contain path separators.
		for _, ch := range got {
			if ch == '/' || ch == '\\' {
				t.Errorf("SanitizeUsername(%q) = %q contains path separator %q", tc.in, got, string(ch))
			}
		}
		// Result must not be "." or ".."
		if got == "." || got == ".." {
			t.Errorf("SanitizeUsername(%q) = %q is a reserved path segment", tc.in, got)
		}
	}
}
