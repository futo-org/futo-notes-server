package config

import "strings"

// NormalizeDatabaseURL keeps Postgres URLs meaning what they meant before pgx
// 5.11, whose libpq-exact parser ends the credentials at the first '@' where
// pgx 5.10 (net/url) and the TypeScript server ended them at the last '@' of
// the authority. Compose files splice the raw POSTGRES_PASSWORD into
// DATABASE_URL, so a password containing '@' must keep its host.
//
// The authority ends at the first '/', '?' or '#', as net/url read it. Every
// '@' in it but the last is percent-encoded, and a path-less authority gets
// the empty path it implied, so an '@' in the query is never read as one either.
func NormalizeDatabaseURL(raw string) string {
	for _, scheme := range []string{"postgres://", "postgresql://"} {
		if !strings.HasPrefix(raw, scheme) {
			continue
		}
		rest := raw[len(scheme):]
		end := strings.IndexAny(rest, "/?#")
		if end < 0 {
			end = len(rest)
		}
		authority, tail := rest[:end], rest[end:]
		if last := strings.LastIndexByte(authority, '@'); last > 0 {
			authority = strings.ReplaceAll(authority[:last], "@", "%40") + authority[last:]
		}
		if tail != "" && tail[0] != '/' {
			tail = "/" + tail
		}
		return scheme + authority + tail
	}
	return raw
}
