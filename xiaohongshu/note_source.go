package xiaohongshu

import (
	"context"
	"sync"
	"time"
)

// xsec_source values the site itself uses when opening a note. The token is the
// same in every case; the source says which surface handed it out, and the site
// can compare that against the Referer it received.
const (
	xsecSourceFeed   = "pc_feed"   // home / explore feed
	xsecSourceSearch = "pc_search" // search results page
	xsecSourceNote   = "pc_note"   // profile pages and other note listings
)

// noteEntryPoint is where a note's xsec_token was handed to us.
type noteEntryPoint struct {
	source   string
	referrer string
	at       time.Time
}

const (
	// NoteSourceTTL bounds how long a provenance record is worth claiming.
	// Past it the search page the token came from is long gone, and a stale
	// claim is worse than the honest default. Exported because a persistent
	// implementation has to apply the same window in its own query.
	//
	// The measurement in #18 puts the token's own lifetime at hours, so this
	// window sits comfortably inside it: a record that is still valid here
	// describes a token that still works.
	NoteSourceTTL = 30 * time.Minute

	// noteSourceCapacity bounds the in-memory table. One search or profile
	// page yields a few dozen notes, so this holds roughly the last ten pages
	// an agent looked at. A store-backed implementation needs no such cap —
	// it has a retention sweep instead.
	noteSourceCapacity = 512
)

// NoteSources remembers, per note id, which page handed out that note's
// xsec_token, and hands the pair back when the note is opened.
//
// This is the "session context" the xsec_source is inferred from. It is
// deliberately not an MCP tool argument: an agent asked to supply a source
// would have to guess, and a guess that contradicts the Referer is worse than
// no claim at all. Here the two are derived from the same record, so they
// cannot disagree.
//
// The seam exists so that the record can outlive the process when a store is
// configured (issue #7, WS4). The default implementation is the in-memory
// table below, so a deployment with no database behaves exactly as it did
// before the seam existed.
//
// Neither method returns an error: provenance is advisory. A backend that is
// down means the default entry point is used, which is what happens when
// nothing was remembered anyway.
type NoteSources interface {
	Remember(ctx context.Context, feedID, source, referrer string)
	Lookup(ctx context.Context, feedID string) (source, referrer string, ok bool)
}

// noteSourceTable is the in-memory NoteSources, and the default.
type noteSourceTable struct {
	mu      sync.Mutex
	entries map[string]noteEntryPoint
	now     func() time.Time
}

func newNoteSourceTable() *noteSourceTable {
	return &noteSourceTable{
		entries: make(map[string]noteEntryPoint),
		now:     time.Now,
	}
}

// NewMemoryNoteSources returns an independent in-memory implementation. A
// store-backed implementation uses one as its fallback for the window before
// the account id is known, which is precisely the window in which the first
// listing of every restart is read.
func NewMemoryNoteSources() NoteSources { return newNoteSourceTable() }

// noteSources is process-wide because the entry point outlives any single
// action object: the search that produced a token and the comment that uses it
// are separate tool calls with separate actions.
var (
	noteSourcesMu sync.RWMutex
	noteSources   NoteSources = newNoteSourceTable()
)

// SetNoteSources installs a different implementation, once, at startup. Passing
// nil restores the in-memory default.
func SetNoteSources(s NoteSources) {
	noteSourcesMu.Lock()
	defer noteSourcesMu.Unlock()

	if s == nil {
		s = newNoteSourceTable()
	}
	noteSources = s
}

func currentNoteSources() NoteSources {
	noteSourcesMu.RLock()
	defer noteSourcesMu.RUnlock()
	return noteSources
}

// rememberNoteSource records where one note's token came from.
func rememberNoteSource(ctx context.Context, feedID, source, referrer string) {
	currentNoteSources().Remember(ctx, feedID, source, referrer)
}

// rememberFeedSources records a whole listing at once.
func rememberFeedSources(ctx context.Context, feeds []Feed, source, referrer string) {
	ns := currentNoteSources()
	for _, f := range feeds {
		ns.Remember(ctx, f.ID, source, referrer)
	}
}

func (t *noteSourceTable) Remember(_ context.Context, feedID, source, referrer string) {
	if feedID == "" || source == "" {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if len(t.entries) >= noteSourceCapacity {
		t.evictLocked()
	}
	t.entries[feedID] = noteEntryPoint{source: source, referrer: referrer, at: t.now()}
}

func (t *noteSourceTable) Lookup(_ context.Context, feedID string) (string, string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	e, ok := t.entries[feedID]
	if !ok {
		return "", "", false
	}
	if t.now().Sub(e.at) > NoteSourceTTL {
		delete(t.entries, feedID)
		return "", "", false
	}
	return e.source, e.referrer, true
}

// evictLocked drops expired entries first, then the oldest one if still full.
func (t *noteSourceTable) evictLocked() {
	now := t.now()
	for id, e := range t.entries {
		if now.Sub(e.at) > NoteSourceTTL {
			delete(t.entries, id)
		}
	}
	if len(t.entries) < noteSourceCapacity {
		return
	}

	var oldestID string
	var oldestAt time.Time
	for id, e := range t.entries {
		if oldestID == "" || e.at.Before(oldestAt) {
			oldestID, oldestAt = id, e.at
		}
	}
	delete(t.entries, oldestID)
}

// feedEntryPoint returns the xsec_source and referrer to present when opening a
// note.
//
// With no record it falls back to the feed: that is the commonest entry point,
// and it is the value that used to be hard-coded for every note, so "we don't
// know" is at least no worse than the old behaviour.
//
// It takes a context because a store-backed NoteSources issues a query here;
// every caller already has one.
func feedEntryPoint(ctx context.Context, feedID string) (source, referrer string) {
	if s, r, ok := currentNoteSources().Lookup(ctx, feedID); ok {
		return s, r
	}
	return xsecSourceFeed, urlExplore
}
