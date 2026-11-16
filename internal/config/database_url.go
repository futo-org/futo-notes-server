package config

import "strings"

// normalizeDatabaseURL keeps Postgres URLs whose password contains a raw '@'
// meaning what they meant before pgx 5.11: the credentials end at the last '@'
// before the path. Every earlier '@' is percent-encoded so pgx's libpq-exact
// parser (credentials end at the first '@') reads the same user and host.
func normalizeDatabaseURL(raw string) string {
	for _, scheme := range []string{"postgres://", "postgresql://"} {
		if !strings.HasPrefix(raw, scheme) {
			continue
		}
		rest := raw[len(scheme):]
		authorityEnd := strings.IndexByte(rest, '/')
		if authorityEnd < 0 {
			authorityEnd = len(rest)
		}
		authority := rest[:authorityEnd]
		last := strings.LastIndexByte(authority, '@')
		if last <= 0 || strings.IndexByte(authority, '@') == last {
			return raw
		}
		userinfo := strings.ReplaceAll(authority[:last], "@", "%40")
		return scheme + userinfo + authority[last:] + rest[authorityEnd:]
	}
	return raw
}
