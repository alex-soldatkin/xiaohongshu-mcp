package xiaohongshu

import (
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
	// A token older than this is not worth claiming provenance for: the search
	// page it came from is long gone, and a stale claim is worse than the
	// honest default.
	noteSourceTTL = 30 * time.Minute
	// Bound on remembered notes. One search or profile page yields a few dozen,
	// so this holds roughly the last ten pages an agent looked at.
	noteSourceCapacity = 512
)

// noteSourceTable remembers, per note id, which page handed out its token.
//
// This is the "session context" the xsec_source is inferred from. It is
// deliberately not an MCP tool argument: an agent asked to supply a source
// would have to guess, and a guess that contradicts the Referer is worse than
// no claim at all. Here the two are derived from the same record, so they
// cannot disagree.
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

// noteSources is process-wide because the entry point outlives any single
// action object: the search that produced a token and the comment that uses it
// are separate tool calls with separate actions.
var noteSources = newNoteSourceTable()

func (t *noteSourceTable) remember(feedID, source, referrer string) {
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

func (t *noteSourceTable) rememberFeeds(feeds []Feed, source, referrer string) {
	for _, f := range feeds {
		t.remember(f.ID, source, referrer)
	}
}

func (t *noteSourceTable) lookup(feedID string) (noteEntryPoint, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	e, ok := t.entries[feedID]
	if !ok {
		return noteEntryPoint{}, false
	}
	if t.now().Sub(e.at) > noteSourceTTL {
		delete(t.entries, feedID)
		return noteEntryPoint{}, false
	}
	return e, true
}

// evictLocked drops expired entries first, then the oldest one if still full.
func (t *noteSourceTable) evictLocked() {
	now := t.now()
	for id, e := range t.entries {
		if now.Sub(e.at) > noteSourceTTL {
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
func feedEntryPoint(feedID string) (source, referrer string) {
	if e, ok := noteSources.lookup(feedID); ok {
		return e.source, e.referrer
	}
	return xsecSourceFeed, urlExplore
}
