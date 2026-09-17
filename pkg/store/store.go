// Package store is the persistence seam for issue #7.
//
// The design rule is that the store is dumb. It holds two kinds of thing:
// a document cache (one row per fetched payload, keyed by account/kind/key)
// and an append-only history log (notifications and comments, read back by an
// opaque cursor). It does not know what a note is, it does not know what a TTL
// is, and it never unmarshals a payload. Freshness is decided in Go by the
// caller from Doc.FetchedAt; that is what keeps a Postgres backend and a
// Mongo backend equally implementable behind this interface.
//
// Consequently the store sees only json.RawMessage. Typed marshalling is the
// caller's job, and this package must never import the xiaohongshu package.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Kind names a family of cached documents. It is the middle component of a
// document's primary key, so two kinds never collide even on the same key.
type Kind string

const (
	// KindNote is a feed detail fetched without loading all comments.
	KindNote Kind = "note"
	// KindNoteFull is a feed detail fetched with the full comment list. A
	// note_full document also satisfies a KindNote request; that rule lives
	// in the cache layer, not here.
	KindNoteFull Kind = "note_full"
	// KindProfile is another user's profile, keyed by "<user_id>:<tab>".
	KindProfile Kind = "profile"
	// KindMyProfile is the logged-in account's own profile, keyed by tab.
	KindMyProfile Kind = "my_profile"
	// KindFeed is the home feed listing.
	KindFeed Kind = "feed"
	// KindSearch is a search result page, keyed by a hash of the query.
	KindSearch Kind = "search"
	// KindNotifications is a notification listing, keyed by "<tab>:<limit>".
	KindNotifications Kind = "notifications"
	// KindUnread is the unread counter.
	KindUnread Kind = "unread"
)

// ErrNotFound is returned by every single-row read when the row is absent.
// Callers must test it with errors.Is; implementations may wrap it.
var ErrNotFound = errors.New("store: not found")

// Doc is one cached payload.
//
// Payload is exactly what the tool would have returned, as JSON. Meta is
// out-of-band information the cache layer needs in order to decide whether a
// hit is usable at all — for example the comment limit a note_full was fetched
// with, so a document fetched with MaxCommentItems=20 does not satisfy a
// request for 100. Meta is read in Go and never queried.
//
// FetchedAt is when the payload was obtained from the site, not when it was
// written to the store. It is the only input to TTL evaluation.
type Doc struct {
	Payload   json.RawMessage
	Meta      json.RawMessage
	FetchedAt time.Time
}

// Cursor is an opaque position in a history stream.
//
// It is deliberately a string and not an integer: Postgres numbers rows with
// bigserial, Mongo would use an ObjectID, and the caller must be able to hand
// the value back verbatim without either backend leaking its row-id type into
// the interface or into the MCP tool schema.
//
// Implementations must guarantee that, within one stream, cursors are unique,
// stable for the life of a row, and ordered lexicographically by byte value in
// the same order the rows are returned. The last property is what lets a
// caller store a cursor and compare it later; it costs a zero-padded
// formatting in a bigserial backend and comes free with ObjectIDs.
type Cursor string

// NotificationRecord is one notification as it enters the history log.
//
// Identity is (account, Tab, NotificationID). Tab is part of the key because
// notification-id uniqueness across tabs is unconfirmed on the site side, and
// merging two tabs on an unverified assumption would lose rows silently.
//
// FromUserID, NoteID and CommentID are typed only because a query filters on
// them; everything else stays inside Payload.
type NotificationRecord struct {
	Tab            string
	NotificationID string
	FromUserID     string
	NoteID         string
	CommentID      string
	Payload        json.RawMessage
}

// CommentRecord is one comment as it enters the history log. A reply is stored
// as its own row with ParentID set, rather than nested inside its parent's
// payload, so that "new since cursor" is a single ordered scan. The comment
// tree is never reconstructed from these rows; the cached document serves that.
//
// Identity is (account, NoteID, CommentID).
type CommentRecord struct {
	NoteID    string
	CommentID string
	ParentID  string
	AuthorID  string
	Payload   json.RawMessage
}

// HistoryItem is one row read back out of a history stream.
//
// FirstSeen is when the row was first appended, not when the site claims the
// item was created. The site's own timestamps are int64 of an unverified unit
// and nothing orders by them.
type HistoryItem struct {
	Cursor    Cursor
	Payload   json.RawMessage
	FirstSeen time.Time
}

// Account is a Xiaohongshu account this deployment has driven.
//
// AccountID is the XHS user id, not the fingerprint seed: the seed survives a
// logout and re-login, so scoping data by it would merge two accounts' history.
// Seed is recorded anyway, because it is the only identifier available at
// startup before any page has been read, and AccountBySeed uses it to recover
// the last account without costing a browser trip.
type Account struct {
	AccountID string
	Seed      int
	Nickname  string
	FirstSeen time.Time
	LastSeen  time.Time
}

