package store

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
)

// Opener builds a backend from a URL. A backend registers one per scheme it
// answers for.
type Opener func(ctx context.Context, url string) (Store, error)

var (
	openersMu sync.RWMutex
	openers   = map[string]Opener{}
)

// Register makes a backend reachable through Open.
//
// The indirection exists because a backend package has to import this one for
// Doc, Cursor and the rest, so this package cannot import a backend back
// without a cycle. It is the database/sql arrangement and it has the same
// consequence: the process must link the backend it intends to use, which
// main.go does with a blank import.
//
// Registering a scheme twice panics, at init time, where a duplicate is a
// programming error rather than a runtime condition.
func Register(scheme string, open Opener) {
	openersMu.Lock()
	defer openersMu.Unlock()

	scheme = strings.ToLower(scheme)
	if _, dup := openers[scheme]; dup {
		panic("store: backend already registered for scheme " + scheme)
	}
	openers[scheme] = open
}

// Open returns the store described by a database URL.
//
// An empty URL is the documented default deployment — a single-user local MCP
// server that should not have to stand up a database — and yields Nop, which
// misses everything. Any other URL must name a registered backend; an
// unrecognised one is an error and never a silent downgrade to Nop, because an
// operator who configured a database has told us they expect caching to work.
func Open(ctx context.Context, rawURL string) (Store, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return Nop{}, nil
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("store: database url is not a url: %w", err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme == "" {
		return nil, fmt.Errorf("store: database url %q has no scheme; expected something like postgres://user:pass@host/db", redact(parsed))
	}

	openersMu.RLock()
	open, ok := openers[scheme]
	known := registeredSchemes()
	openersMu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("store: no backend for scheme %q (linked backends: %s)", scheme, strings.Join(known, ", "))
	}
	return open(ctx, rawURL)
}

// registeredSchemes is called with openersMu held.
func registeredSchemes() []string {
	out := make([]string, 0, len(openers))
	for scheme := range openers {
		out = append(out, scheme)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return []string{"none"}
	}
	return out
}

// redact strips the password before a URL reaches a log line or an error a
// user will paste into an issue.
func redact(u *url.URL) string {
	if u.User != nil {
		if _, hasPassword := u.User.Password(); hasPassword {
			u.User = url.UserPassword(u.User.Username(), "xxxxx")
		}
	}
	return u.String()
}

// Redact returns a database URL safe to log: everything but the password.
func Redact(rawURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "<unparseable database url>"
	}
	return redact(parsed)
}
