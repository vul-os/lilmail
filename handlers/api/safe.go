package api

import (
	"regexp"
	"strings"
)

// reUnsafe matches any character outside the safe set [A-Za-z0-9._@+-].
var reUnsafe = regexp.MustCompile(`[^A-Za-z0-9._@+\-]+`)

// SanitizeUsername converts a username into a safe single filesystem path
// segment. It replaces any character outside [A-Za-z0-9._@+-] with an
// underscore, collapses runs of underscores, replaces ".." sequences, and
// returns a non-empty fallback ("_user") when the result would otherwise be
// empty.
//
// The raw LOGIN username is never changed — only the on-disk cache folder name
// should be derived via this function.
func SanitizeUsername(username string) string {
	if username == "" {
		return "_user"
	}

	// Replace runs of unsafe characters with a single underscore.
	s := reUnsafe.ReplaceAllString(username, "_")

	// Strip leading/trailing underscores produced by the replacement.
	s = strings.Trim(s, "_")

	// Replace any ".." segment (now surrounded by underscores or at the
	// boundary) with a single underscore, then re-trim.
	for strings.Contains(s, "..") {
		s = strings.ReplaceAll(s, "..", "_")
		s = strings.Trim(s, "_")
		// Collapse consecutive underscores that may have been introduced.
		s = regexp.MustCompile(`_+`).ReplaceAllString(s, "_")
	}

	if s == "" || s == "." {
		return "_user"
	}
	return s
}