// Store is the persistence contract. Every implementation must pass
// storetest.Run.
//
// General rules, assumed by every caller:
//   - All methods are safe for concurrent use.
//   - Reads that find nothing return ErrNotFound; listings that find nothing
//     return an empty slice and a nil error.
//   - Writes are idempotent at the level of their key.
//   - Nothing here enforces TTL or retention on its own. PruneDocs is the only
//     deletion by age and the caller decides when to run it.
type Store interface {
	// GetDoc returns the cached document, or ErrNotFound. It never judges
	// freshness; a document a year old comes back exactly like a fresh one.
	GetDoc(ctx context.Context, account string, kind Kind, key string) (Doc, error)

	// PutDoc inserts or replaces the document at (account, kind, key). A nil
	// or empty Meta is normalised to the JSON object "{}" on read.
	//
	// Payload must be valid JSON. Behaviour with invalid JSON is undefined:
	// an in-memory store will happily keep the bytes and a JSONB column will
	// reject them.
	PutDoc(ctx context.Context, account string, kind Kind, key string, doc Doc) error

	// DeleteDocs removes documents for one account and kind. With no keys it
	// removes every document of that kind for that account. Deleting an
	// absent key is not an error.
	DeleteDocs(ctx context.Context, account string, kind Kind, keys ...string) error

	// AppendNotifications appends items that are not already present and
	// returns how many rows were genuinely new. Items already in the log are
	// left untouched — in particular their cursor and FirstSeen do not move,
	// which is what makes a stored cursor meaningful across re-fetches.
	// Duplicates inside one batch count once.
	AppendNotifications(ctx context.Context, account string, items []NotificationRecord) (int, error)

	// NotificationsSince returns rows for one account and tab that sort
	// strictly after the given cursor, oldest first. An empty cursor starts at
	// the beginning of the stream. A limit of zero or less means no limit.
	NotificationsSince(ctx context.Context, account, tab string, after Cursor, limit int) ([]HistoryItem, error)

	// AppendComments is AppendNotifications for the comment log; identity is
	// (account, note id, comment id).
	AppendComments(ctx context.Context, account string, items []CommentRecord) (int, error)

	// CommentsSince is NotificationsSince for the comment log, scoped to one
	// note instead of one tab.
	CommentsSince(ctx context.Context, account, noteID string, after Cursor, limit int) ([]HistoryItem, error)

	// UpsertAccount records an account. A zero FirstSeen or LastSeen is filled
	// with the store's current time. Re-upserting an existing AccountID
	// updates seed, nickname and LastSeen but preserves the original
	// FirstSeen.
	UpsertAccount(ctx context.Context, a Account) error

	// AccountBySeed returns the most recently seen account last observed under
	// that fingerprint seed, or ErrNotFound. An account that has since moved
	// to a different seed is no longer returned for the old one.
	AccountBySeed(ctx context.Context, seed int) (Account, error)

	// PruneDocs deletes documents, across all accounts, whose FetchedAt is
	// older than olderThan ago, and returns how many went. It never touches
	// history rows: the log is the thing that cannot be re-fetched. An
	// olderThan of zero or less is a no-op returning 0, so that a missing or
	// misparsed retention setting cannot wipe the cache.
	PruneDocs(ctx context.Context, olderThan time.Duration) (int64, error)

	// Close releases resources. It is safe to call more than once. Use of the
	// store after Close is undefined.
	Close() error
}

// NoteSource is the provenance of one note: which surface handed out its
// xsec_token, and the referrer that surface would have sent.
//
// SeenAt is when the token was handed to us. A zero SeenAt on the way in is
// filled from the store's clock, exactly as Doc.FetchedAt is.
type NoteSource struct {
	FeedID   string
	Source   string
	Referrer string
	SeenAt   time.Time
}

// NoteSourceStore is an optional capability, type-asserted by its consumer.
// It persists the provenance of a note (which listing it was opened from) so
// that a restart does not desynchronise provenance from the cached listing it
// came out of. A store that does not implement it keeps the in-memory table.
//
// Identity is (account, FeedID); remembering the same note again overwrites
// the record, because the newest surface is the one whose token we now hold.
type NoteSourceStore interface {
	RememberNoteSource(ctx context.Context, account string, src NoteSource) error

	// LookupNoteSource returns the provenance of one note, or ErrNotFound when
	// there is none or the record is older than maxAge. A maxAge of zero or
	// less applies no age filter. Age is measured against the store's clock,
	// the way PruneDocs measures retention; the caller states the window
	// rather than filtering afterwards, so a stale row never crosses the wire.
	LookupNoteSource(ctx context.Context, account, feedID string, maxAge time.Duration) (NoteSource, error)

	// PruneNoteSources deletes records, across all accounts, older than
	// olderThan ago, and returns how many went. Like PruneDocs, a zero or
	// negative duration is a no-op returning 0. Provenance is a cache, not a
	// log: a record past its window can never be used again, so nothing is
	// lost by removing it.
	PruneNoteSources(ctx context.Context, olderThan time.Duration) (int64, error)
}

// PacingStateStore is an optional capability, type-asserted by its consumer.
// It holds one opaque snapshot of the rate-limit counters per key — the key is
// device-scoped ("profile:<seed>"), because at startup no account id is known
// yet and the budget belongs to the device profile anyway.
//
// The snapshot is a single blob, not an events table: nothing queries inside
// it. LoadPacingState returns ErrNotFound when there is no snapshot.
type PacingStateStore interface {
	LoadPacingState(ctx context.Context, key string) (json.RawMessage, error)
	SavePacingState(ctx context.Context, key string, snapshot json.RawMessage) error
}
